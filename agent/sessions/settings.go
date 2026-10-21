package sessions

import (
	"context"
	"time"

	"terva.sh/airut/agent/sessions/dbq"
)

// SettingChange is one change to a runtime setting. Old and New are the
// values in force before and after, as text; a string setting's value is
// chat text, so they are never logged.
type SettingChange struct {
	Name     string
	Old, New string
	// Unset is true when the change dropped the override.
	Unset bool
	// Who is the changer's id-only scope key, and WhoName their name
	// when they made it.
	Who, WhoName string
	Channel      string // the channel's id-only scope key
	Source       string // command or agent
	At           time.Time
}

// keptSettingChanges is how many changes a setting's log keeps. Older
// ones are deleted as new ones are recorded.
const keptSettingChanges = 100

// SettingOverrides returns every stored override, by setting name.
func (d *DB) SettingOverrides(ctx context.Context) (map[string]string, error) {
	rows, err := d.q.SettingOverrides(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Name] = r.Value
	}
	return out, nil
}

// SaveSetting records c and, in the same transaction, makes c.New the
// override of c.Name, or drops the override when c.Unset.
func (d *DB) SaveSetting(ctx context.Context, c SettingChange) error {
	at := c.At
	if at.IsZero() {
		at = d.now()
	}
	return d.tx(ctx, func(q *dbq.Queries) error {
		var err error
		if c.Unset {
			err = q.DeleteSettingOverride(ctx, c.Name)
		} else {
			err = q.PutSettingOverride(ctx, dbq.PutSettingOverrideParams{Name: c.Name, Value: c.New, Updated: at.UnixMilli()})
		}
		if err != nil {
			return err
		}
		unset := int64(0)
		if c.Unset {
			unset = 1
		}
		if err := q.InsertSettingChange(ctx, dbq.InsertSettingChangeParams{
			Name: c.Name, Old: c.Old, New: c.New, Unset: unset, Who: c.Who, WhoName: c.WhoName,
			Channel: c.Channel, Source: c.Source, At: at.UnixMilli(),
		}); err != nil {
			return err
		}
		return q.TrimSettingChanges(ctx, dbq.TrimSettingChangesParams{Setting: c.Name, Keep: keptSettingChanges})
	})
}

// SettingChanges returns name's latest changes, newest first, at most
// limit of them.
func (d *DB) SettingChanges(ctx context.Context, name string, limit int) ([]SettingChange, error) {
	rows, err := d.q.SettingChanges(ctx, dbq.SettingChangesParams{Name: name, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]SettingChange, 0, len(rows))
	for _, r := range rows {
		out = append(out, SettingChange{
			Name: r.Name, Old: r.Old, New: r.New, Unset: r.Unset == 1, Who: r.Who, WhoName: r.WhoName,
			Channel: r.Channel, Source: r.Source, At: time.UnixMilli(r.At),
		})
	}
	return out, nil
}
