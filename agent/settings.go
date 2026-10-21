package agent

import (
	"context"
	"strings"

	"terva.sh/airut"
	"terva.sh/airut/agent/sessions"
)

var _ airut.SettingStore = (*Agent)(nil)

// SettingOverrides implements airut.SettingStore.
func (a *Agent) SettingOverrides(ctx context.Context) (map[string]string, error) {
	return a.db.SettingOverrides(ctx)
}

// SaveSetting implements airut.SettingStore.
func (a *Agent) SaveSetting(ctx context.Context, ch airut.SettingChange) error {
	return a.db.SaveSetting(ctx, sessions.SettingChange{
		Name: ch.Name, Old: ch.Old, New: ch.New, Unset: ch.Unset, Who: airut.UserScope(ch.By), WhoName: ch.By.Name,
		Channel: airut.ChannelScope(ch.Channel), Source: string(ch.Source), At: ch.At,
	})
}

// SettingChanges implements airut.SettingStore.
func (a *Agent) SettingChanges(ctx context.Context, name string, limit int) ([]airut.SettingChange, error) {
	rs, err := a.db.SettingChanges(ctx, name, limit)
	if err != nil {
		return nil, err
	}
	out := make([]airut.SettingChange, 0, len(rs))
	for _, r := range rs {
		by := airut.User{Name: r.WhoName}
		if p, id, ok := strings.Cut(r.Who, "/user/"); ok {
			by.Platform, by.ID = airut.Platform(p), id
		}
		var ch airut.Channel
		if p, id, ok := strings.Cut(r.Channel, "/channel/"); ok {
			ch = airut.Channel{Platform: airut.Platform(p), ID: id}
		}
		out = append(out, airut.SettingChange{
			Name: r.Name, Old: r.Old, New: r.New, Unset: r.Unset, By: by, Channel: ch,
			Source: airut.SettingSource(r.Source), At: r.At,
		})
	}
	return out, nil
}
