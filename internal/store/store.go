// Package store defines persistence ports and their Postgres implementation.
//
// The service layer depends on ConversationRepo/MessageRepo, never on pgx, so
// the branching logic is testable with a fake and the SQL stays in one place.
package store

import (
	"context"

	"github.com/dylin/llmchat/internal/domain"
)

// ConversationRepo persists conversation headers.
type ConversationRepo interface {
	Create(ctx context.Context, c domain.Conversation) (domain.Conversation, error)
	Get(ctx context.Context, id string) (domain.Conversation, error)
	List(ctx context.Context, ownerID string, limit, offset int) ([]domain.Conversation, error)
	Touch(ctx context.Context, id, title string) error
	// SetCurrentLeaf repoints a conversation's active leaf on the header row.
	// Kept separate from Touch because most appends do not change the title.
	SetCurrentLeaf(ctx context.Context, id, leafID string) error
	Delete(ctx context.Context, id string) error
}

// MessageRepo persists the message tree.
type MessageRepo interface {
	// AppendMessage inserts a node and makes it the conversation's active leaf.
	// Implementations must do the leaf swap and the insert in one transaction so
	// the one-active-leaf invariant cannot be transiently violated by a
	// concurrent request from a second browser tab.
	AppendMessage(ctx context.Context, m domain.Message) (domain.Message, error)

	// Branch returns the root-to-leaf path, oldest first. This is the read that
	// the context packer consumes.
	Branch(ctx context.Context, leafID string) (domain.Branch, error)

	// Tree returns every node in a conversation, for the branch picker.
	Tree(ctx context.Context, conversationID string) ([]domain.Message, error)

	// Children returns direct children; len > 1 means this node has siblings.
	Children(ctx context.Context, parentID string) ([]domain.Message, error)

	// SwitchBranch repoints the active leaf without touching message rows.
	SwitchBranch(ctx context.Context, conversationID, leafID string) error

	// NextDepth returns the depth a child of parentID would have.
	NextDepth(ctx context.Context, conversationID, parentID string) (int, error)

	// UpdateMessageContent edits a node in place, leaving descendants intact.
	UpdateMessageContent(ctx context.Context, id, content string, tokenCount int) (domain.Message, error)
}
