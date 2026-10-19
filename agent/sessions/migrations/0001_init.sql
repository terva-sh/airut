-- The migrations this database has had, which the runner checks against
-- the ones embedded in the binary.
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
);

-- One conversation an agent remembers, named by the core's session key.
-- Times are Unix milliseconds. AUTOINCREMENT keeps a forgotten session's
-- id from being reused, so a handle opened before the forget can never
-- write into the session that replaces it.
CREATE TABLE sessions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    key         TEXT    NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,
    last_active INTEGER NOT NULL
);

CREATE INDEX sessions_last_active ON sessions (last_active);

-- A session's log: every write the engine made, in order. Reload replays
-- it, so kind names a TranscriptStore method and body is its argument.
CREATE TABLE entries (
    session_id INTEGER NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    body       BLOB    NOT NULL,
    PRIMARY KEY (session_id, seq)
);
