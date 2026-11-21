// Package contextpack assembles a Branch into the message list actually sent
// to the model, under a token budget.
//
// This is where a chat backend stops being CRUD. Naively taking the last N
// messages corrupts tool-call exchanges and silently changes model behaviour.
package contextpack

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dylin/llmchat/internal/domain"
)

// Budget describes how much context the packer may use.
type Budget struct {
	// MaxContextTokens is the model's window.
	MaxContextTokens int
	// ReserveForReply is what we refuse to fill, so generation cannot overflow.
	ReserveForReply int
	// ReserveForInput is the current user turn plus the tool schema.
	ReserveForInput int
}

// Available is what history may occupy after both reservations.
func (b Budget) Available() int {
	n := b.MaxContextTokens - b.ReserveForReply - b.ReserveForInput
	if n < 0 {
		return 0
	}
	return n
}

// Strategy picks which history to send.
type Strategy string

const (
	// StrategyTruncate keeps the newest messages that fit and drops the rest.
	// Cheapest, zero extra latency, but the model forgets everything older.
	StrategyTruncate Strategy = "truncate"
	// StrategySummarise replaces the dropped middle with an LLM-written
	// summary. Preserves facts at the cost of one extra model call.
	StrategySummarise Strategy = "summarise"
	// StrategyHybrid pins the first exchange, summarises the middle, keeps a
	// recent tail. Best recall per token, most complex.
	StrategyHybrid Strategy = "hybrid"
)

// Packer turns a branch plus an optional summary into an outbound request.
type Packer struct {
	Strategy Strategy
	// Summariser is required for StrategySummarise and StrategyHybrid.
	Summariser Summariser
	// PinFirstExchange keeps the opening exchange verbatim under Hybrid: it
	// usually carries the user's actual goal, which summaries routinely lose.
	PinFirstExchange bool
}

// Summariser compresses messages into a shorter textual record. Kept as an
// interface so packing logic is testable without a model and so the summarising
// call can use a cheaper model than the main conversation.
type Summariser interface {
	Summarise(messages []domain.Message) (string, int, error)
}

// Result is the payload plus the accounting needed for metrics and billing.
type Result struct {
	Messages []domain.Message
	// TokensUsed counts the assembled messages.
	TokensUsed int
	// DroppedCount counts dialogue messages removed from the request.
	DroppedCount int
	// Summarised counts messages folded into the summary rather than deleted.
	Summarised int
	// UsedSummary reports whether a summary block is present.
	UsedSummary bool
}

// Pack assembles the outbound message list within b.Available().
func (p Packer) Pack(branch domain.Branch, b Budget) (Result, error) {
	sys := branch.SystemPrefix()
	dialogue := branch.Dialogue()

	available := b.Available()
	sysCost := costOf(sys)
	if sysCost > available {
		// The pinned system prompt alone overflows the window. Truncating it
		// would change the assistant's identity, so we surface it instead of
		// quietly degrading.
		return Result{}, &OverflowError{SystemCost: sysCost, Available: available}
	}
	available -= sysCost

	// Everything already fits: the common case for short chats. Send as-is so
	// the model sees the exact history the user sees.
	if costOf(dialogue) <= available {
		return Result{
			Messages:   append(append([]domain.Message{}, sys...), dialogue...),
			TokensUsed: sysCost + costOf(dialogue),
		}, nil
	}

	keep, summarised, dropped, err := p.selectDialogue(dialogue, available)
	if err != nil {
		return Result{}, err
	}

	out := make([]domain.Message, 0, len(sys)+len(summarised)+len(keep)+1)
	out = append(out, sys...)
	if len(summarised) > 0 {
		text, tokens, serr := p.Summariser.Summarise(p.summariseInput(summarised))
		if serr != nil {
			// A failed summary must not sink the user's turn. Degrade to pure
			// truncation and let metrics show the fallback happened.
			keep = takeTailAligned(dialogue, available)
			dropped = len(dialogue) - len(keep)
			summarised = nil
		} else {
			out = append(out, summaryMessage(text, tokens))
			out = append(out, keep...)
			return Result{
				Messages:     out,
				TokensUsed:   costOf(out),
				DroppedCount: dropped,
				Summarised:   len(summarised),
				UsedSummary:  true,
			}, nil
		}
	}

	out = append(out, keep...)
	return Result{
		Messages:     out,
		TokensUsed:   costOf(out),
		DroppedCount: dropped,
		Summarised:   len(summarised),
	}, nil
}

