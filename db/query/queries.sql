-- sqlc input. Generated Go lives in internal/store/sqlcgen.
-- Run: sqlc generate

-- name: CreateConversation :one
INSERT INTO conversations (id, title, owner_id, model)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetConversation :one
SELECT * FROM conversations WHERE id = $1;

-- name: ListConversations :many
SELECT * FROM conversations
WHERE owner_id = $1
ORDER BY updated_at DESC
LIMIT $2 OFFSET $3;

-- name: TouchConversation :exec
UPDATE conversations
SET updated_at = now(), title = COALESCE(NULLIF($2, ''), title)
WHERE id = $1;

-- name: SetCurrentLeaf :exec
-- Clears the previous leaf then sets the new one; wrapped in a transaction by
-- the service so the partial unique index is never momentarily violated.
UPDATE messages SET is_active_leaf = false
WHERE conversation_id = $1 AND is_active_leaf;

-- name: MarkActiveLeaf :exec
UPDATE messages SET is_active_leaf = true WHERE id = $1;

-- name: InsertMessage :one
INSERT INTO messages (
    id, conversation_id, parent_id, role, content,
    tool_call_id, tool_name, tool_calls, token_count, depth, is_active_leaf
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
RETURNING *;

-- name: CountChildren :one
SELECT count(*) FROM messages WHERE parent_id = $1;

-- name: MaxDepthUnder :one
SELECT COALESCE(max(depth), -1)::integer AS max_depth
FROM messages WHERE conversation_id = $1;

-- name: GetBranch :many
-- THE core query. Walks from a leaf back to its root and returns that single
-- path, oldest first.
--
-- A conversation may hold many sibling subtrees after edits; "the conversation"
-- the user sees is exactly one root-to-leaf path. Doing this in application code
-- means either loading every message and rebuilding the tree in memory (O(all
-- messages), and it gets worse with every edit) or N sequential queries for each
-- parent hop. The recursive CTE does it in one round trip.
WITH RECURSIVE ancestry AS (
    SELECT m.*
    FROM messages m
    WHERE m.id = sqlc.arg(leaf_id)::uuid

    UNION ALL

    SELECT p.*
    FROM messages p
    JOIN ancestry a ON p.id = a.parent_id
)
SELECT * FROM ancestry
ORDER BY depth ASC, created_at ASC;

-- name: GetThreadForLastMessage :many
-- Dify-style variant: given the newest message, return only the messages that
-- belong to that thread rather than the whole conversation. Kept separate from
-- GetBranch because the two are used at different points in the request flow.
WITH RECURSIVE ancestry AS (
    SELECT m.* FROM messages m WHERE m.id = sqlc.arg(leaf_id)::uuid
    UNION ALL
    SELECT p.* FROM messages p JOIN ancestry a ON p.id = a.parent_id
)
SELECT * FROM ancestry ORDER BY depth ASC;

-- name: GetActiveLeafID :one
SELECT id FROM messages
WHERE conversation_id = $1 AND is_active_leaf
LIMIT 1;

-- name: GetChildren :many
SELECT * FROM messages
WHERE parent_id = $1
ORDER BY created_at ASC;

-- name: GetTree :many
-- Full tree for the sidebar branch picker. Deliberately not recursive: the
-- handler builds the tree in memory since it needs every node anyway.
SELECT * FROM messages
WHERE conversation_id = $1
ORDER BY depth ASC, created_at ASC;

-- name: UpdateMessageContent :one
-- Editing a message does NOT delete its descendants: the old answer stays as a
-- sibling subtree so the user can switch back. Reparenting happens in the
-- service when a regenerated answer is attached.
UPDATE messages SET content = $2, token_count = $3
WHERE id = $1
RETURNING *;

-- name: DeleteConversation :exec
DELETE FROM conversations WHERE id = $1;
