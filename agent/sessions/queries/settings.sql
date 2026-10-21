-- name: SettingOverrides :many
SELECT name, value FROM setting_overrides ORDER BY name;

-- name: PutSettingOverride :exec
INSERT INTO setting_overrides (name, value, updated) VALUES (?, ?, ?)
ON CONFLICT (name) DO UPDATE SET value = excluded.value, updated = excluded.updated;

-- name: DeleteSettingOverride :exec
DELETE FROM setting_overrides WHERE name = ?;

-- name: InsertSettingChange :exec
INSERT INTO setting_changes (name, old, new, unset, who, who_name, channel, source, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: SettingChanges :many
SELECT * FROM setting_changes WHERE name = ? ORDER BY id DESC LIMIT ?;

-- A setting keeps only its latest changes, so its log stays bounded.
-- name: TrimSettingChanges :exec
DELETE FROM setting_changes
WHERE setting_changes.name = sqlc.arg(setting) AND setting_changes.id NOT IN (
    SELECT kept.id FROM setting_changes AS kept
    WHERE kept.name = sqlc.arg(setting) ORDER BY kept.id DESC LIMIT sqlc.arg(keep)
);
