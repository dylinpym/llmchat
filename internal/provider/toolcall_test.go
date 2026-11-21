package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dylin/llmchat/internal/domain"
)

// TestToolCallWireRoundTrip locks the shape translation between the OpenAI wire
// format and the flat domain representation.
//
// The bug this guards: domain.ToolCall keeps name/arguments flat, but every
// OpenAI-compatible gateway (verified against chat.ecnu.edu.cn) nests them under
// "function". Decoding the wire shape straight into the flat struct does not
// return an error — it leaves Name and Arguments as "", so tool calls silently
// become anonymous. On the next turn the assistant.tool_calls sent upstream are
// then invalid and the provider rejects the whole request with a 400, which is
// the precise failure contextpack's exchange alignment exists to prevent.
//
// Asserting non-empty Name is the point; asserting only that unmarshalling
// succeeds would pass on the broken code.
func TestToolCallWireRoundTrip(t *testing.T) {
	// Exactly what the gateway sends.
	const wire = `[{"id":"call_65a7","type":"function","function":{"name":"multiply","arguments":"{\"a\":24,\"b\":7}"}}]`

	var got []wireToolCall
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("unmarshal wire shape: %v", err)
	}
	domainCalls := toDomainToolCalls(got)
	if len(domainCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(domainCalls))
	}
	tc := domainCalls[0]
	if tc.ID != "call_65a7" {
		t.Errorf("ID = %q, want call_65a7", tc.ID)
	}
	if tc.Name != "multiply" {
		t.Errorf("Name = %q, want multiply (empty means the nesting was lost)", tc.Name)
	}
	if tc.Arguments != `{"a":24,"b":7}` {
		t.Errorf("Arguments = %q, want {\"a\":24,\"b\":7}", tc.Arguments)
	}

	// Round trip back out must restore the nesting, or the next request is invalid.
	out := toWireToolCalls(domainCalls)
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(encoded)
	for _, want := range []string{`"function"`, `"multiply"`, `"call_65a7"`} {
		if !strings.Contains(s, want) {
			t.Errorf("re-encoded tool call missing %s: %s", want, s)
		}
	}
	// A flat re-encode would put "name" at top level, which upstream rejects.
	if strings.Contains(s, `{"id":"call_65a7","type":"function","name"`) {
		t.Errorf("tool calls re-encoded flat instead of nested under function: %s", s)
	}
}

// TestEmptyToolCallsMarshalToOmitted checks that a message with no tool calls
// emits no tool_calls key at all. Sending "tool_calls":[] on an ordinary
// assistant turn is rejected by strict OpenAI-compatible endpoints.
func TestEmptyToolCallsMarshalToOmitted(t *testing.T) {
	if got := toWireToolCalls(nil); got != nil {
		t.Fatalf("nil input -> %v, want nil so the field is omitted", got)
	}
	if got := toWireToolCalls([]domain.ToolCall{}); got != nil {
		t.Fatalf("empty slice -> %v, want nil so the field is omitted", got)
	}
	if got := toDomainToolCalls(nil); got != nil {
		t.Fatalf("nil wire -> %v, want nil", got)
	}
}

// TestWireToolCallDefaultsType covers gateways that omit the "type" field on
// tool calls in a delta; replaying them without type fails validation.
func TestWireToolCallDefaultsType(t *testing.T) {
	out := toWireToolCalls([]domain.ToolCall{{ID: "c1", Name: "lookup", Arguments: "{}"}})
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].Type != "function" {
		t.Errorf("Type = %q, want the function default", out[0].Type)
	}
}
