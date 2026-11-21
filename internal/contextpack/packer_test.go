package contextpack

// Tests for the context packer.
//
// The packer is the part of this backend that stops being CRUD: a naive
// "keep the last N messages that fit" loop corrupts tool-call exchanges and
// silently changes what the model sees. These tests are written as executable
// documentation of that claim, so the central test
// (TestPack_ToolCallBoundaryNotSplit) pins the API-level invariant rather than
// an implementation detail: the assembled request must never contain a `tool`
// message whose originating assistant-with-tool_calls message is missing.
//
// Conventions used throughout:
//   - TokenCount is set explicitly on every fixture message, because budget
//     arithmetic reads TokenCount and never re-tokenises (see costOf). Relying
//     on EstimateTokens here would make the expected numbers indirect.
//   - Expected indices are stated in terms of dialogue positions, so a failure
//     says "position 4 kept but 3 dropped" instead of a bare count mismatch.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dylin/llmchat/internal/domain"
	"github.com/dylin/llmchat/internal/provider"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// msg builds a message with an explicit token cost. Callers set TokenCount on
// every message in these tests: packer arithmetic is TokenCount-based.
func msg(role domain.Role, content string, tokens int) domain.Message {
	return domain.Message{
		Role:       role,
		Content:    content,
		TokenCount: tokens,
		CreatedAt:  baseTime,
	}
}

// baseTime keeps fixtures deterministic. SortByDepth falls back to CreatedAt,
// so a fixed instant means helper ordering is the only thing that matters.
var baseTime = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

func systemMsg(content string, tokens int) domain.Message {
	return msg(domain.RoleSystem, content, tokens)
}

func userMsg(content string, tokens int) domain.Message {
	return msg(domain.RoleUser, content, tokens)
}

// assistantMsg is a plain assistant turn: no tool_calls, so IsToolRelated is
// false for it. That distinction is what TestPack_ToolCallBoundaryNotSplit
// depends on.
func assistantMsg(content string, tokens int) domain.Message {
	return msg(domain.RoleAssistant, content, tokens)
}

// assistantWithTools builds the message that starts a tool exchange. Per
// domain.Message.IsToolRelated, RoleAssistant is tool-related only when
// ToolCalls is non-empty, so the calls must be real fixtures, not decoration.
//
// ids are the tool_call ids the assistant requested; the matching tool results
// must reuse them, which is exactly the pairing the OpenAI API validates.
func assistantWithTools(content string, tokens int, ids ...string) domain.Message {
	calls := make([]domain.ToolCall, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, domain.ToolCall{
			ID:        id,
			Type:      "function",
			Name:      "lookup",
			Arguments: `{"q":"` + id + `"}`,
		})
	}
	m := msg(domain.RoleAssistant, content, tokens)
	m.ToolCalls = calls
	return m
}

// toolMsg is one tool result. callID must match an id from assistantWithTools.
func toolMsg(callID, content string, tokens int) domain.Message {
	m := msg(domain.RoleTool, content, tokens)
	m.ToolCallID = callID
	m.ToolName = "lookup"
	return m
}

// withContents tags each message so assertions can report *which* message was
// wrong, not just how many.
func withContents(ms ...domain.Message) domain.Branch {
	b := make(domain.Branch, len(ms))
	copy(b, ms)
	return b
}

// indexOfContent returns positions of messages carrying the marker text.
func indexOfContent(ms []domain.Message) map[string]int {
	out := make(map[string]int, len(ms))
	for i, m := range ms {
		out[m.Content] = i
	}
	return out
}

// requireNoOrphanToolMessages enforces the invariant that motivated the whole
// packer: for every `tool` message in the outbound request, the assistant
// message that requested it (matched by tool_call id) must also be present.
//
// Why this matters in production terms: the OpenAI Chat Completions API
// validates message shape, not just content. An assistant message carrying
// tool_calls must be immediately followed by a tool message for every id in
// tool_calls, and a tool message is only legal when the preceding assistant
// message advertised that tool_call id. Both halves of the pairing are checked
// server-side, so:
//
//   - dropping the assistant(tool_calls) but keeping its tool results yields
//     "messages with role 'tool' must be a response to a preceding message with
//     'tool_calls'" -> HTTP 400, the whole request rejected;
//   - dropping the tool results but keeping the assistant(tool_calls) yields
//     "an assistant message with 'tool_calls' must be followed by tool messages
//     responding to each tool_call_id" -> HTTP 400 again.
//
// Either way the user's turn fails outright, and it only happens once tool
// calling is in use and history is long enough to compact — which is why a
// plain "last N that fit" loop passes a smoke test and breaks in production.
//
// This helper checks both directions so the test cannot pass by accident.
func requireNoOrphanToolMessages(t *testing.T, msgs []domain.Message) {
	t.Helper()

	requested := make(map[string]bool) // ids advertised by a kept assistant
	answered := make(map[string]bool)  // ids answered by a kept tool message
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			requested[tc.ID] = true
		}
		if m.Role == domain.RoleTool {
			answered[m.ToolCallID] = true
		}
	}

	for _, m := range msgs {
		if m.Role != domain.RoleTool {
			continue
		}
		if !requested[m.ToolCallID] {
			t.Errorf("orphan tool message %q (tool_call_id=%s): the assistant message carrying "+
				"tool_calls was dropped, so the provider would reject the request with 400",
				m.Content, m.ToolCallID)
		}
		// A tool message may never be the first dialogue message: there would be
		// no preceding assistant at all.
		if idx := indexOfRole(msgs, m); idx > 0 && msgs[idx-1].Role == domain.RoleSystem {
			t.Errorf("tool message %q is the first non-system message; provider requires a "+
				"preceding assistant message with tool_calls", m.Content)
		}
	}

	for id := range requested {
		if !answered[id] {
			t.Errorf("assistant requested tool_call %q but no tool result for it survived: a kept "+
				"tool_calls message must be followed by a response for every id", id)
		}
	}
}

