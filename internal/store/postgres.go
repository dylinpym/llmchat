package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dylin/llmchat/internal/domain"
)

// PG implements ConversationRepo and MessageRepo on Postgres.
type PG struct {
	pool *pgxpool.Pool
}

func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

func (p *PG) Pool() *pgxpool.Pool { return p.pool }

func parseUUID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(s)
}

// ---------------------------------------------------------------- conversations

func (p *PG) Create(ctx context.Context, c domain.Conversation) (domain.Conversation, error) {
	id := c.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	row := p.pool.QueryRow(ctx, `
		INSERT INTO conversations (id, title, owner_id, model)
		VALUES ($1, $2, $3, $4)
		RETURNING id, title, owner_id, model, current_leaf, created_at, updated_at`,
		id, c.Title, c.OwnerID, c.Model)

	var out domain.Conversation
	var leafPtr *uuid.UUID
	if err := row.Scan(&out.ID, &out.Title, &out.OwnerID, &out.Model,
		&leafPtr, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return out, fmt.Errorf("store: create conversation: %w", err)
	}
	if leafPtr != nil {
		out.CurrentLeaf = *leafPtr
	}
	return out, nil
}

const convCols = `id, title, owner_id, model, current_leaf, created_at, updated_at`

func scanConv(row pgx.Row) (domain.Conversation, error) {
	var c domain.Conversation
	var leafPtr *uuid.UUID
	if err := row.Scan(&c.ID, &c.Title, &c.OwnerID, &c.Model,
		&leafPtr, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, domain.ErrNotFound
		}
		return c, err
	}
	if leafPtr != nil {
		c.CurrentLeaf = *leafPtr
	}
	return c, nil
}

func (p *PG) Get(ctx context.Context, id string) (domain.Conversation, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return domain.Conversation{}, domain.ErrNotFound
	}
	return scanConv(p.pool.QueryRow(ctx,
		`SELECT `+convCols+` FROM conversations WHERE id = $1`, uid))
}

