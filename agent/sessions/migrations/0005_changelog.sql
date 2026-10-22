-- Changelog announcements. changelog_announcements holds, for each room a
-- bot announces its changelog in, the last version recorded there, in
-- canonical form, such as v1.2.0. room is the channel's id-only scope
-- key. updated is Unix milliseconds.
CREATE TABLE changelog_announcements (
    room    TEXT    PRIMARY KEY,
    version TEXT    NOT NULL,
    updated INTEGER NOT NULL
);
