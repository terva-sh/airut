package sessions

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAnnouncedVersionsSurviveAReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	d := open(t, path, Options{Now: newClock().now})
	if v, err := d.AnnouncedVersion(ctx, "test/channel/a"); err != nil || v != "" {
		t.Fatalf("a room with no record = %q, %v; want \"\"", v, err)
	}
	for _, s := range []struct{ room, version string }{
		{"test/channel/a", "v1.2.0"}, {"test/channel/b", "v1.3.0"}, {"test/channel/a", "v1.4.0"},
	} {
		if err := d.SaveAnnouncedVersion(ctx, s.room, s.version); err != nil {
			t.Fatalf("SaveAnnouncedVersion(%q, %q): %v", s.room, s.version, err)
		}
	}
	d.Close()

	d = open(t, path, Options{Now: newClock().now})
	for room, want := range map[string]string{"test/channel/a": "v1.4.0", "test/channel/b": "v1.3.0", "other/channel/a": ""} {
		if v, err := d.AnnouncedVersion(ctx, room); err != nil || v != want {
			t.Errorf("after a reopen, %s = %q, %v; want %q", room, v, err, want)
		}
	}
}
