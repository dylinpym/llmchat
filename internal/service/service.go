// Package service orchestrates conversations: assembling context, calling the
// model, persisting the reply as a new node in the tree.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dylin/llmchat/internal/contextpack"
	"github.com/dylin/llmchat/internal/domain"
	"github.com/dylin/llmchat/internal/observability"
	"github.com/dylin/llmchat/internal/provider"
	"github.com/dylin/llmchat/internal/store"
)

// ErrLeafNotInConversation prevents switching or replying against a leaf that
// belongs to a different conversation.
var ErrLeafNotInConversation = errors.New("service: leaf does not belong to conversation")

// Config tunes context assembly per request.
type Config struct {
	Strategy     contextpack.Strategy
	MaxContext   int
	ReserveReply int
	ReserveInput int
	PinFirst     bool
	// SummaryModel may be a cheaper model than the conversation's, since
	// summarisation quality matters less than reply quality.
	SummaryModel string
}

// Service is the application service.
type Service struct {
	convs   store.ConversationRepo
	msgs    store.MessageRepo
	prov    provider.Provider
	summar  provider.Provider // may be the same instance
	cfg     Config
	metrics *observability.Metrics
}

func New(
	convs store.ConversationRepo,
	msgs store.MessageRepo,
	prov provider.Provider,
	cfg Config,
	metrics *observability.Metrics,
) *Service {
	return &Service{
		convs: convs, msgs: msgs, prov: prov, summar: prov, cfg: cfg, metrics: metrics,
	}
}

// WithSummariser overrides the provider used for summarisation.
func (s *Service) WithSummariser(p provider.Provider) *Service {
	s.summar = p
	return s
}

// Create starts a conversation with a system message so the tree always has a
// root, which keeps depth arithmetic and branch walking uniform.
func (s *Service) Create(ctx context.Context, ownerID, title, model, systemPrompt string) (domain.Conversation, error) {
	conv, err := s.convs.Create(ctx, domain.Conversation{
		ID: uuid.New(), Title: title, OwnerID: ownerID, Model: model,
	})
	if err != nil {
		return conv, err
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return conv, nil
	}

	msg := domain.Message{
		ID:             uuid.New(),
		ConversationID: conv.ID,
		ParentID:       nil,
		Role:           domain.RoleSystem,
		Content:        systemPrompt,
		Depth:          0,
	}
	msg.TokenCount = contextpack.EstimateTokens(systemPrompt)

	saved, err := s.msgs.AppendMessage(ctx, msg)
	if err != nil {
		return conv, err
	}
	// AppendMessage repointed both is_active_leaf and conversations.current_leaf
	// inside its own transaction; mirror that on the returned value so the create
	// response carries a usable leaf instead of the zero UUID.
	conv.CurrentLeaf = saved.ID
	return conv, nil
}

// SendMessageInput describes a new user turn.
type SendMessageInput struct {
	ConversationID string
	Content        string
	// ParentID overrides the current leaf. This is what implements editing: send
	// a message whose parent is an older node and a sibling branch is created
	// instead of overwriting anything.
	ParentID string
}

