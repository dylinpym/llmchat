package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseBody is the exact ECNU gateway frame order: role frame, content frames,
// a finish_reason frame with an empty delta, then a frame whose choices array
// is EMPTY and which carries usage only, then the [DONE] sentinel.
//
// Written as one backtick literal on purpose. An earlier version of this test
// assembled each line by concatenating "data: {" + payload + "}", which added a
// brace the payload already closed. Every frame then failed to unmarshal, the
// provider skipped all of them and emitted only Done, and the test "proved" the
// provider dropped usage. It took printing the real json error to see it. Fixtures
// must be validated, not eyeballed — hence TestSSEFixtureIsValidJSON below.
const sseBody = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":14,\"completion_tokens\":2,\"total_tokens\":16}}\n\n" +
	"data: [DONE]\n\n"

// TestSSEFixtureIsValidJSON guards the fixture itself. Cheap insurance against a
// malformed literal silently testing nothing.
func TestSSEFixtureIsValidJSON(t *testing.T) {
	frames := 0
	for _, line := range strings.Split(sseBody, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatalf("fixture frame %d is not valid JSON: %v\n%s", frames, err, data)
		}
		frames++
	}
	if frames != 4 {
		t.Fatalf("fixture has %d JSON frames, want 4", frames)
	}
}

// TestStreamParsesUsageFromEmptyChoicesChunk is the regression test for usage
// accounting. The usage frame carries an empty choices array, so the content
// path must not run, and Usage must still reach the consumer with real values.
//
// This matters because Usage is persisted as each assistant message's
// TokenCount, which the context packer later trusts. If usage silently reads
// zero, cost reporting looks healthy while being fictional, and the packer
// falls back to estimates without anyone noticing.
func TestStreamParsesUsageFromEmptyChoicesChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	p := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second})
	ch, err := p.Stream(context.Background(), Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}

	var (
		gotUsage *Usage
		text     strings.Builder
		sawDone  bool
	)
	for c := range ch {
		if c.Content != "" {
			text.WriteString(c.Content)
		}
		if c.Usage != nil {
			gotUsage = c.Usage
		}
		if c.Done {
			sawDone = true
		}
	}

	if text.String() != "OK" {
		t.Errorf("streamed content = %q, want %q", text.String(), "OK")
	}
	if !sawDone {
		t.Error("no Done chunk emitted")
	}
	if gotUsage == nil {
		t.Fatal("usage chunk never reached the consumer")
	}
	if gotUsage.PromptTokens != 14 || gotUsage.CompletionTokens != 2 || gotUsage.TotalTokens != 16 {
		t.Fatalf("usage = %+v, want 14/2/16", *gotUsage)
	}
}
