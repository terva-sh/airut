-- Which session produced each of the bot's messages, so a reply to one
-- can resume the conversation it came from. A message is named by its
-- platform, channel, and id, as the core names it. A link goes with its
-- session, when the session is forgotten or expires.
CREATE TABLE links (
    platform   TEXT    NOT NULL,
    channel    TEXT    NOT NULL,
    message    TEXT    NOT NULL,
    session_id INTEGER NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    PRIMARY KEY (platform, channel, message)
);

CREATE INDEX links_session ON links (session_id);

-- The session each person last spoke in, in each channel, so their next
-- message can go on in it: their own conversation, or one they joined by
-- replying or in a thread. person is the core's id-only name for one
-- person in one channel; at is Unix milliseconds. A row goes with its
-- session.
CREATE TABLE follows (
    person     TEXT    PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    at         INTEGER NOT NULL
);

CREATE INDEX follows_session ON follows (session_id);
