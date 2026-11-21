-- +goose Up
-- Messages form a tree: a conversation is a set of nodes, and the "active"
-- conversation is the single root-to-CurrentLeaf path. Editing a mid-conversation
-- message and regenerating creates a sibling subtree rather than mutating rows,
-- which is what makes ChatGPT-style "edit & retry" possible without losing history.

CREATE TABLE conversations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title        text        NOT NULL DEFAULT '',
    owner_id     text        NOT NULL,
    model        text        NOT NULL DEFAULT '',
    current_leaf uuid        NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX conversations_owner_idx ON conversations (owner_id, updated_at DESC);

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid        NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    -- NULL marks a root (system) message. ON DELETE CASCADE would orphan the
    -- subtree, which is what we want when a whole conversation goes away;
    -- deleting one intermediate node is handled in code, never by the DB.
    parent_id       uuid        NULL REFERENCES messages(id) ON DELETE CASCADE,
    role            text        NOT NULL CHECK (role IN ('system','user','assistant','tool')),
    content         text        NOT NULL DEFAULT '',
    tool_call_id    text        NOT NULL DEFAULT '',
    tool_name       text        NOT NULL DEFAULT '',
    tool_calls      jsonb       NOT NULL DEFAULT '[]'::jsonb,
    token_count     integer     NOT NULL DEFAULT 0,
    -- Denormalised depth (root = 0) so listing a thread and rendering
    -- indentation do not each need a recursive walk.
    depth           integer     NOT NULL DEFAULT 0,

    -- Exactly one leaf per conversation may be "active". Kept as a boolean
    -- rather than derived by "no children" so branch switching is a single
    -- indexed UPDATE instead of a tree recompute.
    is_active_leaf  boolean     NOT NULL DEFAULT false,

    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX messages_conversation_idx ON messages (conversation_id, created_at);
CREATE INDEX messages_parent_idx ON messages (parent_id);

-- Partial index for the hot lookup: find the active leaf of a conversation.
CREATE INDEX messages_active_leaf_idx
    ON messages (conversation_id) WHERE is_active_leaf;

-- One active leaf per conversation. Without this, a race between two tabs
-- posting simultaneously can leave two active leaves and silently fork the
-- visible history. The DB is the only place that can enforce this cheaply.
CREATE UNIQUE INDEX messages_one_active_leaf_per_conversation
    ON messages (conversation_id) WHERE is_active_leaf;

-- +goose Down
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS conversations;