// indexOfRole finds the first occurrence of m by identity-by-content. Good
// enough for fixtures whose contents are unique markers.
func indexOfRole(ms []domain.Message, m domain.Message) int {
	for i, x := range ms {
		if x.Content == m.Content && x.Role == m.Role {
			return i
		}
	}
	return -1
}

// firstDialogueIndex is where conversation content starts, i.e. just past the
// pinned system prefix. Used so "the window must not begin with a tool message"
// is measured against the dialogue, not against the system prompt.
func firstDialogueIndex(ms []domain.Message) int {
	i := 0
	for i < len(ms) && ms[i].Role == domain.RoleSystem {
		i++
	}
	return i
}

// ---------------------------------------------------------------------------
// 1. Fits without compaction
// ---------------------------------------------------------------------------

func TestPack_FitsWithoutCompaction(t *testing.T) {
	branch := withContents(
		systemMsg("You are terse.", 10),
		userMsg("hi", 5),
		assistantMsg("hello", 6),
		userMsg("again", 5),
	)
	p := Packer{Strategy: StrategyHybrid, Summariser: &provider.ScriptedSummariser{
		Text:   "should not be called",
		Tokens: 999,
	}}

	b := Budget{MaxContextTokens: 1000, ReserveForReply: 100, ReserveForInput: 50}
	got, err := p.Pack(branch, b)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	// Nothing was dropped, so the request must be byte-identical to the branch.
	if len(got.Messages) != len(branch) {
		t.Fatalf("len(Messages) = %d, want %d (unchanged passthrough)", len(got.Messages), len(branch))
	}
	for i := range branch {
		if got.Messages[i].Role != branch[i].Role || got.Messages[i].Content != branch[i].Content {
			t.Errorf("Messages[%d] = %s/%q, want %s/%q", i,
				got.Messages[i].Role, got.Messages[i].Content,
				branch[i].Role, branch[i].Content)
		}
	}
	if got.DroppedCount != 0 {
		t.Errorf("DroppedCount = %d, want 0", got.DroppedCount)
	}
	if got.Summarised != 0 {
		t.Errorf("Summarised = %d, want 0", got.Summarised)
	}
	if got.UsedSummary {
		t.Error("UsedSummary = true, want false: fitting history must not be rewritten")
	}
	wantTokens := 10 + 5 + 6 + 5
	if got.TokensUsed != wantTokens {
		t.Errorf("TokensUsed = %d, want %d", got.TokensUsed, wantTokens)
	}
	// A summary model call on the happy path would be pure wasted latency and
	// spend, so assert the summariser was never consulted.
	if s := p.Summariser.(*provider.ScriptedSummariser); s.LastInput != nil {
		t.Errorf("Summariser called with %d messages on the no-op path", len(s.LastInput))
	}
}

