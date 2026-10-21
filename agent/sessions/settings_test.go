package sessions

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsSurviveAReopen(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	path := filepath.Join(t.TempDir(), "s.db")
	d := open(t, path, Options{Now: c.now})
	for _, ch := range []SettingChange{
		{Name: "quiet", Old: "false", New: "true", Who: "test/user/owner", WhoName: "Olive", Channel: "test/channel/c", Source: "command"},
		{Name: "size", Old: "1", New: "4", Who: "test/user/owner", WhoName: "Olive", Channel: "test/channel/c", Source: "agent"},
		{Name: "size", Old: "4", New: "6", Who: "test/user/mod", WhoName: "Mod", Channel: "test/channel/c", Source: "command"},
		{Name: "quiet", Old: "true", New: "false", Unset: true, Who: "test/user/owner", WhoName: "Olive", Channel: "test/channel/c", Source: "command"},
	} {
		c.advance(time.Minute)
		if err := d.SaveSetting(ctx, ch); err != nil {
			t.Fatalf("SaveSetting(%+v): %v", ch, err)
		}
	}
	d.Close()

	d = open(t, path, Options{Now: c.now})
	got, err := d.SettingOverrides(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"size": "6"}; !maps.Equal(got, want) {
		t.Errorf("overrides after a reopen = %v, want %v, since quiet's was dropped", got, want)
	}
	changes, err := d.SettingChanges(ctx, "size", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []SettingChange{
		{Name: "size", Old: "4", New: "6", Who: "test/user/mod", WhoName: "Mod", Channel: "test/channel/c", Source: "command",
			At: time.Date(2026, 10, 18, 12, 3, 0, 0, time.UTC)},
		{Name: "size", Old: "1", New: "4", Who: "test/user/owner", WhoName: "Olive", Channel: "test/channel/c", Source: "agent",
			At: time.Date(2026, 10, 18, 12, 2, 0, 0, time.UTC)},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v", changes)
	}
	for i := range want {
		if !changes[i].At.Equal(want[i].At) {
			t.Errorf("change %d at %v, want %v", i, changes[i].At, want[i].At)
		}
		changes[i].At = want[i].At
		if changes[i] != want[i] {
			t.Errorf("change %d = %+v, want %+v", i, changes[i], want[i])
		}
	}
	if q, _ := d.SettingChanges(ctx, "quiet", 1); len(q) != 1 || !q[0].Unset || q[0].New != "false" {
		t.Errorf("quiet's latest change = %+v, want the unset", q)
	}
}

func TestSettingChangesStayBounded(t *testing.T) {
	ctx := context.Background()
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: newClock().now})
	for i := range keptSettingChanges + 5 {
		for _, name := range []string{"a", "b"} {
			ch := SettingChange{Name: name, Old: fmt.Sprint(i), New: fmt.Sprint(i + 1), Who: "w", Channel: "c", Source: "command"}
			if err := d.SaveSetting(ctx, ch); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"a", "b"} {
		all, err := d.SettingChanges(ctx, name, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != keptSettingChanges || all[0].New != fmt.Sprint(keptSettingChanges+5) {
			t.Errorf("%s keeps %d changes, the newest %+v; want the latest %d", name, len(all), all[0], keptSettingChanges)
		}
	}
}