// Send appends the user message, packs context, and streams the assistant reply.
//
// The returned channel yields deltas; the assistant message is persisted when
// generation ends — including on cancellation, with whatever partial text was
// produced, so a user who closed the tab still sees the incomplete reply rather
// than a blank gap in history.
func (s *Service) Send(ctx context.Context, in SendMessageInput) (<-chan provider.Chunk, error) {
	conv, err := s.convs.Get(ctx, in.ConversationID)
	if err != nil {
		return nil, err
	}

	parentID := in.ParentID
	if parentID == "" && conv.CurrentLeaf != uuid.Nil {
		parentID = conv.CurrentLeaf.String()
	}

	depth, err := s.msgs.NextDepth(ctx, conv.ID.String(), parentID)
	if err != nil {
		return nil, err
	}

	userMsg := domain.Message{
		ID:             uuid.New(),
		ConversationID: conv.ID,
		Role:           domain.RoleUser,
		Content:        in.Content,
		Depth:          depth,
	}
	userMsg.TokenCount = contextpack.EstimateTokens(in.Content)

	var parent uuid.UUID
	if parentID != "" {
		parent, err = uuid.Parse(parentID)
		if err != nil {
			return nil, ErrLeafNotInConversation
		}
		userMsg.ParentID = &parent
	}

	savedUser, err := s.msgs.AppendMessage(ctx, userMsg)
	if err != nil {
		return nil, err
	}

	// Read back the path that includes the message just added. Going through the
	// repository rather than appending in memory keeps the view identical to what
	// any other instance would read — the service is stateless so a replica may
	// serve the next request.
	branch, err := s.msgs.Branch(ctx, savedUser.ID.String())
	if err != nil {
		return nil, err
	}

	packed, err := s.packer().Pack(branch, s.budget())
	if err != nil {
		return nil, err
	}
	if s.metrics != nil {
		s.metrics.ObservePack(string(s.cfg.Strategy), packed.DroppedCount, packed.Summarised, packed.TokensUsed)
	}

	stream, err := s.prov.Stream(ctx, provider.Request{
		Model:    conv.Model,
		Messages: packed.Messages,
		Stream:   true,
	})
	if err != nil {
		return nil, err
	}

	out := make(chan provider.Chunk, 32)
	go s.persistStream(ctx, conv, savedUser, stream, out, packed.TokensUsed)
	return out, nil
}

// persistStream forwards chunks and writes the assistant node when done.
func (s *Service) persistStream(
	ctx context.Context,
	conv domain.Conversation,
	userMsg domain.Message,
	in <-chan provider.Chunk,
	out chan<- provider.Chunk,
	promptTokens int,
) {
	defer close(out)

	var (
		sb       strings.Builder
		usage    provider.Usage
		started  = time.Now()
		firstTok time.Duration
		gotFirst bool
	)

	finish := func(interrupted bool) {
		if sb.Len() == 0 && !interrupted {
			return
		}
		text := sb.String()
		assistant := domain.Message{
			ID:             uuid.New(),
			ConversationID: conv.ID,
			ParentID:       &userMsg.ID,
			Role:           domain.RoleAssistant,
			Content:        text,
			Depth:          userMsg.Depth + 1,
		}
		if usage.CompletionTokens > 0 {
			assistant.TokenCount = usage.CompletionTokens
		} else {
			// Some gateways omit usage on streams; fall back to the estimate so
			// budgeting degrades rather than breaks.
			contextpack.MergeEstimate(&assistant)
		}
		// Detach from the request context: the client is already gone on
		// cancellation, but the reply must still be saved. context.WithoutCancel
		// keeps only the values and the deadline added here, so this save is not
		// aborted by the disconnect that triggered it.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := s.msgs.AppendMessage(saveCtx, assistant); err != nil && s.metrics != nil {
			s.metrics.IncSaveFailure()
		}
		if s.metrics != nil {
			s.metrics.ObserveGeneration(time.Since(started), firstTok, promptTokens, usage.CompletionTokens, interrupted)
		}
	}

	// The select must wrap the RECEIVE, not just the send.
	//
	// `for c := range in { select { case out <- c: ... case <-ctx.Done(): } }`
	// looks equivalent and is wrong: ranging drains the upstream channel to
	// closure regardless of ctx, and once the client is gone the buffered
	// forward channel still accepts writes, so the send case always wins and the
	// ctx case is never selected. Cancellation then appears to work in tests that
	// happen to overrun the buffer and silently does nothing in production.
	//
	// Upstream cancellation itself is guaranteed separately — the provider keys
	// its goroutine off the same ctx and closes the response body — so this loop's
	// job is only to stop forwarding and persist what was produced.
	for {
		select {
		case <-ctx.Done():
			finish(true)
			return
		case c, ok := <-in:
			if !ok {
				finish(false)
				return
			}
			if c.Content != "" {
				if !gotFirst {
					firstTok = time.Since(started)
					gotFirst = true
				}
				sb.WriteString(c.Content)
			}
			if c.Usage != nil {
				usage = *c.Usage
			}
			select {
			case out <- c:
			case <-ctx.Done():
				finish(true)
				return
			}
		}
	}
}

