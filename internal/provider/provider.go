// Package provider isolates the LLM backend. Everything above this package
// talks to an interface, which is what makes the chat logic testable without
// network access or spending API quota.
package provider

import (
	"context"

	"github.com/dylin/llmchat/internal/domain"
)

// Chunk is one streamed delta. Exactly one of Content, ToolCalls or Done is
// meaningful at a time; Usage arrives on the final chunk when the provider
// supports include_usage.
type Chunk struct {
	Content   string
	ToolCalls []domain.ToolCall
	Usage     *Usage
	Done      bool
}

// Usage is per-request token accounting, the basis for rate limiting and cost.
//
// The JSON tags are load-bearing, not cosmetic. encoding/json falls back to a
// case-insensitive field-name match, which does NOT bridge snake_case: a field
// named PromptTokens does not match "prompt_tokens", and an unmatched key is
// silently left at its zero value rather than reported as an error. Untagged,
// every number here parsed as 0 while still producing a non-nil *Usage, so
// token accounting reported free requests and billing/cost reporting looked
// healthy while being entirely fictional.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Request is a single completion call.
type Request struct {
	Model    string
	Messages []domain.Message
	// Stream is always true in this service; the non-streaming path exists for
	// the summariser, where incremental output has no value.
	Stream      bool
	Temperature float64
	MaxTokens   int
	// StopGenerationOnClientDisconnect is enforced via ctx cancellation rather
	// than a flag, but is recorded here so callers document their intent.
}

// Provider is a chat completion backend.
type Provider interface {
	// Stream returns chunks over ch and closes ch when finished. It must
	// return promptly when ctx is cancelled, even mid-generation: a client
	// that closed its tab must stop costing money.
	Stream(ctx context.Context, req Request) (<-chan Chunk, error)

	// Complete performs a single non-streaming call.
	Complete(ctx context.Context, req Request) (string, Usage, error)

	// Name identifies the implementation in metrics and /healthz.
	Name() string
}

// ErrUpstream signals a failure originating from the model backend, as opposed
// to a client or storage error. Handlers map it to 502 so upstream trouble is
// distinguishable from our own bugs in logs and alerts.
type ErrUpstream struct {
	Status  int
	Message string
	Err     error
}

func (e *ErrUpstream) Error() string { return "provider: upstream error: " + e.Message }
func (e *ErrUpstream) Unwrap() error { return e.Err }
