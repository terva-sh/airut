package sessions

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"terva.sh/terva/packages/core"
	"terva.sh/terva/packages/core/transcripttest"
	"terva.sh/terva/packages/provider"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)} }

func open(t *testing.T, path string, opts Options) *DB {
	t.Helper()
	d, err := Open(context.Background(), path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func session(t *testing.T, d *DB, key string) *Session {
	t.Helper()
	s, err := d.Session(context.Background(), key)
	if err != nil {
		t.Fatalf("Session(%q): %v", key, err)
	}
	return s
}

func text(role provider.Role, s string) provider.Message {
	return provider.Message{Role: role, Content: []provider.Content{provider.TextBlock{Text: s}}}
}

func TestTranscriptSuite(t *testing.T) {
	transcripttest.Run(t, func(t *testing.T) transcripttest.Store {
		d := open(t, filepath.Join(t.TempDir(), "sessions.db"), Options{})
		s := session(t, d, "discord:guild/channel/user")
		return transcripttest.Store{TranscriptStore: s, Reload: s.Reload}
	})
}

// TestTranscriptSuiteAcrossReopen runs the suite with each reload going
// through a fresh DB on the same file, the way a restarted bot reads it.
func TestTranscriptSuiteAcrossReopen(t *testing.T) {
	transcripttest.Run(t, func(t *testing.T) transcripttest.Store {
		path := filepath.Join(t.TempDir(), "sessions.db")
		s := session(t, open(t, path, Options{}), "k")
		return transcripttest.Store{TranscriptStore: s, Reload: func() (core.Transcript, error) {
			d, err := Open(context.Background(), path, Options{})
			if err != nil {
				return core.Transcript{}, err
			}
			defer d.Close()
			fresh, err := d.Session(context.Background(), "k")
			if err != nil {
				return core.Transcript{}, err
			}
			return fresh.Reload()
		}}
	})
}

func TestSessionsAreSeparate(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "sessions.db"), Options{})
	a, b := session(t, d, "a"), session(t, d, "b")
	if err := a.AppendMessage(text(provider.RoleUser, "for a")); err != nil {
		t.Fatal(err)
	}
	if tr, err := b.Reload(); err != nil || len(tr.Messages) != 0 {
		t.Fatalf("b reloaded %d messages (%v); a's write leaked into it", len(tr.Messages), err)
	}
	// Opening a again returns the same log.
	if tr, err := session(t, d, "a").Reload(); err != nil || len(tr.Messages) != 1 {
		t.Fatalf("a reopened with %d messages (%v), want 1", len(tr.Messages), err)
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	d, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := session(t, d, "k")
	want := []provider.Message{text(provider.RoleUser, "hello"), text(provider.RoleAssistant, "hi")}
	for _, m := range want {
		if err := s.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(text(provider.RoleUser, "late")); err == nil {
		t.Fatal("a write after Close succeeded")
	}

	tr, err := session(t, open(t, path, Options{}), "k").Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tr.Messages, want) {
		t.Fatalf("after a restart: %#v, want %#v", tr.Messages, want)
	}
}

func TestIdleSessionsExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	c := newClock()
	d := open(t, path, Options{Retention: time.Hour, Now: c.now})
	stale, live := session(t, d, "stale"), session(t, d, "live")
	for _, s := range []*Session{stale, live} {
		if err := s.AppendMessage(text(provider.RoleUser, "x")); err != nil {
			t.Fatal(err)
		}
	}

	c.advance(40 * time.Minute)
	if err := live.AppendMessage(text(provider.RoleUser, "still here")); err != nil {
		t.Fatal(err)
	}
	c.advance(30 * time.Minute)
	if n, err := d.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1 deleted", n, err)
	}
	if err := stale.AppendMessage(text(provider.RoleUser, "y")); !errors.Is(err, ErrForgotten) {
		t.Fatalf("a write to an expired session: %v, want ErrForgotten", err)
	}
	if tr, err := live.Reload(); err != nil || len(tr.Messages) != 2 {
		t.Fatalf("the live session reloaded %d messages (%v), want 2", len(tr.Messages), err)
	}

	// Open sweeps too, so a bot that was down past the retention starts
	// without the sessions that expired meanwhile.
	d.Close()
	c.advance(2 * time.Hour)
	d = open(t, path, Options{Retention: time.Hour, Now: c.now})
	if n, err := d.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("Sweep after reopening = %d, %v; Open should have swept already", n, err)
	}
	if tr, err := session(t, d, "live").Reload(); err != nil || len(tr.Messages) != 0 {
		t.Fatalf("an expired session came back with %d messages (%v)", len(tr.Messages), err)
	}
}

