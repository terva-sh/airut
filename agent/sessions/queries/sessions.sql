-- name: OpenSession :one
INSERT INTO sessions (key, created_at, last_active) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET last_active = excluded.last_active
RETURNING id;

-- name: TouchSession :execrows
UPDATE sessions SET last_active = ? WHERE id = ?;

-- name: NextSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) + 1 AS INTEGER) AS next FROM entries WHERE session_id = ?;

-- name: InsertEntry :exec
INSERT INTO entries (session_id, seq, kind, body) VALUES (?, ?, ?, ?);

-- name: ListEntries :many
SELECT kind, body FROM entries WHERE session_id = ? ORDER BY seq;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE key = ?;

-- name: DeleteIdleSessions :execrows
DELETE FROM sessions WHERE last_active < ?;
