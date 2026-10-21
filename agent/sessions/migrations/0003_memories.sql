-- Long-term memories. scope is user, channel, or bot, and scope_key names
-- whom or where by ids only, empty for the bot. key is a short name for
-- what a memory is about; text is what it says, written from chat, so it
-- is never logged. source is user, model, or admin, author is the
-- writer's id-only scope key and author_name their name when they wrote
-- it. status is active, pending, or archived. Times are Unix
-- milliseconds. AUTOINCREMENT keeps a removed memory's id from naming a
-- later one, since people remove memories by id.
CREATE TABLE memories (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope       TEXT    NOT NULL CHECK (scope IN ('user', 'channel', 'bot')),
    scope_key   TEXT    NOT NULL,
    key         TEXT    NOT NULL,
    text        TEXT    NOT NULL,
    source      TEXT    NOT NULL CHECK (source IN ('user', 'model', 'admin')),
    author      TEXT    NOT NULL,
    author_name TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('active', 'pending', 'archived')),
    created     INTEGER NOT NULL,
    last_used   INTEGER NOT NULL
);

CREATE INDEX memories_scope ON memories (scope, scope_key, status);
