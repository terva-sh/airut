-- name: AnnouncedVersion :one
SELECT version FROM changelog_announcements WHERE room = ?;

-- name: PutAnnouncedVersion :exec
INSERT INTO changelog_announcements (room, version, updated) VALUES (?, ?, ?)
ON CONFLICT (room) DO UPDATE SET version = excluded.version, updated = excluded.updated;
