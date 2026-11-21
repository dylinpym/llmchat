package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dylin/llmchat/internal/domain"
)

// OpenAIConfig configures an OpenAI-compatible endpoint.
//
// Almost every domestic gateway (school portals, SiliconFlow, DashScope
// compatible mode, vLLM, Ollama) speaks this wire format, so only BaseURL and
// Model change between them.
type OpenAIConfig struct {
	BaseURL string // e.g. https://gateway.example.edu/v1  (no trailing slash)
	APIKey  string
	Model   string
	Timeout time.Duration // dial/TLS bound; must NOT cap the whole stream
}

// OpenAI is an OpenAI-compatible chat completions client.
type OpenAI struct {
	cfg  OpenAIConfig
	http *http.Client
}

func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &OpenAI{
		cfg: cfg,
		// Timeout deliberately covers only connection setup, not the response
		// body: a client-wide http.Client.Timeout would kill a healthy 3-minute
		// stream. Overall stream duration is governed by ctx instead.
		http: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ResponseHeaderTimeout: cfg.Timeout,
			},
		},
	}
}

func (o *OpenAI) Name() string { return "openai-compatible" }

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

// wireToolCall is the OpenAI wire shape: name and arguments are nested under
// "function", not at the top level. domain.ToolCall is deliberately flat
// because the rest of the codebase only cares whether a tool call exists, and
// flattening at the boundary keeps that shape out of the domain.
//
// Decoding the wire shape straight into domain.ToolCall does not error: it just
// leaves Name and Arguments empty, so tool calls silently vanish from history
// and the assistant.tool_calls sent back on the next turn are invalid — which
// is the exact 400 that contextpack exists to avoid.
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (w wireToolCall) toDomain() domain.ToolCall {
	return domain.ToolCall{
		ID:        w.ID,
		Type:      w.Type,
		Name:      w.Function.Name,
		Arguments: w.Function.Arguments,
	}
}

func toWireToolCalls(in []domain.ToolCall) []wireToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]wireToolCall, len(in))
	for i, tc := range in {
		out[i] = wireToolCall{ID: tc.ID, Type: tc.Type}
		if out[i].Type == "" {
			out[i].Type = "function"
		}
		out[i].Function.Name = tc.Name
		out[i].Function.Arguments = tc.Arguments
	}
	return out
}

func toDomainToolCalls(in []wireToolCall) []domain.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.ToolCall, len(in))
	for i, w := range in {
		out[i] = w.toDomain()
	}
	return out
}

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			Role      string         `json:"role"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message *chatMessage `json:"message"`
		Finish  string       `json:"finish_reason"`
	} `json:"choices"`
	// Usage arrives either on the final chunk with empty choices (OpenAI with
	// include_usage) or on the last chunk that also carries finish_reason. The
	// empty-choices case is why the content loop below guards on len(Choices).
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (o *OpenAI) build(req Request) ([]byte, error) {
	body := chatRequest{
		Model:       o.pickModel(req.Model),
		Stream:      req.Stream,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if req.Stream {
		// Without this flag most compatible gateways omit usage on streamed
		// responses, which silently zeroes out token accounting and billing.
		body.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, chatMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.ToolName,
			ToolCalls:  toWireToolCalls(m.ToolCalls),
		})
	}
	return json.Marshal(body)
}

func (o *OpenAI) pickModel(m string) string {
	if m != "" {
		return m
	}
	return o.cfg.Model
}

func (o *OpenAI) newRequest(ctx context.Context, payload []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(o.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if o.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	return httpReq, nil
}

// Stream consumes the SSE stream and forwards deltas.
//
// Cancellation contract: ctx cancellation closes the response body, which
// unblocks the scanner below, so the goroutine exits and generation stops. This
// is the mechanism that keeps a disconnected client from burning quota.
func (o *OpenAI) Stream(ctx context.Context, req Request) (<-chan Chunk, error) {
	req.Stream = true
	payload, err := o.build(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := o.newRequest(ctx, payload)
	if err != nil {
		return nil, err
	}

	resp, err := o.http.Do(httpReq)
	if err != nil {
		return nil, &ErrUpstream{Message: "request failed", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &ErrUpstream{Status: resp.StatusCode, Message: string(raw)}
	}

	out := make(chan Chunk, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close() // unblocks the scanner on every exit path

		sc := bufio.NewScanner(resp.Body)
		// Long tool-call payloads and CJK content routinely exceed the 64 KiB
		// default line limit; without this, large chunks error the stream.
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

		emit := func(c Chunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				emit(Chunk{Done: true})
				return
			}
			var cr chatResponse
			if err := json.Unmarshal(data, &cr); err != nil {
				continue // keep-alive or gateway-specific frames
			}
			if cr.Error != nil {
				emit(Chunk{Done: true})
				return
			}
			if len(cr.Choices) > 0 {
				d := cr.Choices[0].Delta
				tcs := toDomainToolCalls(d.ToolCalls)
				if d.Content != "" || len(tcs) > 0 {
					if !emit(Chunk{Content: d.Content, ToolCalls: tcs}) {
						return
					}
				}
			}
			if cr.Usage != nil {
				if !emit(Chunk{Usage: cr.Usage}) {
					return
				}
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			emit(Chunk{Done: true})
		}
	}()

	return out, nil
}

// Complete performs a single non-streaming call and returns text only.
//
// It intentionally drops finish_reason and any tool_calls: its only caller is
// the history summariser, which sends no tools and wants prose. Adding tool
// support here would mean a second return path with no consumer.
func (o *OpenAI) Complete(ctx context.Context, req Request) (string, Usage, error) {
	req.Stream = false
	payload, err := o.build(req)
	if err != nil {
		return "", Usage{}, err
	}
	httpReq, err := o.newRequest(ctx, payload)
	if err != nil {
		return "", Usage{}, err
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := o.http.Do(httpReq)
	if err != nil {
		return "", Usage{}, &ErrUpstream{Message: "request failed", Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", Usage{}, &ErrUpstream{Message: "read failed", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, &ErrUpstream{Status: resp.StatusCode, Message: string(raw)}
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", Usage{}, fmt.Errorf("provider: decode response: %w", err)
	}
	if cr.Error != nil {
		return "", Usage{}, &ErrUpstream{Message: cr.Error.Message}
	}
	var text string
	if len(cr.Choices) > 0 && cr.Choices[0].Message != nil {
		text = cr.Choices[0].Message.Content
	}
	u := Usage{}
	if cr.Usage != nil {
		u = *cr.Usage
	}
	return text, u, nil
}
