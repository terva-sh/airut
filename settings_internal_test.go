package airut

import (
	"context"
	"testing"
)

// nullStore keeps nothing and refuses nothing.
type nullStore struct{}

func (nullStore) SettingOverrides(context.Context) (map[string]string, error) { return nil, nil }
func (nullStore) SaveSetting(context.Context, SettingChange) error            { return nil }
func (nullStore) SettingChanges(context.Context, string, int) ([]SettingChange, error) {
	return nil, nil
}

func TestChimeTurnsCannotChangeSettings(t *testing.T) {
	b := New(Config{Settings: Settings{Store: nullStore{}, Owner: func(*Ctx) (bool, error) { return true, nil }}})
	quiet := b.DeclareBool(BoolSetting{Name: "quiet", Description: "Answer less"})
	set := setCommand(b)
	value := "true"
	for name, c := range map[string]*Ctx{
		"the handler": {ctx: context.Background(), bot: b, chime: &chime{}},
		// A tool its agent turn calls carries the mark on, through Call.
		"a tool it calls": {ctx: context.Background(), bot: b, chimed: (&Ctx{chime: &chime{}}).unasked()},
	} {
		err := b.changeSetting(c, quiet.s, &value)
		if err == nil || err.Error() != "A setting is changed only for someone who asked for it." {
			t.Errorf("%s: %v", name, err)
		}
	}
	parent := &Ctx{ctx: context.Background(), bot: b, chime: &chime{}}
	if _, err := set.Call(parent, []byte(`{"name": "quiet", "value": "true"}`)); err == nil {
		t.Error("set called from a Chime turn changed the setting")
	}
	if quiet.Get() {
		t.Error("quiet changed")
	}
}
