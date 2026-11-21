package provider

import (
	"context"
	"strings"
	"time"

	"github.com/dylin/llmchat/internal/domain"
)

// Mock is a deterministic provider used by tests and by `go run` without a key.
//
// It also lets tests reproduce conditions that are awkward to trigger against a
// real API: mid-generation client disconnect, oversized output, a provider that
// reports no usage, or slow streaming that races a timeout.
type Mock struct {
	// Reply is streamed word by word.
	Reply string
	// WordDelay simulates real generation latency.
	WordDelay time.Duration
	// UsageToReport, when nil, simulates a gateway that omits usage on streams.
	UsageToReport *Usage
	// FailWith makes the call return an error, to exercise the 502 path.
	FailWith error

	calls int
}

func NewMock() *Mock {
	return &Mock{
		Reply:         "This is a mock reply so the full pipeline can run without an API key.",
		WordDelay:     12 * time.Millisecond,
		UsageToReport: &Usage{PromptTokens: 42, CompletionTokens: 18, TotalTokens: 60},
	}
}

func (m *Mock) Name() string { return "mock" }

func (m *Mock) Calls() int { return m.calls }

func (m *Mock) Stream(ctx context.Context, req Request) (<-chan Chunk, error) {
	m.calls++
	if m.FailWith != nil {
		return nil, m.FailWith
	}
	words := strings.Fields(m.Reply)
	out := make(chan Chunk, len(words)+2)
	go func() {
		defer close(out)
		send := func(c Chunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for i, w := range words {
			if m.WordDelay > 0 {
				select {
				case <-time.After(m.WordDelay):
				case <-ctx.Done():
					return
				}
			}
			piece := w
			if i < len(words)-1 {
				piece += " "
			}
			if !send(Chunk{Content: piece}) {
				return
			}
		}
		if m.UsageToReport != nil {
			send(Chunk{Usage: m.UsageToReport})
		}
		send(Chunk{Done: true})
	}()
	return out, nil
}

func (m *Mock) Complete(ctx context.Context, req Request) (string, Usage, error) {
	m.calls++
	if m.FailWith != nil {
		return "", Usage{}, m.FailWith
	}
	u := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	if m.UsageToReport != nil {
		u = *m.UsageToReport
	}
	return m.Reply, u, nil
}

// ScriptedSummariser is a Summariser stub whose output length is predictable,
// so budget arithmetic can be asserted exactly.
type ScriptedSummariser struct {
	Text   string
	Tokens int
	Err    error
	// LastInput records what was summarised, for assertions.
	LastInput []domain.Message
}

func (s *ScriptedSummariser) Summarise(ms []domain.Message) (string, int, error) {
	s.LastInput = ms
	if s.Err != nil {
		return "", 0, s.Err
	}
	return s.Text, s.Tokens, nil
}