// GetConversation returns a conversation header.
func (s *Service) GetConversation(ctx context.Context, id string) (domain.Conversation, error) {
	return s.convs.Get(ctx, id)
}

// ListConversations returns a page of a user's conversations, newest first.
func (s *Service) ListConversations(ctx context.Context, ownerID string, limit, offset int) ([]domain.Conversation, error) {
	return s.convs.List(ctx, ownerID, limit, offset)
}

// DeleteConversation removes a conversation and, via ON DELETE CASCADE, its
// messages and every sibling branch.
func (s *Service) DeleteConversation(ctx context.Context, id string) error {
	return s.convs.Delete(ctx, id)
}

// Edit rewrites a message and detaches its descendants from the active path.
//
// The old subtree is kept, not deleted: the previous answer remains reachable by
// switching branches. Deleting it would make "undo my edit" impossible.
func (s *Service) Edit(ctx context.Context, messageID, newContent string) (domain.Message, error) {
	return s.msgs.UpdateMessageContent(ctx, messageID, newContent, contextpack.EstimateTokens(newContent))
}

// SwitchBranch points the conversation at a different leaf.
func (s *Service) SwitchBranch(ctx context.Context, conversationID, leafID string) error {
	return s.msgs.SwitchBranch(ctx, conversationID, leafID)
}

// Branch returns the active path, for rendering the transcript.
func (s *Service) Branch(ctx context.Context, conversationID string) (domain.Branch, error) {
	conv, err := s.convs.Get(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if conv.CurrentLeaf == uuid.Nil {
		return nil, nil
	}
	return s.msgs.Branch(ctx, conv.CurrentLeaf.String())
}

// Tree returns every node, so the client can draw branch choices.
func (s *Service) Tree(ctx context.Context, conversationID string) ([]domain.Message, error) {
	return s.msgs.Tree(ctx, conversationID)
}

func (s *Service) budget() contextpack.Budget {
	return contextpack.Budget{
		MaxContextTokens: s.cfg.MaxContext,
		ReserveForReply:  s.cfg.ReserveReply,
		ReserveForInput:  s.cfg.ReserveInput,
	}
}

func (s *Service) packer() contextpack.Packer {
	return contextpack.Packer{
		Strategy:         s.cfg.Strategy,
		Summariser:       &llmSummariser{p: s.summar, model: s.cfg.SummaryModel},
		PinFirstExchange: s.cfg.PinFirst,
	}
}

// llmSummariser adapts a provider to contextpack.Summariser.
type llmSummariser struct {
	p     provider.Provider
	model string
}

const summarisePrompt = `Condense the conversation below into a compact record.
Preserve: stated goals, decisions, names, numbers, file paths, constraints,
and anything the user corrected. Drop pleasantries and repetition.
Answer with the summary only.`

func (l *llmSummariser) Summarise(ms []domain.Message) (string, int, error) {
	if l.p == nil {
		return "", 0, errors.New("service: no summariser provider configured")
	}
	var sb strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	text, usage, err := l.p.Complete(context.Background(), provider.Request{
		Model: l.model,
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: summarisePrompt},
			{Role: domain.RoleUser, Content: sb.String()},
		},
	})
	if err != nil {
		return "", 0, err
	}
	tokens := usage.CompletionTokens
	if tokens == 0 {
		tokens = contextpack.EstimateTokens(text)
	}
	return text, tokens, nil
}
