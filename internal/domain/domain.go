// Package domain holds the core conversation model. It has no dependency on
// HTTP, SQL or any LLM provider, so the tricky parts (message tree traversal,
// context packing) can be unit tested in isolation.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Role is the OpenAI-style message author role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ErrNotFound is returned by repositories when a row is missing. Handlers map
// it to 404 so the storage layer never leaks driver-specific errors upward.
var ErrNotFound = errors.New("domain: not found")

// Conversation is a named container of messages arranged in a tree.
//
// A conversation is NOT a list. Editing a message in the middle and
// regenerating creates a sibling subtree, so the messages form a tree whose
// roots are system messages. The "visible" conversation is the single path
// from a root to CurrentLeaf.
// JSON tags are required: without them encoding/json emits Go field names
// (ID, CurrentLeaf), which is a broken public contract and a silent breaking
// change the moment a field is renamed.
type Conversation struct {
	ID      uuid.UUID `json:"id"`
	Title   string    `json:"title"`
	OwnerID string    `json:"-"` // never leak tenant identity to other callers
	Model   string    `json:"model"`
	// CurrentLeaf points at the message the UI is currently showing the
	// continuation of. Switching branches means repointing this, not moving
	// or deleting rows.
	CurrentLeaf uuid.UUID `json:"current_leaf"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Message is one node in the conversation tree.
type Message struct {
	ID             uuid.UUID `json:"id"`
	ConversationID uuid.UUID `json:"conversation_id"`
	// ParentID is nil for root messages (system prompts).
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	Role     Role       `json:"role"`
	Content  string     `json:"content"`

	// ToolCallID / ToolName are set when Role == RoleTool, and ToolCalls when
	// Role == RoleAssistant requested a tool. They exist because the OpenAI
	// API contract requires an assistant message with tool_calls to be
	// immediately followed by tool messages with matching tool_call_ids;
	// breaking that pair apart makes the upstream reject the request with 400.
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`

	// TokenCount is the provider-reported token cost of this single message,
	// cached so context packing never has to re-tokenise history.
	TokenCount int `json:"token_count"`

	// Depth is denormalised (root = 0) purely to make "is this subtree deep"
	// cheap for the compaction policy and for rendering indentation.
	Depth int `json:"depth"`

	CreatedAt time.Time `json:"created_at"`
}

// ToolCall is a tool invocation requested by the assistant.
type ToolCall struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// IsToolRelated reports whether the message participates in a tool-call
// exchange. The context packer must never cut a run of these in half.
func (m *Message) IsToolRelated() bool {
	return m.Role == RoleTool || (m.Role == RoleAssistant && len(m.ToolCalls) > 0)
}

// Branch is the linear view of a conversation: the path from a root message
// down to a chosen leaf, oldest first. This is the shape the LLM API needs.
type Branch []Message

// SystemPrefix returns the leading system messages of the branch. Providers
// treat these as pinned context that must survive any truncation.
func (b Branch) SystemPrefix() Branch {
	var out Branch
	for _, m := range b {
		if m.Role != RoleSystem {
			break
		}
		out = append(out, m)
	}
	return out
}

// Dialogue returns everything after the leading system messages.
func (b Branch) Dialogue() Branch {
	i := 0
	for i < len(b) && b[i].Role == RoleSystem {
		i++
	}
	return b[i:]
}
