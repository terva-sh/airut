-- name: InsertMemory :one
INSERT INTO memories (scope, scope_key, key, text, source, author, author_name, status, created, last_used)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetMemory :one
SELECT * FROM memories WHERE id = ?;

-- name: ScopeMemories :many
SELECT * FROM memories WHERE scope = ? AND scope_key = ? ORDER BY created, id;

-- name: ReplaceMemory :one
UPDATE memories
SET text = ?, source = ?, author = ?, author_name = ?, status = ?, created = ?, last_used = ?
WHERE id = ?
RETURNING *;

-- name: SetMemoryStatus :execrows
UPDATE memories SET status = ? WHERE id = ?;

-- name: TouchMemory :exec
UPDATE memories SET last_used = ? WHERE id = ?;

-- name: DeleteMemory :exec
DELETE FROM memories WHERE id = ?;

-- name: DeleteIdleUserMemories :execrows
DELETE FROM memories WHERE scope = 'user' AND last_used < ?;

-- A memory is removed only as it was read: a write that replaced it, or
-- an approval that changed its status, leaves it in place.
-- name: DeleteMemoryAsRead :execrows
DELETE FROM memories
WHERE id = ? AND status = ? AND author = ? AND text = ? AND created = ?;