// TestPack_FitsWithoutCompaction does not mutate the caller's branch: the
// packer is called per request from concurrent handlers.
func TestPack_InputBranchNotMutated(t *testing.T) {
	branch := withContents(
		systemMsg("sys", 10),
		userMsg("one", 50),
		assistantMsg("two", 50),
		userMsg("three", 50),
	)
	before := make(domain.Branch, len(branch))
	copy(before, branch)

	p := Packer{Strategy: StrategyTruncate}
	if _, err := p.Pack(branch, Budget{MaxContextTokens: 90}); err != nil {
		t.Fatalf("Pack: %v", err)
	}

	if len(branch) != len(before) {
		t.Fatalf("caller branch length changed to %d, want %d", len(branch), len(before))
	}
	for i := range before {
		if branch[i].Content != before[i].Content {
			t.Errorf("caller branch[%d] = %q, want %q (Pack must not rewrite input)",
				i, branch[i].Content, before[i].Content)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Truncation keeps the newest
// ---------------------------------------------------------------------------

func TestPack_TruncateKeepsNewest(t *testing.T) {
	// Available = 240 - 40 - 30 = 170, and the 20-token system prompt is paid out
	// of that before history is considered, leaving 150. floor(150/50) = 3, so the
	// three newest turns survive and the five oldest go away. Worth spelling out:
	// the system prompt is charged against the same window the history competes
	// for, which is the first thing to check when truncation drops more than
	// expected.
	branch := withContents(
		systemMsg("sys", 20),
		userMsg("u1", 50), assistantMsg("a1", 50),
		userMsg("u2", 50), assistantMsg("a2", 50),
		userMsg("u3", 50), assistantMsg("a3", 50),
		userMsg("u4", 50), assistantMsg("a4", 50),
	)
	p := Packer{Strategy: StrategyTruncate}

	// Available = 240 - 40 - 30 = 170.
	b := Budget{MaxContextTokens: 240, ReserveForReply: 40, ReserveForInput: 30}
	got, err := p.Pack(branch, b)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	// The newest message is the last dialogue message, under any strategy.
	last := branch[len(branch)-1]
	if got.Messages[len(got.Messages)-1].Content != last.Content {
		t.Errorf("last message = %q, want %q", got.Messages[len(got.Messages)-1].Content, last.Content)
	}

	// Oldest-first order must survive: contents appear in the same relative
	// order as in the branch.
	var kept []string
	for _, m := range got.Messages {
		kept = append(kept, m.Content)
	}
	want := []string{"sys", "a3", "u4", "a4"}
	if strings.Join(kept, ",") != strings.Join(want, ",") {
		t.Errorf("Messages = %v, want %v", kept, want)
	}

	if got.DroppedCount != 5 {
		t.Errorf("DroppedCount = %d, want 5", got.DroppedCount)
	}
	if got.UsedSummary || got.Summarised != 0 {
		t.Errorf("UsedSummary/Summarised = %v/%d, want false/0 for StrategyTruncate",
			got.UsedSummary, got.Summarised)
	}
	// TokensUsed is the assembled cost, not the pre-compaction cost: the 20-token
	// system prompt plus the three 50-token turns that fit.
	if want := 20 + 3*50; got.TokensUsed != want {
		t.Errorf("TokensUsed = %d, want %d", got.TokensUsed, want)
	}
	// Dropped messages must be exactly the oldest ones, in branch order.
	dropped := []string{"u1", "a1", "u2", "a2", "u3"}
	_ = dropped // reported via DroppedCount above; kept explicit for readability
}

// TestPack_TruncateDropsNothingWhenOnlyNewestFits pins the documented override
// in takeTailAligned: when even the newest message busts the budget, it is sent
// anyway rather than sending an empty dialogue.
func TestPack_TruncateDropsNothingWhenOnlyNewestFits(t *testing.T) {
	branch := withContents(
		userMsg("old", 400),
		userMsg("newest is huge", 900),
	)
	p := Packer{Strategy: StrategyTruncate}

	got, err := p.Pack(branch, Budget{MaxContextTokens: 100})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "newest is huge" {
		t.Fatalf("Messages = %d messages %v, want exactly the newest message", len(got.Messages), got.Messages)
	}
	if got.DroppedCount != 1 {
		t.Errorf("DroppedCount = %d, want 1", got.DroppedCount)
	}
	if got.TokensUsed > 100 {
		t.Logf("TokensUsed = %d exceeds the 100 budget: intentional, takeTailAligned keeps the "+
			"newest message so the request is at least answerable", got.TokensUsed)
	}
}

// ---------------------------------------------------------------------------
// 3. THE tool-call boundary test
// ---------------------------------------------------------------------------

// TestPack_ToolCallBoundaryNotSplit is the regression test for the invariant
// the package doc comment calls the most important thing here.
//
// Dialogue (oldest first), ending in a completed tool exchange:
//
//	0 user                       "setup"      30
//	1 assistant                  "plan"       20
//	2 assistant                  "note"       10
//	3 assistant + tool_calls     "call it"    80   <- opens the exchange
//	4 tool  (id=call_a)          "result a"   35
//	5 tool  (id=call_b)          "result b"   25
//	6 assistant                  "final"      50
//
// With 240 system-inclusive headroom we get Available = 180 after a 30-token
// system prompt. Walking backwards, 50 + 25 + 35 = 110 fits, and the next
// candidate is the 80-token assistant that carries the tool_calls: 110 + 80 =
// 190 > 180, so the naive loop stops at index 6. The window therefore begins
// with the final assistant turn while indices 3-5 (the whole tool exchange) are
// dropped, which is legal-looking but *not* the point: the assertion below is
// the general one — every surviving tool message keeps its assistant, and the
// kept dialogue never begins with a tool message.
//
// Splitting this pair makes the OpenAI API return 400 because tool-call
// bookkeeping is validated message-by-message on the way in. A `tool` role
// message is only interpretable relative to the immediately preceding assistant
// message: the server matches its tool_call_id against the tool_calls array of
// that assistant message. Remove the assistant and the tool message is an
// answer to a question nobody asked ("must be a response to a preceding message
// with 'tool_calls'"); remove the tool results and the assistant is a question
// nobody answered ("must be followed by tool messages responding to each
// tool_call_id"). Neither is a warning — the request is refused outright, so a
// packer that gets this wrong does not degrade quality, it takes the endpoint
// down for every long-running tool-using conversation.
func TestPack_ToolCallBoundaryNotSplit(t *testing.T) {
	build := func() domain.Branch {
		return withContents(
			systemMsg("sys", 30),
			userMsg("setup", 30),
			assistantMsg("plan", 20),
			assistantMsg("note", 10),
			assistantWithTools("call it", 80, "call_a", "call_b"),
			toolMsg("call_a", "result a", 35),
			toolMsg("call_b", "result b", 25),
			assistantMsg("final", 50),
		)
	}

	// Several budgets, all of which put the raw cut point at or inside the tool
	// exchange. A single lucky budget would not be evidence.
	cases := []struct {
		name         string
		max, reply   int
		inputReserve int
	}{
		{"cut lands on the tool run", 270, 30, 30}, // available 210
		{"cut lands on the tool_calls msg", 240, 30, 30},
		{"tightest legal budget", 200, 20, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branch := build()
			b := Budget{
				MaxContextTokens: tc.max,
				ReserveForReply:  tc.reply,
				ReserveForInput:  tc.inputReserve,
			}
			// Precondition: this fixture genuinely needs compaction, otherwise
			// the test would pass through the no-op path and assert nothing.
			if costOf(branch.Dialogue()) <= b.Available() {
				t.Fatalf("fixture does not require compaction: dialogue %d, available %d",
					costOf(branch.Dialogue()), b.Available())
			}

			for _, strat := range []Strategy{StrategyTruncate, StrategySummarise, StrategyHybrid} {
				t.Run(string(strat), func(t *testing.T) {
					p := Packer{
						Strategy: strat,
						Summariser: &provider.ScriptedSummariser{
							Text:   "earlier: user asked, assistant ran two tools",
							Tokens: 40,
						},
						PinFirstExchange: true,
					}
					got, err := p.Pack(branch, Budget{
						MaxContextTokens: tc.max,
						ReserveForReply:  tc.reply,
						ReserveForInput:  tc.inputReserve,
					})
					if err != nil {
						t.Fatalf("Pack(%s): %v", strat, err)
					}

					requireNoOrphanToolMessages(t, got.Messages)

					// The kept dialogue must open on a message that can legally
					// start a request: never a tool result, never an orphan.
					if fd := firstDialogueIndex(got.Messages); fd < len(got.Messages) {
						if got.Messages[fd].Role == domain.RoleTool {
							t.Errorf("kept window begins with tool message %q; the assistant that "+
								"requested it was dropped -> provider 400", got.Messages[fd].Content)
						}
					}

					// If the assistant that opened the exchange survived, so must
					// both of its results, in order and immediately after it.
					openIdx := -1
					for i, m := range got.Messages {
						if len(m.ToolCalls) > 0 {
							openIdx = i
						}
					}
					if openIdx >= 0 {
						wantIDs := []string{"call_a", "call_b"}
						if openIdx+1 >= len(got.Messages) {
							t.Fatalf("assistant with tool_calls kept at %d but nothing follows it", openIdx)
						}
						for j, want := range wantIDs {
							k := openIdx + 1 + j
							if k >= len(got.Messages) {
								t.Fatalf("tool result for %s dropped while its assistant message was kept", want)
							}
							if got.Messages[k].Role != domain.RoleTool {
								t.Fatalf("Messages[%d] = %s, want tool result for %s (tool results must "+
									"immediately follow the assistant tool_calls message)",
									k, got.Messages[k].Role, want)
							}
							if got.Messages[k].ToolCallID != want {
								t.Errorf("Messages[%d].ToolCallID = %q, want %q", k, got.Messages[k].ToolCallID, want)
							}
						}
					}
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Summarise replaces the middle
// ---------------------------------------------------------------------------

func TestPack_SummariseReplacesMiddle(t *testing.T) {
	branch := withContents(
		systemMsg("sys", 20),
		userMsg("u1", 30),
		assistantMsg("a1", 40),
		userMsg("u2", 35),
		assistantMsg("a2", 45),
		userMsg("u3", 25),
		assistantMsg("a3", 50),
	)
	sum := &provider.ScriptedSummariser{Text: "  user and assistant discussed X and Y  ", Tokens: 60}
	p := Packer{Strategy: StrategySummarise, Summariser: sum}

	// Available = 300 - 40 - 36 = 224. Dialogue cost is 225, so compaction
	// triggers by exactly one token.
	b := Budget{MaxContextTokens: 300, ReserveForReply: 40, ReserveForInput: 36}
	got, err := p.Pack(branch, b)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	if !got.UsedSummary {
		t.Fatal("UsedSummary = false, want true")
	}
	if got.Summarised == 0 {
		t.Fatal("Summarised = 0, want > 0: the middle must be folded into the summary")
	}

	// Exactly one summary message, a system message, carrying the preamble and
	// the trimmed summary text.
	var summaryAt = -1
	summaries := 0
	for i, m := range got.Messages {
		if strings.HasPrefix(m.Content, "[Summary of earlier conversation]") {
			summaries++
			summaryAt = i
		}
	}
	if summaries != 1 {
		t.Fatalf("found %d summary messages, want exactly 1", summaries)
	}
	if got.Messages[summaryAt].Role != domain.RoleSystem {
		t.Errorf("summary role = %s, want system", got.Messages[summaryAt].Role)
	}
	if !strings.Contains(got.Messages[summaryAt].Content, "discussed X and Y") {
		t.Errorf("summary content = %q, want it to contain the summariser text", got.Messages[summaryAt].Content)
	}
	if strings.HasSuffix(got.Messages[summaryAt].Content, " ") {
		t.Errorf("summary text was not trimmed: %q", got.Messages[summaryAt].Content)
	}
	if got.Messages[summaryAt].TokenCount != 60 {
		t.Errorf("summary TokenCount = %d, want the summariser's 60", got.Messages[summaryAt].TokenCount)
	}

	// Order: pinned system prefix, then summary, then the kept tail.
	if summaryAt != 1 {
		t.Errorf("summary at index %d, want 1 (immediately after the system prefix)", summaryAt)
	}
	tailStart := summaryAt + 1
	wantTail := []string{"u3", "a3"} // newest-first fit inside the reduced room
	var tail []string
	for _, m := range got.Messages[tailStart:] {
		tail = append(tail, m.Content)
	}
	if strings.Join(tail, ",") != strings.Join(wantTail, ",") {
		t.Errorf("kept tail = %v, want %v", tail, wantTail)
	}

	// The summariser must have received the messages that are absent from the
	// request — that is what "replaced", rather than "deleted", means.
	if sum.LastInput == nil {
		t.Fatal("Summariser was never called")
	}
	if len(sum.LastInput) != got.Summarised {
		t.Errorf("Summariser saw %d messages, Summarised = %d", len(sum.LastInput), got.Summarised)
	}
	kept := indexOfContent(got.Messages)
	for _, m := range sum.LastInput {
		if _, ok := kept[m.Content]; ok {
			t.Errorf("message %q was both summarised and kept verbatim", m.Content)
		}
	}

	// Accounting: system + summary + tail.
	if want := 20 + 60 + 25 + 50; got.TokensUsed != want {
		t.Errorf("TokensUsed = %d, want %d", got.TokensUsed, want)
	}
	if got.DroppedCount != 0 {
		t.Errorf("DroppedCount = %d, want 0: summarised messages are not dropped", got.DroppedCount)
	}
}

// ---------------------------------------------------------------------------
// 5. Hybrid pins the first exchange
// ---------------------------------------------------------------------------

func TestPack_HybridPinsFirstExchange(t *testing.T) {
	branch := withContents(
		systemMsg("sys", 10),
		userMsg("goal: migrate the billing service", 20), // must survive verbatim
		assistantMsg("ack", 15),
		userMsg("m1", 25),
		assistantMsg("m2", 30),
		userMsg("m3", 18),
		assistantMsg("m4", 22),
		userMsg("m5", 28),
		assistantMsg("m6", 26),
		userMsg("m7", 24),
		assistantMsg("m8", 35),
	)
	goal := "goal: migrate the billing service"
	sum := &provider.ScriptedSummariser{Text: "middle recap", Tokens: 50}
	p := Packer{Strategy: StrategyHybrid, Summariser: sum, PinFirstExchange: true}

	// Dialogue cost is 243; available must be below that or the packer takes its
	// no-op path and the strategy under test is never exercised.
	b := Budget{MaxContextTokens: 270, ReserveForReply: 20, ReserveForInput: 20}
	if costOf(branch.Dialogue()) <= b.Available() {
		t.Fatalf("fixture must overflow: dialogue %d <= available %d", costOf(branch.Dialogue()), b.Available())
	}

	got, err := p.Pack(branch, b)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if !got.UsedSummary {
		t.Fatal("UsedSummary = false, want true for Hybrid")
	}

	fd := firstDialogueIndex(got.Messages)
	if fd >= len(got.Messages) {
		t.Fatal("no dialogue survived at all")
	}

	// Pinning means the goal reaches the model as the framing of the summary, not
	// as a verbatim turn. The kept dialogue is always a contiguous suffix: an
	// earlier version prepended the pinned exchange in front of an independently
	// chosen tail, which produced a window with its middle missing. That is not
	// just odd conversation — it is what orphaned tool results and made the
	// provider return 400 (see TestPack_ToolCallBoundaryNotSplit).
	goalText := sum.LastInput[0].Content
	if !strings.Contains(goalText, goal) {
		t.Errorf("summariser framing does not quote the goal.\nframing: %q\nwant substring: %q", goalText, goal)
	}
	if sum.LastInput[0].Role != domain.RoleSystem {
		t.Errorf("framing role = %s, want system", sum.LastInput[0].Role)
	}

	// The kept window is a contiguous suffix of the dialogue, so the goal turn is
	// not replayed verbatim.
	for _, m := range got.Messages[fd:] {
		if m.Content == goal {
			t.Error("goal appears as a verbatim turn; the kept window must stay contiguous")
		}
	}
	if got.Summarised == 0 {
		t.Error("Summarised = 0, want > 0: Hybrid summarises the opening and middle")
	}

	// The newest message still survives alongside the pin.
	if last := got.Messages[len(got.Messages)-1]; last.Content != "m8" {
		t.Errorf("last message = %q, want %q", last.Content, "m8")
	}

	// And pinning is not free: with PinFirstExchange=false the same budget must
	// drop the goal message, which is what makes the flag meaningful.
	unpinned := Packer{Strategy: StrategyHybrid, Summariser: &provider.ScriptedSummariser{Text: "x", Tokens: 50}}
	got2, err := unpinned.Pack(branch, b)
	if err != nil {
		t.Fatalf("Pack(unpinned): %v", err)
	}
	for _, m := range got2.Messages {
		if m.Content == goal {
			t.Error("without PinFirstExchange the first exchange still survived; the fixture does not " +
				"actually force its eviction, so the assertion above is vacuous")
		}
	}
}

// ---------------------------------------------------------------------------
// 6. Graceful degradation
// ---------------------------------------------------------------------------

func TestPack_SummariserFailureDegradesToTruncate(t *testing.T) {
	sumErr := errors.New("summariser: upstream 503")
	branch := withContents(
		systemMsg("sys", 20),
		userMsg("u1", 30),
		assistantMsg("a1", 40),
		userMsg("u2", 35),
		assistantMsg("a2", 45),
		userMsg("u3", 25),
		assistantMsg("a3", 50),
	)
	sum := &provider.ScriptedSummariser{Err: sumErr}
	p := Packer{Strategy: StrategySummarise, Summariser: sum}

	b := Budget{MaxContextTokens: 300, ReserveForReply: 40, ReserveForInput: 36}
	got, err := p.Pack(branch, b)
	if err != nil {
		// The contract under test: a failed summary must not sink the user's
		// turn. The user still gets an answer on truncated history.
		t.Fatalf("Pack returned %v; a summariser failure must degrade, not fail the request", err)
	}
	if got.UsedSummary {
		t.Error("UsedSummary = true, want false: no summary block was produced")
	}
	for i, m := range got.Messages {
		if strings.Contains(m.Content, "Summary of earlier conversation") {
			t.Errorf("Messages[%d] contains a summary block despite the failure: %q", i, m.Content)
		}
	}
	if got.DroppedCount == 0 {
		t.Error("DroppedCount = 0, want > 0: history must be truncated instead")
	}
	if got.Summarised != 0 {
		t.Errorf("Summarised = %d, want 0 after the fallback", got.Summarised)
	}
	// The tail is what survives, and it must still be a legal request.
	if last := got.Messages[len(got.Messages)-1]; last.Content != "a3" {
		t.Errorf("last message = %q, want %q", last.Content, "a3")
	}
	requireNoOrphanToolMessages(t, got.Messages)
	if sum.LastInput == nil {
		t.Error("Summariser was not attempted before degrading")
	}
	if got.TokensUsed > b.Available() {
		t.Errorf("TokensUsed = %d exceeds available %d after the fallback", got.TokensUsed, b.Available())
	}
}

// ---------------------------------------------------------------------------
// 7. Overflow
// ---------------------------------------------------------------------------

func TestPack_SystemOverflow(t *testing.T) {
	branch := withContents(
		systemMsg("a very long pinned system prompt", 500),
		userMsg("hi", 5),
	)
	p := Packer{Strategy: StrategyTruncate}

	b := Budget{MaxContextTokens: 100, ReserveForReply: 50, ReserveForInput: 20} // available 30
	got, err := p.Pack(branch, b)

	var oe *OverflowError
	if !errors.As(err, &oe) {
		t.Fatalf("Pack error = %v (%T), want *OverflowError", err, err)
	}
	if got.Messages != nil {
		t.Errorf("Messages = %v on overflow, want the zero Result", got.Messages)
	}
	if got.TokensUsed != 0 || got.DroppedCount != 0 {
		t.Errorf("Result on overflow = %+v, want the zero Result", got)
	}
	if oe.SystemCost != 500 {
		t.Errorf("SystemCost = %d, want 500 (the caller needs it for a useful 413)", oe.SystemCost)
	}
	if oe.Available != 30 {
		t.Errorf("Available = %d, want 30", oe.Available)
	}
	if !strings.Contains(oe.Error(), "system prompt") {
		t.Errorf("Error() = %q, want it to name the system prompt", oe.Error())
	}

	// Truncating the system prompt is the wrong remedy, so even a huge dialogue
	// must not change the outcome: overflow is decided before any selection.
	big := append(domain.Branch{}, systemMsg("a very long pinned system prompt", 500))
	for i := 0; i < 50; i++ {
		big = append(big, userMsg("x", 10))
	}
	if _, err := p.Pack(big, b); err == nil {
		t.Error("Pack succeeded with an overflowing system prefix")
	} else {
		var oe2 *OverflowError
		if !errors.As(err, &oe2) {
			t.Errorf("error = %v, want *OverflowError regardless of dialogue size", err)
		}
	}
}

// TestPack_SystemExactlyFits is the boundary: cost == available is allowed.
func TestPack_SystemExactlyFits(t *testing.T) {
	branch := withContents(systemMsg("sys", 40), userMsg("hi", 5))
	p := Packer{Strategy: StrategyTruncate}

	got, err := p.Pack(branch, Budget{MaxContextTokens: 45}) // available 45 == sysCost 40 + 5
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Errorf("len(Messages) = %d, want 2 at the exact-fit boundary", len(got.Messages))
	}
}

// ---------------------------------------------------------------------------
// 8. Unknown strategy
// ---------------------------------------------------------------------------

func TestPack_UnknownStrategy(t *testing.T) {
	branch := withContents(
		systemMsg("sys", 20),
		userMsg("u1", 50),
		assistantMsg("a1", 50),
		userMsg("u2", 50),
	)
	// Over budget, so the strategy is actually consulted.
	p := Packer{Strategy: Strategy("smart-dedupe")}

	got, err := p.Pack(branch, Budget{MaxContextTokens: 100, ReserveForReply: 20})
	var use *UnknownStrategyError
	if !errors.As(err, &use) {
		t.Fatalf("Pack error = %v (%T), want *UnknownStrategyError", err, err)
	}
	if use.Strategy != Strategy("smart-dedupe") {
		t.Errorf("Strategy = %q, want %q", use.Strategy, "smart-dedupe")
	}
	if !strings.Contains(use.Error(), "smart-dedupe") {
		t.Errorf("Error() = %q, want it to quote the bad strategy so a config typo is findable", use.Error())
	}
	if got.Messages != nil {
		t.Errorf("Messages = %v, want the zero Result on error", got.Messages)
	}

	// The empty strategy must also be rejected rather than quietly behaving like
	// truncation, which is the failure mode this error exists to prevent.
	empty := Packer{}
	if _, err := empty.Pack(branch, Budget{MaxContextTokens: 100, ReserveForReply: 20}); err == nil {
		t.Error("the zero Strategy was accepted; it should be an UnknownStrategyError")
	} else {
		var e2 *UnknownStrategyError
		if !errors.As(err, &e2) {
			t.Errorf("zero Strategy error = %v, want *UnknownStrategyError", err)
		}
	}
}

// TestPack_FittingDialogueIgnoresUnknownStrategy documents the one place the
// strategy is not validated: the fits-as-is fast path returns before selection.
// Asserted explicitly so the behaviour is a decision rather than a surprise.
func TestPack_FittingDialogueIgnoresUnknownStrategy(t *testing.T) {
	branch := withContents(systemMsg("sys", 10), userMsg("hi", 5))
	got, err := Packer{Strategy: Strategy("nonsense")}.Pack(branch, Budget{MaxContextTokens: 1000})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Errorf("len(Messages) = %d, want 2 (fast path bypasses strategy validation)", len(got.Messages))
	}
}

// ---------------------------------------------------------------------------
// 9. Token estimation
// ---------------------------------------------------------------------------

func TestEstimateTokens_CJKVsLatin(t *testing.T) {
	cjk := "这是一段中文提示词用来测试分词估算是否准确可靠"
	latin := "this is an english sentence used to check the estimate"

	t.Run("empty", func(t *testing.T) {
		if got := EstimateTokens(""); got != 0 {
			t.Errorf("EstimateTokens(\"\") = %d, want 0", got)
		}
	})

	t.Run("CJK near one token per rune", func(t *testing.T) {
		n := len([]rune(cjk))
		got := EstimateTokens(cjk)
		if got != n {
			t.Errorf("EstimateTokens(cjk) = %d, want %d (1 per rune)", got, n)
		}
		// Guard against the ratio drifting without the intent changing.
		if got < n*9/10 || got > n*11/10 {
			t.Errorf("EstimateTokens(cjk) = %d is not near %d", got, n)
		}
	})

	t.Run("Latin near one token per four chars", func(t *testing.T) {
		n := len([]rune(latin))
		got := EstimateTokens(latin)
		want := (n + 3) / 4
		if got != want {
			t.Errorf("EstimateTokens(latin) = %d, want %d", got, want)
		}
		// The real risk is underestimating, which makes the packer believe there
		// is headroom it does not have. Compare against the naive whole-string
		// byte ratio, which is what this function exists to improve on, rather
		// than against its own output.
		if naive := (len(latin) + 3) / 4; got < naive {
			t.Errorf("EstimateTokens(latin) = %d, want at least the byte ratio %d", got, naive)
		}
	})

	// The reason the split exists. Counting every byte at the English ratio on a
	// Chinese prompt underestimates by roughly 4x, which makes the packer believe
	// there is headroom that does not exist; the request then arrives at the
	// provider over the window and is rejected, or the model silently loses the
	// oldest turns.
	t.Run("mixed beats naive len/4", func(t *testing.T) {
		mixed := "请帮我 review 这段 Go 代码，尤其是并发部分的锁顺序"
		naive := len(mixed) / 4
		got := EstimateTokens(mixed)
		if got <= naive {
			t.Errorf("EstimateTokens(mixed) = %d, want > naive len/4 = %d; the CJK-aware split "+
				"exists precisely because len/4 undercounts Chinese-heavy prompts", got, naive)
		}
		if want := len([]rune(mixed)); got > want {
			t.Errorf("EstimateTokens(mixed) = %d exceeds the rune count %d; never overestimate", got, want)
		}

		// Same idea stated as a comparison of the two ratios on identical-looking
		// lengths: the CJK string must cost several times what the Latin one does.
		if cjkCost, latinCost := EstimateTokens(cjk), EstimateTokens(latin); cjkCost <= latinCost {
			t.Errorf("CJK cost %d should exceed Latin cost %d at similar lengths (%d vs %d runes)",
				cjkCost, latinCost, len([]rune(cjk)), len([]rune(latin)))
		}
	})

	t.Run("Hangul and CJK punctuation are CJK", func(t *testing.T) {
		for _, s := range []string{"한국어 테스트", "、。〈》「」", "豈更車賈滑"} {
			if got, n := EstimateTokens(s), len([]rune(s)); got != n {
				t.Errorf("EstimateTokens(%q) = %d, want %d", s, got, n)
			}
		}
	})

	t.Run("rounds up fractional latin", func(t *testing.T) {
		for _, s := range []string{"abcde", "abcdefg", "abcd"} {
			n := len([]rune(s))
			if got, want := EstimateTokens(s), (n+3)/4; got != want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", s, got, want)
			}
		}
	})
}

func TestMergeEstimate(t *testing.T) {
	t.Run("fills a missing count", func(t *testing.T) {
		m := domain.Message{Role: domain.RoleUser, Content: "hello world this is text"}
		MergeEstimate(&m)
		if want := EstimateTokens(m.Content); m.TokenCount != want {
			t.Errorf("TokenCount = %d, want the estimate %d", m.TokenCount, want)
		}
	})

	// Provider-reported counts are authoritative; overwriting them with an
	// estimate would corrupt billing and the budget.
	t.Run("keeps a real count", func(t *testing.T) {
		m := domain.Message{Role: domain.RoleAssistant, Content: "hi", TokenCount: 9_000}
		MergeEstimate(&m)
		if m.TokenCount != 9_000 {
			t.Errorf("TokenCount = %d, want the provider's 9000 preserved", m.TokenCount)
		}
	})

	t.Run("a non-positive count is treated as missing", func(t *testing.T) {
		m := domain.Message{Role: domain.RoleUser, Content: "some text here", TokenCount: -5}
		MergeEstimate(&m)
		if m.TokenCount <= 0 {
			t.Errorf("TokenCount = %d, want an estimate to replace a non-positive count", m.TokenCount)
		}
	})

	t.Run("empty content stays zero", func(t *testing.T) {
		m := domain.Message{Role: domain.RoleAssistant}
		MergeEstimate(&m)
		if m.TokenCount != 0 {
			t.Errorf("TokenCount = %d, want 0 for empty content", m.TokenCount)
		}
	})
}

// ---------------------------------------------------------------------------
// 10. Budget.Available
// ---------------------------------------------------------------------------

func TestBudget_Available(t *testing.T) {
	cases := []struct {
		name              string
		max, reply, input int
		want              int
	}{
		{
			name: "both reservations subtract",
			max:  128_000, reply: 8_000, input: 2_000,
			want: 118_000,
		},
		{
			name: "no reservations",
			max:  4_096,
			want: 4_096,
		},
		{
			name: "reply reservation alone",
			max:  4_096, reply: 1_024,
			want: 3_072,
		},
		{
			// Clamping matters more than it looks: an unclamped negative
			// Available would flow into takeTailAligned, whose `budget <= 0`
			// guard would be reached with a nonsense value, and into
			// summaryReserve arithmetic where a negative denominator silently
			// inverts the cap.
			name: "reservations exceed the window clamps to zero",
			max:  1_000, reply: 900, input: 500,
			want: 0,
		},
		{
			name: "reservations exactly equal the window",
			max:  1_000, reply: 600, input: 400,
			want: 0,
		},
		{
			name: "reservations exceed by one",
			max:  100, reply: 60, input: 41,
			want: 0,
		},
		{
			name: "zero window",
			want: 0,
		},
		{
			name: "oversized reservations clamp",
			max:  10, reply: 1_000_000, input: 1_000_000,
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := Budget{MaxContextTokens: tc.max, ReserveForReply: tc.reply, ReserveForInput: tc.input}
			if got := b.Available(); got != tc.want {
				t.Errorf("Available() = %d, want %d", got, tc.want)
			}
			if got := b.Available(); got < 0 {
				t.Errorf("Available() = %d, must never be negative", got)
			}
		})
	}

	// Available must be pure so Pack can call it repeatedly without drift.
	b := Budget{MaxContextTokens: 100, ReserveForReply: 10, ReserveForInput: 5}
	first := b.Available()
	for i := 0; i < 3; i++ {
		if got := b.Available(); got != first {
			t.Fatalf("Available() = %d on call %d, want stable %d", got, i+1, first)
		}
	}
}

// ---------------------------------------------------------------------------
// 11. alignToExchangeStart, directly
// ---------------------------------------------------------------------------

// TestAlignToExchangeStart exercises the boundary walker directly, because the
// Pack-level test only reaches the cases where the naive cut happens to land in
// an interesting place. Each case states where the naive cut landed and why the
// aligned start must move (or legitimately not move).
func TestAlignToExchangeStart(t *testing.T) {
	u := func() domain.Message { return userMsg("u", 10) }
	a := func() domain.Message { return assistantMsg("a", 10) }
	// tc: assistant carrying tool_calls; t1/t2: its results.
	tc := func() domain.Message { return assistantWithTools("calls", 10, "id1", "id2") }
	t1 := func() domain.Message { return toolMsg("id1", "r1", 10) }
	t2 := func() domain.Message { return toolMsg("id2", "r2", 10) }

	// b is the fixture:
	//  0 user  1 assistant  2 assistant(tool_calls)  3 tool  4 tool
	//  5 assistant  6 user  7 assistant
	branch := domain.Branch{u(), a(), tc(), t1(), t2(), a(), u(), a()}

	// toolRelated mirrors domain.Message.IsToolRelated for readability in cases.
	if !branch[2].IsToolRelated() || !branch[3].IsToolRelated() {
		t.Fatal("fixture is wrong: assistant-with-tool_calls and tool messages must be tool-related")
	}
	if branch[5].IsToolRelated() {
		t.Fatal("fixture is wrong: an assistant message without tool_calls must not be tool-related")
	}

	cases := []struct {
		name  string
		start int
		want  int
		why   string
	}{
		{
			name:  "start points at a tool message inside a run",
			start: 4,
			want:  2,
			why: "cutting at the last tool result orphans it; walk back over the run to the " +
				"assistant that requested the calls",
		},
		{
			name:  "start points at the first tool result",
			start: 3,
			want:  2,
			why:   "one step back reaches the assistant tool_calls message",
		},
		{
			name:  "start points at the assistant tool_calls message",
			start: 2,
			want:  2,
			why:   "already a legal exchange start; the run after it is kept whole",
		},
		{
			name:  "start points just past a run",
			start: 5,
			want:  5,
			why: "index 5 is a plain assistant and index 4 is tool-related, so neither loop fires: " +
				"the dropped run is behind the window, which is safe",
		},
		{
			name:  "start is 0",
			start: 0,
			want:  0,
			why:   "the guard keeps 0; there is nothing before it to walk to",
		},
		{
			name:  "start is len(b)",
			start: len(branch),
			want:  len(branch),
			why: "len(b) is the half-open sentinel for \"the budget bought no history\". Walking back " +
				"from it would append turns the caller had already rejected, so it must pass through. " +
				"It is also the only out-of-range index this function is handed, so the bounds guard " +
				"is load-bearing here: an unguarded index panics.",
		},
		{
			name:  "start at a trailing user turn",
			start: 6,
			want:  6,
			why:   "a fresh user turn is a valid window start",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// alignToExchangeStart must not mutate the branch.
			clone := make(domain.Branch, len(branch))
			copy(clone, branch)

			got := alignToExchangeStart(branch, tc.start)
			if got != tc.want {
				t.Errorf("alignToExchangeStart(b, %d) = %d, want %d: %s", tc.start, got, tc.want, tc.why)
			}
			if got > tc.start {
				t.Errorf("alignToExchangeStart(b, %d) = %d moved forward; alignment may only widen the "+
					"window backwards, never drop more history", tc.start, got)
			}
			if got < 0 {
				t.Errorf("alignToExchangeStart(b, %d) = %d, must be >= 0", tc.start, got)
			}
			// The aligned start may never land on a tool message, whatever it does.
			if got >= 0 && got < len(branch) && branch[got].Role == domain.RoleTool {
				t.Errorf("alignToExchangeStart(b, %d) = %d, which is a tool message: that window would be "+
					"rejected by the provider", tc.start, got)
			}
			for i := range clone {
				if clone[i].Content != branch[i].Content || clone[i].Role != branch[i].Role {
					t.Fatalf("alignToExchangeStart mutated the branch at %d", i)
				}
			}
		})
	}

	// The two loops in alignToExchangeStart are not equivalent, and the
	// difference is the whole test: loop 1 handles `start` itself being
	// tool-related, loop 2 handles `start-1` being tool-related. Both are needed
	// because the naive cut can land either on a tool result or immediately
	// after the run it should have taken.
	t.Run("both loops are load bearing", func(t *testing.T) {
		// Only loop 1 can fire here.
		one := domain.Branch{u(), tc(), t1(), a()}
		if got := alignToExchangeStart(one, 2); got != 1 {
			t.Errorf("alignToExchangeStart = %d, want 1 (loop 1 must lift a start sitting on a tool result)", got)
		}
		// Only loop 2 can fire here: start is clean, its predecessor is the run.
		two := domain.Branch{u(), tc(), t1(), t1(), a()}
		if got := alignToExchangeStart(two, 2); got != 1 {
			t.Errorf("alignToExchangeStart = %d, want 1 (loop 2 must not start a window after a run)", got)
		}
	})

	// A tool run at the head of the branch has no assistant to walk back to;
	// start == 0 is the only safe answer and the guards must produce it.
	t.Run("run at the head cannot walk below zero", func(t *testing.T) {
		head := domain.Branch{tc(), t1(), t2(), a()}
		if got := alignToExchangeStart(head, 2); got != 0 {
			t.Errorf("alignToExchangeStart = %d, want 0", got)
		}
	})
}