func (p *PG) List(ctx context.Context, ownerID string, limit, offset int) ([]domain.Conversation, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT `+convCols+` FROM conversations
		 WHERE owner_id = $1 ORDER BY updated_at DESC LIMIT $2 OFFSET $3`,
		ownerID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Conversation
	for rows.Next() {
		c, err := scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *PG) Touch(ctx context.Context, id, title string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`UPDATE conversations SET updated_at = now(), title = COALESCE(NULLIF($2,''), title)
		 WHERE id = $1`, uid, title)
	return err
}

func (p *PG) SetCurrentLeaf(ctx context.Context, id, leafID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if leafID == "" {
		_, err = p.pool.Exec(ctx,
			`UPDATE conversations SET current_leaf = NULL, updated_at = now() WHERE id = $1`, uid)
		return err
	}
	leaf, err := parseUUID(leafID)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`UPDATE conversations SET current_leaf = $2, updated_at = now() WHERE id = $1`, uid, leaf)
	return err
}

func (p *PG) Delete(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `DELETE FROM conversations WHERE id = $1`, uid)
	return err
}

// -------------------------------------------------------------------- messages

const msgCols = `id, conversation_id, parent_id, role, content,
	tool_call_id, tool_name, tool_calls, token_count, depth, is_active_leaf, created_at`

func scanMsg(row pgx.Row) (domain.Message, error) {
	var (
		m       domain.Message
		parent  *uuid.UUID
		toolRaw []byte
	)
	err := row.Scan(&m.ID, &m.ConversationID, &parent, &m.Role, &m.Content,
		&m.ToolCallID, &m.ToolName, &toolRaw, &m.TokenCount, &m.Depth,
		new(bool), &m.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return m, domain.ErrNotFound
		}
		return m, err
	}
	if parent != nil {
		m.ParentID = parent
	}
	if len(toolRaw) > 0 {
		_ = json.Unmarshal(toolRaw, &m.ToolCalls)
	}
	return m, nil
}

// AppendMessage inserts a node and repoints the active leaf atomically.
//
// Two tabs posting to the same conversation concurrently would otherwise both
// try to become the active leaf; the partial unique index turns that race into a
// clean serialisation failure instead of two silently diverging branches.
func (p *PG) AppendMessage(ctx context.Context, m domain.Message) (domain.Message, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return m, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	convID, err := parseUUID(m.ConversationID.String())
	if err != nil {
		return m, err
	}

	// Serialise appends to the same conversation.
	//
	// Without this, two concurrent writers on one conversation (a user with the
	// chat open in two tabs, or a double-submit) both run the clear-then-insert
	// below under READ COMMITTED. Each clears the current leaf and each inserts a
	// new row flagged is_active_leaf before either commits, so one hits the
	// partial unique index and fails with a 23505 unique violation — surfaced to
	// the client as a 500. The index correctly protects the invariant; the write
	// path just was not serialised, so it detected conflicts instead of avoiding
	// them. A load test reproduced this at roughly one failure per twenty
	// concurrent same-conversation sends.
	//
	// Locking the parent conversation row makes the second writer block until the
	// first commits, so both succeed and the leaf simply advances twice. The lock
	// is per-conversation, so unrelated conversations never contend. The partial
	// unique index is kept as a defensive last line; after this it should never
	// fire, and a load test confirms zero violations.
	var lock uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM conversations WHERE id = $1 FOR UPDATE`, convID).Scan(&lock); err != nil {
		return m, fmt.Errorf("store: lock conversation: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE messages SET is_active_leaf = false
		 WHERE conversation_id = $1 AND is_active_leaf`, convID); err != nil {
		return m, fmt.Errorf("store: clear leaf: %w", err)
	}

	toolJSON, err := json.Marshal(m.ToolCalls)
	if err != nil {
		return m, err
	}
	if len(m.ToolCalls) == 0 {
		toolJSON = []byte(`[]`)
	}

	var parent any
	if m.ParentID != nil && *m.ParentID != uuid.Nil {
		parent = *m.ParentID
	}

	var out domain.Message
	var insertedParent *uuid.UUID
	var insertedToolRaw []byte
	err = tx.QueryRow(ctx, `
		INSERT INTO messages (
			id, conversation_id, parent_id, role, content,
			tool_call_id, tool_name, tool_calls, token_count, depth, is_active_leaf
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true)
		RETURNING `+msgCols,
		m.ID, convID, parent, m.Role, m.Content,
		m.ToolCallID, m.ToolName, toolJSON, m.TokenCount, m.Depth).
		Scan(&out.ID, &out.ConversationID, &insertedParent, &out.Role, &out.Content,
			&out.ToolCallID, &out.ToolName, &insertedToolRaw, &out.TokenCount, &out.Depth,
			new(bool), &out.CreatedAt)
	if err != nil {
		return m, fmt.Errorf("store: insert message: %w", err)
	}
	out.ParentID = insertedParent
	if len(insertedToolRaw) > 0 {
		_ = json.Unmarshal(insertedToolRaw, &out.ToolCalls)
	}

	// Keep the header's current_leaf in step with is_active_leaf. Both rows move
	// in one transaction, so the two representations of "the active leaf" can
	// never disagree. Leaving this to callers is what caused the first version of
	// this service to append messages under a stale leaf: every write path had to
	// remember a second UPDATE, and only one of them did.
	if _, err := tx.Exec(ctx,
		`UPDATE conversations SET current_leaf = $2, updated_at = now() WHERE id = $1`,
		convID, out.ID); err != nil {
		return m, err
	}
	if err := tx.Commit(ctx); err != nil {
		return m, fmt.Errorf("store: commit append: %w", err)
	}
	return out, nil
}

// Branch walks from leaf to root in one round trip via a recursive CTE.
func (p *PG) Branch(ctx context.Context, leafID string) (domain.Branch, error) {
	uid, err := parseUUID(leafID)
	if err != nil {
		return nil, domain.ErrNotFound
	}
	rows, err := p.pool.Query(ctx, `
		WITH RECURSIVE ancestry AS (
			SELECT m.* FROM messages m WHERE m.id = $1
			UNION ALL
			SELECT par.* FROM messages par JOIN ancestry a ON par.id = a.parent_id
		)
		SELECT `+msgCols+` FROM ancestry ORDER BY depth ASC, created_at ASC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out domain.Branch
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *PG) Tree(ctx context.Context, conversationID string) ([]domain.Message, error) {
	uid, err := parseUUID(conversationID)
	if err != nil {
		return nil, domain.ErrNotFound
	}
	rows, err := p.pool.Query(ctx, `
		SELECT `+msgCols+` FROM messages
		WHERE conversation_id = $1 ORDER BY depth ASC, created_at ASC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *PG) Children(ctx context.Context, parentID string) ([]domain.Message, error) {
	uid, err := parseUUID(parentID)
	if err != nil {
		return nil, domain.ErrNotFound
	}
	rows, err := p.pool.Query(ctx, `
		SELECT `+msgCols+` FROM messages WHERE parent_id = $1 ORDER BY created_at ASC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *PG) SwitchBranch(ctx context.Context, conversationID, leafID string) error {
	convID, err := parseUUID(conversationID)
	if err != nil {
		return domain.ErrNotFound
	}
	leaf, err := parseUUID(leafID)
	if err != nil {
		return domain.ErrNotFound
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Same per-conversation serialisation as AppendMessage: two concurrent branch
	// switches to different leaves would otherwise both clear and both set
	// is_active_leaf, violating the one-active-leaf invariant.
	var lock uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM conversations WHERE id = $1 FOR UPDATE`, convID).Scan(&lock); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE messages SET is_active_leaf = false WHERE conversation_id = $1 AND is_active_leaf`,
		convID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`UPDATE messages SET is_active_leaf = true WHERE id = $1 AND conversation_id = $2`,
		leaf, convID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Guards against switching to a leaf belonging to another conversation,
		// which would otherwise silently merge two histories.
		return domain.ErrNotFound
	}
	if _, err := tx.Exec(ctx,
		`UPDATE conversations SET current_leaf = $1, updated_at = now() WHERE id = $2`,
		leaf, convID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PG) NextDepth(ctx context.Context, conversationID, parentID string) (int, error) {
	if parentID == "" {
		return 0, nil
	}
	pID, err := parseUUID(parentID)
	if err != nil {
		return 0, domain.ErrNotFound
	}
	var parentDepth int
	err = p.pool.QueryRow(ctx,
		`SELECT depth FROM messages WHERE id = $1`, pID).Scan(&parentDepth)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.ErrNotFound
		}
		return 0, err
	}
	return parentDepth + 1, nil
}

func (p *PG) UpdateMessageContent(ctx context.Context, id, content string, tokenCount int) (domain.Message, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return domain.Message{}, domain.ErrNotFound
	}
	return scanMsg(p.pool.QueryRow(ctx, `
		UPDATE messages SET content = $2, token_count = $3 WHERE id = $1
		RETURNING `+msgCols, uid, content, tokenCount))
}