// summariseInput prepares the messages handed to the summariser. Under the
// pinning strategy the opening exchange is explicitly quoted as the framing
// instruction: a summary that paraphrases away the user's original goal is the
// dominant failure mode of long-chat compaction, and quoting it costs almost no
// tokens. The pinned messages are still counted as summarised because they leave
// the request as verbatim turns.
func (p Packer) summariseInput(summarised domain.Branch) domain.Branch {
	if !p.PinFirstExchange {
		return summarised
	}
	pinned := takeFirstExchange(summarised)
	if len(pinned) == 0 || len(pinned) == len(summarised) {
		return summarised
	}
	rest := summarised[len(pinned):]

	var sb strings.Builder
	sb.WriteString("The conversation opens with the turns below. Preserve them as the framing of the summary.\n\n")
	for _, m := range pinned {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	sb.WriteString("\nSummarise the remaining turns:\n")

	out := make(domain.Branch, 0, len(rest)+1)
	out = append(out, domain.Message{
		Role:       domain.RoleSystem,
		Content:    sb.String(),
		TokenCount: EstimateTokens(sb.String()),
	})
	return append(out, rest...)
}

// selectDialogue splits dialogue into (kept tail, summarised middle, dropped).
//
// Structural invariant: the kept dialogue is always a contiguous SUFFIX of the
// branch, and exchange alignment is applied exactly once, at the end. The
// obvious alternative — prepending a "pinned" opening exchange in front of an
// independently chosen tail — produces a window whose middle is missing, which
// both breaks the tool-call adjacency rule and is nonsense as conversation.
func (p Packer) selectDialogue(dialogue domain.Branch, available int) (keep, summarised domain.Branch, dropped int, err error) {
	switch p.Strategy {
	case StrategySummarise, StrategyHybrid:
		// Reserve room for the summary block itself so it cannot be the piece
		// that overflows the window.
		room := available - p.summaryReserve(dialogue, available)

		tail := takeTailAligned(dialogue, room)
		keepFrom := len(dialogue) - len(tail)

		keep = dialogue[keepFrom:]
		summarised = dialogue[:keepFrom]
		// Messages removed by alignment are neither kept nor summarised.
		dropped = len(dialogue) - len(summarised) - len(keep)
		return keep, summarised, dropped, nil

	case StrategyTruncate:
		tail := takeTailAligned(dialogue, available)
		return tail, nil, len(dialogue) - len(tail), nil

	default:
		return nil, nil, 0, &UnknownStrategyError{Strategy: p.Strategy}
	}
}

// summaryReserve is the budget carved out for the summary block. It is estimated
// from the newest turn rather than the summarised volume, because the summariser
// compresses: an output roughly proportional to its input would defeat the point.
func (p Packer) summaryReserve(dialogue domain.Branch, available int) int {
	n := dialogue[len(dialogue)-1].TokenCount / 2
	if n < 128 {
		n = 128
	}
	if n > available/2 {
		n = available / 2
	}
	return n
}

func (p Packer) degrade(dialogue domain.Branch, available int) (domain.Branch, domain.Branch) {
	return takeTailAligned(dialogue, available), nil
}

// takeTailAligned keeps the newest messages that fit budget, then walks the
// boundary backwards so a tool-call exchange is never split.
//
// This is the single most important function in the project. Cutting between an
// assistant message carrying tool_calls and its tool results makes the OpenAI
// API reject the whole request with a 400, so a plain "last N that fit" loop is
// a latent production bug that only fires once tool calling is enabled.
func takeTailAligned(b domain.Branch, budget int) domain.Branch {
	if len(b) == 0 || budget <= 0 {
		return nil
	}
	total := 0
	start := len(b)
	for i := len(b) - 1; i >= 0; i-- {
		if total+b[i].TokenCount > budget {
			break
		}
		total += b[i].TokenCount
		start = i
	}
	if start == len(b) {
		// Even the newest message is over budget. Sending nothing guarantees a
		// useless reply, so keep it anyway and let the provider clip it.
		start = len(b) - 1
	}
	start = alignToExchangeStart(b, start)
	return b[start:]
}

// alignToExchangeStart moves start back onto a legal exchange boundary so the
// first kept message is never an orphaned tool result.
//
// Only widening backwards is ever allowed. Moving forward would discard history
// the budget already agreed to keep, silently worsening recall to satisfy a
// structural rule that has a safe backwards answer.
//
// start is a half-open index and may equal len(b) ("keep nothing"), so it is
// bounds-checked before any indexing.
func alignToExchangeStart(b domain.Branch, start int) int {
	if start <= 0 || start == len(b) {
		return start
	}

	// Find the maximal run of tool results adjacent to the cut, then locate the
	// message that precedes it.
	end := start
	if b[end].Role == domain.RoleTool {
		// The cut landed inside a run: everything from here to the last
		// consecutive tool result belongs to it.
		for end < len(b) && b[end].Role == domain.RoleTool {
			end++
		}
		// Walk to the index before the first tool result of the run. `> 0` (not
		// `>= 0`) is what leaves head on the predecessor rather than on a tool
		// message; getting this wrong returns a tool index and produces exactly
		// the 400 this function exists to prevent.
		head := end - 1
		for head > 0 && b[head].Role == domain.RoleTool {
			head--
		}
		if b[head].IsToolRelated() {
			// Widen backwards to include the assistant(tool_calls) header, so
			// the results the budget already bought stay legal.
			return head
		}
		// Header already cut. These results can never be made legal, and
		// starting a window on a tool message is itself a 400, so skip past the
		// orphaned run instead of widening onto it.
		return end
	}

	// The cut landed on a non-tool message. If tool results sit immediately
	// behind the window they are already excluded from the request, which is
	// safe and needs no repair; widening backwards here would drop more history
	// than the budget agreed to.
	return start
}

// takeFirstExchange returns the leading user turn plus the assistant/tool
// replies that immediately follow it.
func takeFirstExchange(b domain.Branch) domain.Branch {
	if len(b) == 0 {
		return nil
	}
	end := 1
	for end < len(b) && b[end].Role != domain.RoleUser {
		end++
	}
	return b[:end]
}

// summaryMessage injects the compressed history as a system message. Placing it
// before the kept tail matches how ChatGPT-style backends label it, and keeps it
// out of the user/assistant alternation the providers validate.
func summaryMessage(text string, tokens int) domain.Message {
	return domain.Message{
		Role:       domain.RoleSystem,
		Content:    "[Summary of earlier conversation]\n" + strings.TrimSpace(text),
		TokenCount: tokens,
	}
}

func costOf(b domain.Branch) int {
	n := 0
	for _, m := range b {
		n += m.TokenCount
	}
	return n
}

// OverflowError means even the pinned system prompt does not fit.
type OverflowError struct {
	SystemCost int
	Available  int
}

func (e *OverflowError) Error() string {
	return "contextpack: system prompt exceeds available context budget"
}

// UnknownStrategyError guards against a typo in configuration, which would
// otherwise silently behave like truncation.
type UnknownStrategyError struct{ Strategy Strategy }

func (e *UnknownStrategyError) Error() string {
	return "contextpack: unknown strategy " + string(e.Strategy)
}

// EstimateTokens is a deliberately cheap token estimate for when the provider
// did not report a count (streaming usage is frequently omitted). It is not a
// tokenizer; it only stops the budget from being wildly wrong. Real counts from
// the API response always override it — see MergeEstimate.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	// CJK characters tokenize near 1 token per character; Latin text averages
	// roughly 4 characters per token. Counting them separately avoids assuming
	// English ratios on Chinese prompts, which underestimates by ~4x.
	var cjk, other int
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func isCJK(r rune) bool {
	return r >= 0x3000 && r <= 0x9FFF || // CJK punctuation and ideographs
		r >= 0xAC00 && r <= 0xD7AF || // Hangul
		r >= 0xF900 && r <= 0xFAFF // CJK compatibility
}

// MergeEstimate fills TokenCount from the estimate only when the provider did
// not supply a real count, so measured values always win.
func MergeEstimate(m *domain.Message) {
	if m.TokenCount <= 0 {
		m.TokenCount = EstimateTokens(m.Content)
	}
}

// SortByDepth is used by tests and by the tree loader to assert ordering.
func SortByDepth(ms []domain.Message) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Depth != ms[j].Depth {
			return ms[i].Depth < ms[j].Depth
		}
		return ms[i].CreatedAt.Before(ms[j].CreatedAt)
	})
}
