-- name: ListMigrations :many
SELECT version, name, checksum FROM schema_migrations ORDER BY version;

-- name: InsertMigration :exec
INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?);
