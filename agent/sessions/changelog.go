package sessions

import (
	"context"
	"database/sql"
	"errors"

	"terva.sh/airut/agent/sessions/dbq"
)

// AnnouncedVersion returns the changelog version last recorded for room,
// a channel's id-only scope key, or "" when there is none.
func (d *DB) AnnouncedVersion(ctx context.Context, room string) (string, error) {
	v, err := d.q.AnnouncedVersion(ctx, room)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SaveAnnouncedVersion records version as the changelog version last
// announced in room, replacing the record there was.
func (d *DB) SaveAnnouncedVersion(ctx context.Context, room, version string) error {
	return d.q.PutAnnouncedVersion(ctx, dbq.PutAnnouncedVersionParams{Room: room, Version: version, Updated: d.now().UnixMilli()})
}
