-- Runtime settings. setting_overrides holds the value set from chat for
-- each setting that has one, as the text the bot parses. setting_changes
-- is the change log: old and new are the values in force before and
-- after, as text, which can be chat text and are never logged. unset is 1
-- when the change dropped the override. who is the changer's id-only
-- scope key and who_name their name then; channel is the channel's
-- id-only scope key. source is command or agent. Times are Unix
-- milliseconds.
CREATE TABLE setting_overrides (
    name    TEXT    PRIMARY KEY,
    value   TEXT    NOT NULL,
    updated INTEGER NOT NULL
);

CREATE TABLE setting_changes (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    name     TEXT    NOT NULL,
    old      TEXT    NOT NULL,
    new      TEXT    NOT NULL,
    unset    INTEGER NOT NULL CHECK (unset IN (0, 1)),
    who      TEXT    NOT NULL,
    who_name TEXT    NOT NULL,
    channel  TEXT    NOT NULL,
    source   TEXT    NOT NULL CHECK (source IN ('command', 'agent')),
    at       INTEGER NOT NULL
);

CREATE INDEX setting_changes_name ON setting_changes (name, id);
