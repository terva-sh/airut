-- Owner claims. owner_claims holds each owner recorded with airut claim
-- on the bot's host, beside the owners its config file lists. platform is
-- the platform's name, such as matrix or discord, and user_id the
-- owner's id there. claimed_at is Unix milliseconds. actor is free text
-- naming who ran the command, as the command was told or found it.
CREATE TABLE owner_claims (
    platform   TEXT    NOT NULL,
    user_id    TEXT    NOT NULL,
    claimed_at INTEGER NOT NULL,
    actor      TEXT    NOT NULL,
    PRIMARY KEY (platform, user_id)
);