func TestForgetPrefix(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "sessions.db"), Options{})
	ctx := context.Background()
	keys := []string{"p/user/a/", "p/user/a/channel/1", "p/user/a/channel/2", "p/user/ab/channel/1", "p/user/_/x", "p/user/a"}
	for _, k := range keys {
		if err := session(t, d, k).AppendMessage(text(provider.RoleUser, "hi")); err != nil {
			t.Fatal(err)
		}
	}
	// LIKE would read _ and % as wildcards; the prefix is matched as
	// written.
	if err := d.ForgetPrefix(ctx, "p/user/%"); err != nil {
		t.Fatal(err)
	}
	if err := d.ForgetPrefix(ctx, "p/user/_/"); err != nil {
		t.Fatal(err)
	}
	if err := d.ForgetPrefix(ctx, "p/user/a/"); err != nil {
		t.Fatal(err)
	}
	if err := d.ForgetPrefix(ctx, ""); err == nil {
		t.Fatal("an empty prefix, which matches every session, was accepted")
	}
	var left []string
	for _, k := range keys {
		if tr, err := session(t, d, k).Reload(); err != nil {
			t.Fatal(err)
		} else if len(tr.Messages) > 0 {
			left = append(left, k)
		}
	}
	if want := []string{"p/user/ab/channel/1", "p/user/a"}; !slices.Equal(left, want) {
		t.Fatalf("left %q, want %q", left, want)
	}
}

func TestForget(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "sessions.db"), Options{})
	old := session(t, d, "k")
	if err := old.AppendMessage(text(provider.RoleUser, "remember this")); err != nil {
		t.Fatal(err)
	}
	if err := d.Forget(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	if err := d.Forget(context.Background(), "k"); err != nil {
		t.Fatalf("forgetting a session twice: %v", err)
	}
	if err := old.AppendMessage(text(provider.RoleUser, "more")); !errors.Is(err, ErrForgotten) {
		t.Fatalf("a write to a forgotten session: %v, want ErrForgotten", err)
	}

	// The key starts afresh, and the old handle cannot reach the new log
	// even though the old row was the newest.
	fresh := session(t, d, "k")
	if tr, err := fresh.Reload(); err != nil || len(tr.Messages) != 0 {
		t.Fatalf("a forgotten session came back with %d messages (%v)", len(tr.Messages), err)
	}
	if err := old.AppendMessage(text(provider.RoleUser, "stray")); !errors.Is(err, ErrForgotten) {
		t.Fatalf("the old handle wrote after the key was reopened: %v", err)
	}
	if tr, _ := fresh.Reload(); len(tr.Messages) != 0 {
		t.Fatalf("the old handle's write reached the new session")
	}

	// The log went with the session.
	var n int
	if err := d.db.QueryRow(`SELECT count(*) FROM entries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d log entries outlived their session (%v)", n, err)
	}
}

func TestOpenRefusesNegativeRetention(t *testing.T) {
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "s.db"), Options{Retention: -time.Second}); err == nil {
		t.Fatal("Open accepted a negative retention")
	}
}

func TestMigrations(t *testing.T) {
	ms, err := loadMigrations(migrations)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sessions.db")
	open(t, path, Options{}).Close()
	// Reopening applies nothing and checks what was applied.
	open(t, path, Options{}).Close()

	raw := func(t *testing.T, stmt string, args ...any) {
		t.Helper()
		db, err := openPool(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(stmt, args...); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an edited migration is refused", func(t *testing.T) {
		raw(t, `UPDATE schema_migrations SET checksum = 'edited' WHERE version = 1`)
		t.Cleanup(func() { raw(t, `UPDATE schema_migrations SET checksum = ? WHERE version = 1`, ms[0].checksum) })
		if _, err := Open(context.Background(), path, Options{}); err == nil {
			t.Fatal("Open accepted a database whose migration differs from this build's")
		}
	})
	t.Run("a newer schema is refused", func(t *testing.T) {
		raw(t, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, 'future', 'x', 0)`, len(ms)+1)
		if _, err := Open(context.Background(), path, Options{}); !errors.Is(err, ErrSchemaNewer) {
			t.Fatalf("Open on a newer schema: %v, want ErrSchemaNewer", err)
		}
	})
}

func TestPragmas(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "sessions.db"), Options{})
	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"foreign_keys": "1",
		"synchronous":  "2",
		"busy_timeout": "5000",
	} {
		var got sql.NullString
		if err := d.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got.String != want {
			t.Errorf("PRAGMA %s = %q (%v), want %q", pragma, got.String, err, want)
		}
	}
}

func TestLinks(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	d := open(t, filepath.Join(t.TempDir(), "sessions.db"), Options{Retention: time.Hour, Now: c.now})
	m := Message{Platform: "matrix", Channel: "!room", ID: "$ev"}
	if _, ok, err := d.Linked(ctx, m); ok || err != nil {
		t.Fatalf("an unlinked message = %v, %v", ok, err)
	}
	// A link to a session that does not exist is not made.
	if err := d.Link(ctx, m, "nobody"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.Linked(ctx, m); ok {
		t.Fatal("a message was linked to a session that does not exist")
	}
	session(t, d, "a")
	session(t, d, "b")
	if err := d.Link(ctx, m, "a"); err != nil {
		t.Fatal(err)
	}
	if key, ok, err := d.Linked(ctx, m); key != "a" || !ok || err != nil {
		t.Fatalf("Linked = %q, %v, %v", key, ok, err)
	}
	// The same message in another channel or platform is another message.
	for _, other := range []Message{{"matrix", "!other", "$ev"}, {"discord", "!room", "$ev"}} {
		if _, ok, _ := d.Linked(ctx, other); ok {
			t.Errorf("%+v is linked", other)
		}
	}
	// A link can be replaced, and goes with its session.
	if err := d.Link(ctx, m, "b"); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := d.Linked(ctx, m); key != "b" {
		t.Fatalf("the replaced link names %q", key)
	}
	if err := d.Forget(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.Linked(ctx, m); ok {
		t.Fatal("a link outlived its forgotten session")
	}
	// A recreated session of the same key is a new one, which an old
	// link never names.
	session(t, d, "b")
	if _, ok, _ := d.Linked(ctx, m); ok {
		t.Fatal("a link came back with a new session under the old key")
	}
	if err := d.Link(ctx, m, "a"); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Hour)
	if _, err := d.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.Linked(ctx, m); ok {
		t.Fatal("a link outlived its expired session")
	}
}

func TestFollows(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	d := open(t, filepath.Join(t.TempDir(), "sessions.db"), Options{Now: c.now})
	if _, _, ok, err := d.Followed(ctx, "u/c"); ok || err != nil {
		t.Fatalf("Followed of nobody = %v, %v", ok, err)
	}
	if err := d.Follow(ctx, "u/c", "nothing"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := d.Followed(ctx, "u/c"); ok {
		t.Fatal("a person followed a session that does not exist")
	}
	session(t, d, "mine")
	session(t, d, "shared")
	if err := d.Follow(ctx, "u/c", "mine"); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if err := d.Follow(ctx, "u/c", "shared"); err != nil {
		t.Fatal(err)
	}
	if key, at, ok, err := d.Followed(ctx, "u/c"); key != "shared" || !ok || err != nil || !at.Equal(c.now().Truncate(time.Millisecond)) {
		t.Fatalf("Followed = %q, %v, %v, %v", key, at, ok, err)
	}
	if _, _, ok, _ := d.Followed(ctx, "v/c"); ok {
		t.Fatal("another person follows the same")
	}
	// The record goes with its session.
	if err := d.Forget(ctx, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := d.Followed(ctx, "u/c"); ok {
		t.Fatal("a person follows a forgotten session")
	}
}
