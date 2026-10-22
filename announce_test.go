package airut_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

const updateNotes = `# Changelog

## [Unreleased]

- Not out yet.

## [1.5.0] - 2026-10-22

- The archive keeps your place.

## [1.5.0-rc.1] - 2026-10-20

- Trying out bookmarks.

## [1.4.0] - 2026-10-01

- Searches are faster.

## [1.3.0] - 2026-09-01

- Pictures load lazily.

## [1.2.0] - 2026-08-01

- First release.
`

// memStore is a VersionStore in memory whose saves can be made to fail.
type memStore struct {
	mu       sync.Mutex
	rooms    map[airut.Channel]string
	failSave int // how many saves still to fail
}

func newMemStore(recs map[airut.Channel]string) *memStore {
	return &memStore{rooms: copyRecs(recs)}
}

func copyRecs(m map[airut.Channel]string) map[airut.Channel]string {
	out := map[airut.Channel]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *memStore) AnnouncedVersion(_ context.Context, ch airut.Channel) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rooms[ch], nil
}

func (s *memStore) SaveAnnouncedVersion(_ context.Context, ch airut.Channel, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave > 0 {
		s.failSave--
		return errors.New("store down")
	}
	s.rooms[ch] = v
	return nil
}

func (s *memStore) snapshot() map[airut.Channel]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyRecs(s.rooms)
}

// keyAdapter is the fake adapter with a Conn that notes each send's
// Key, and a way to announce the Conn again, as after a reconnect.
type keyAdapter struct {
	*airuttest.Adapter
	mu   sync.Mutex
	host airut.Host
	ctx  context.Context
	keys []string
}

func (k *keyAdapter) Run(ctx context.Context, h airut.Host) error {
	k.mu.Lock()
	k.host, k.ctx = h, ctx
	k.mu.Unlock()
	return k.Adapter.Run(ctx, keyHost{h, k})
}

func (k *keyAdapter) reconnect() {
	k.mu.Lock()
	h, ctx := k.host, k.ctx
	k.mu.Unlock()
	h.Ready(ctx, keyConn{k.Adapter.Conn(), k})
}

func (k *keyAdapter) sentKeys() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.keys)
}

type keyHost struct {
	airut.Host
	k *keyAdapter
}

func (h keyHost) Ready(ctx context.Context, c airut.Conn) { h.Host.Ready(ctx, keyConn{c, h.k}) }

type keyConn struct {
	airut.Conn
	k *keyAdapter
}

func (c keyConn) Send(ctx context.Context, ch airut.Channel, m airut.Message) (airut.MessageRef, error) {
	c.k.mu.Lock()
	c.k.keys = append(c.k.keys, m.Key)
	c.k.mu.Unlock()
	return c.Conn.Send(ctx, ch, m)
}

// announced is a running bot and what its announcements did.
type announced struct {
	t     *testing.T
	b     *airut.Bot
	a     *keyAdapter
	logs  *lockedBuffer
	store *memStore
	// listed is how many of the channels are on the fake platform, which
	// is how many outcomes each connect adds.
	listed int
	seen   int
}

// announceRun runs a bot with cfg's changelog announced in channels from
// store's records, and waits for the first connect's announcements.
func announceRun(t *testing.T, cfg airut.Config, channels []airut.Channel, store *memStore) *announced {
	t.Helper()
	if cfg.Changelog == nil {
		cfg.Changelog = &airut.Changelog{Notes: updateNotes, Heading: "📚 New in the archive ({version}):"}
	}
	cfg.Changelog.Channels, cfg.Changelog.Store = channels, store
	b, logs := newBot(cfg)
	r := &announced{t: t, b: b, a: &keyAdapter{Adapter: airuttest.New("")}, logs: logs, store: store}
	for _, ch := range channels {
		if ch.Platform == airuttest.Platform {
			r.listed++
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, r.a) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	select {
	case <-r.a.Ready():
	case err := <-done:
		t.Fatalf("Run returned before serving: %v", err)
	}
	r.wait()
	return r
}

// wait waits for one connect's outcomes.
func (r *announced) wait() {
	r.t.Helper()
	r.seen += r.listed
	deadline := time.Now().Add(5 * time.Second)
	for r.total() < r.seen {
		if time.Now().After(deadline) {
			r.t.Fatalf("announcements recorded %d outcomes, want %d", r.total(), r.seen)
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *announced) reconnect() {
	r.t.Helper()
	r.a.reconnect()
	r.wait()
}

var announceOutcomes = []string{"posted", "send_failed", "record_failed", "store_failed", "first_run", "no_section", "not_release", "not_newer"}

func (r *announced) total() int {
	n := 0
	for _, o := range announceOutcomes {
		n += r.count(o)
	}
	return n
}

func (r *announced) count(outcome string) int {
	s, _ := r.b.Metrics().Get("airut_changelog_posts_total", "platform", string(airuttest.Platform), "outcome", outcome)
	return int(s.Value)
}

func (r *announced) sends() []airuttest.Event {
	var out []airuttest.Event
	for _, e := range r.a.Events() {
		if e.Kind == "send" {
			out = append(out, e)
		}
	}
	return out
}

func (r *announced) wantRecords(want map[airut.Channel]string) {
	r.t.Helper()
	got := r.store.snapshot()
	for ch, v := range want {
		if got[ch] != v {
			r.t.Errorf("record for %s = %q, want %q", ch.ID, got[ch], v)
		}
	}
	for ch, v := range got {
		if _, ok := want[ch]; !ok {
			r.t.Errorf("record for %s = %q, want none", ch.ID, v)
		}
	}
}

func (r *announced) wantNoNotesLogged() {
	r.t.Helper()
	for _, s := range []string{"keeps your place", "bookmarks", "Searches are faster", "Pictures load", "First release"} {
		if strings.Contains(r.logs.String(), s) {
			r.t.Errorf("the log holds the notes text %q:\n%s", s, r.logs)
		}
	}
}

func room(id string) airut.Channel { return airut.Channel{Platform: airuttest.Platform, ID: id} }

func TestChangelogAnnouncedAfterAnUpdate(t *testing.T) {
	jump, step, same, rollback, fresh := room("jump"), room("step"), room("same"), room("rollback"), room("fresh")
	elsewhere := airut.Channel{Platform: airut.Discord, ID: "jump"}
	recs := map[airut.Channel]string{jump: "v1.2.0", step: "v1.5.0-rc.1", same: "v1.5.0", rollback: "v1.6.0", elsewhere: "v1.2.0"}
	r := announceRun(t, airut.Config{Version: "v1.5.0"}, []airut.Channel{jump, step, same, rollback, fresh, elsewhere}, newMemStore(recs))

	sends := r.sends()
	byRoom := map[string]airuttest.Event{}
	for _, e := range sends {
		byRoom[e.Channel] = e
	}
	if len(sends) != 2 || len(byRoom) != 2 {
		t.Fatalf("sends = %+v, want one to jump and one to step", sends)
	}
	// A jump posts every release's section since the record, newest
	// first, under one heading, and leaves out the prerelease's, which
	// the release's notes cover.
	wantJump := "📚 New in the archive (v1.5.0):\n\n**v1.5.0**\n- The archive keeps your place." +
		"\n\n**v1.4.0**\n- Searches are faster.\n\n**v1.3.0**\n- Pictures load lazily."
	if got := byRoom["jump"].Text; got != wantJump {
		t.Errorf("jump's announcement:\n%s\nwant:\n%s", got, wantJump)
	}
	if got, want := byRoom["step"].Text, "📚 New in the archive (v1.5.0):\n- The archive keeps your place."; got != want {
		t.Errorf("step's announcement = %q, want %q", got, want)
	}
	for _, e := range sends {
		if e.Private || e.ReplyTo != "" {
			t.Errorf("an announcement was private or a reply: %+v", e)
		}
	}
	keys := r.a.sentKeys()
	slices.Sort(keys)
	if want := []string{"airut-changelog/v1.5.0/test/channel/jump", "airut-changelog/v1.5.0/test/channel/step"}; !slices.Equal(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
	// The rollback keeps its higher record, and the platform with no
	// adapter is left alone.
	r.wantRecords(map[airut.Channel]string{jump: "v1.5.0", step: "v1.5.0", same: "v1.5.0", rollback: "v1.6.0", fresh: "v1.5.0", elsewhere: "v1.2.0"})
	for o, n := range map[string]int{"posted": 2, "not_newer": 2, "first_run": 1} {
		if got := r.count(o); got != n {
			t.Errorf("%s = %d, want %d", o, got, n)
		}
	}
	if !strings.Contains(r.logs.String(), "changelog announced") || !strings.Contains(r.logs.String(), "channel=jump") {
		t.Errorf("the announcement was not logged by room:\n%s", r.logs)
	}
	r.wantNoNotesLogged()

	// A reconnect finds every record current and posts nothing more.
	r.reconnect()
	if n := len(r.sends()); n != 2 {
		t.Errorf("a reconnect posted again: %d sends", n)
	}
}

func TestChangelogNotAnnouncedForABuild(t *testing.T) {
	old, fresh := room("old"), room("fresh")
	for _, v := range []string{"v1.5.0-3-gabc1234", "v1.5.0-dirty", "", "not a version"} {
		t.Run(v, func(t *testing.T) {
			r := announceRun(t, airut.Config{Version: v}, []airut.Channel{old, fresh}, newMemStore(map[airut.Channel]string{old: "v1.2.0"}))
			if s := r.sends(); len(s) != 0 {
				t.Errorf("a build posted %+v", s)
			}
			r.wantRecords(map[airut.Channel]string{old: "v1.2.0"})
			if got := r.count("not_release"); got != 2 {
				t.Errorf("not_release = %d, want 2", got)
			}
		})
	}
}

func TestChangelogAnnouncesAPrereleaseOnlyWithItsOwnSection(t *testing.T) {
	old, fresh := room("old"), room("fresh")
	r := announceRun(t, airut.Config{Version: "v1.5.0-rc.1"}, []airut.Channel{old, fresh}, newMemStore(map[airut.Channel]string{old: "v1.4.0"}))
	sends := r.sends()
	if len(sends) != 1 || sends[0].Channel != "old" || sends[0].Text != "📚 New in the archive (v1.5.0-rc.1):\n- Trying out bookmarks." {
		t.Fatalf("sends = %+v, want the rc's notes in old", sends)
	}
	r.wantRecords(map[airut.Channel]string{old: "v1.5.0-rc.1", fresh: "v1.5.0-rc.1"})

	// A prerelease with no section posts nothing and moves no record,
	// not even a first one, so its release posts properly later.
	r = announceRun(t, airut.Config{Version: "v1.6.0-rc.1"}, []airut.Channel{old, fresh}, newMemStore(map[airut.Channel]string{old: "v1.5.0"}))
	if s := r.sends(); len(s) != 0 {
		t.Errorf("a prerelease without a section posted %+v", s)
	}
	r.wantRecords(map[airut.Channel]string{old: "v1.5.0"})
	if got := r.count("no_section"); got != 2 {
		t.Errorf("no_section = %d, want 2", got)
	}
}

func TestChangelogRecordsAReleaseWithoutASection(t *testing.T) {
	old, fresh := room("old"), room("fresh")
	r := announceRun(t, airut.Config{Version: "v1.7.0"}, []airut.Channel{old, fresh}, newMemStore(map[airut.Channel]string{old: "v1.4.0"}))
	if s := r.sends(); len(s) != 0 {
		t.Errorf("a release without a section posted %+v", s)
	}
	r.wantRecords(map[airut.Channel]string{old: "v1.7.0", fresh: "v1.7.0"})
	if got, want := r.count("no_section"), 1; got != want {
		t.Errorf("no_section = %d, want %d", got, want)
	}
	if !strings.Contains(r.logs.String(), "no section for the release") || !strings.Contains(r.logs.String(), "version=v1.7.0") {
		t.Errorf("the missing section was not logged:\n%s", r.logs)
	}
}

func TestChangelogRetriesARoomWhosePostFailed(t *testing.T) {
	down, up := room("down"), room("up")
	recs := map[airut.Channel]string{down: "v1.5.0-rc.1", up: "v1.5.0-rc.1"}
	store := newMemStore(recs)
	cfg := airut.Config{Version: "v1.5.0"}
	cfg.Changelog = &airut.Changelog{Notes: updateNotes}
	cfg.Changelog.Channels, cfg.Changelog.Store = []airut.Channel{down, up}, store
	b, logs := newBot(cfg)
	ka := &keyAdapter{Adapter: airuttest.New("")}
	ka.FailSend("down", errors.New("forbidden"))
	r := &announced{t: t, b: b, a: ka, logs: logs, store: store, listed: 2}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, ka) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	<-ka.Ready()
	r.wait()

	if s := r.sends(); len(s) != 1 || s[0].Channel != "up" {
		t.Fatalf("sends = %+v, want only up's", s)
	}
	r.wantRecords(map[airut.Channel]string{down: "v1.5.0-rc.1", up: "v1.5.0"})
	if got := r.count("send_failed"); got != 1 {
		t.Errorf("send_failed = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), "changelog announcement failed") || !strings.Contains(logs.String(), "channel=down") {
		t.Errorf("the failure was not logged by room:\n%s", logs)
	}

	// The next connect retries the room that failed, and only it.
	ka.FailSend("down", nil)
	r.reconnect()
	s := r.sends()
	if len(s) != 2 || s[1].Channel != "down" || s[1].Text != "New in v1.5.0:\n- The archive keeps your place." {
		t.Fatalf("sends after the retry = %+v", s)
	}
	r.wantRecords(map[airut.Channel]string{down: "v1.5.0", up: "v1.5.0"})
}

// A post whose record fails, as in a crash between the two, is sent
// again on the next connect with the same key, which the platform
// answers without posting twice.
func TestChangelogResendIsKeyed(t *testing.T) {
	ch := room("c")
	store := newMemStore(map[airut.Channel]string{ch: "v1.4.0"})
	store.failSave = 1
	r := announceRun(t, airut.Config{Version: "v1.5.0"}, []airut.Channel{ch}, store)
	if got := r.count("record_failed"); got != 1 {
		t.Errorf("record_failed = %d, want 1", got)
	}
	r.wantRecords(map[airut.Channel]string{ch: "v1.4.0"})

	r.reconnect()
	if s := r.sends(); len(s) != 1 {
		t.Errorf("the resend posted again: %+v", s)
	}
	keys := r.a.sentKeys()
	if len(keys) != 2 || keys[0] != keys[1] || keys[0] != "airut-changelog/v1.5.0/test/channel/c" {
		t.Errorf("keys = %q, want the same key twice", keys)
	}
	r.wantRecords(map[airut.Channel]string{ch: "v1.5.0"})
}

func TestRunFailsOnChangelogChannelsWithoutAStore(t *testing.T) {
	b, _ := newBot(airut.Config{Version: "v1.5.0", Changelog: &airut.Changelog{Notes: updateNotes, Channels: []airut.Channel{room("c")}}})
	a := airuttest.New("")
	err := b.Run(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "no Store") {
		t.Fatalf("Run = %v, want an error naming the missing store", err)
	}
	select {
	case <-a.Ready():
		t.Error("the adapter started")
	default:
	}

	b, _ = newBot(airut.Config{Changelog: &airut.Changelog{Notes: updateNotes, Store: newMemStore(nil),
		Channels: []airut.Channel{{Platform: airuttest.Platform}}}})
	if err := b.Run(context.Background(), airuttest.New("")); err == nil || !strings.Contains(err.Error(), "without a platform or an id") {
		t.Fatalf("Run = %v, want an error for a channel with no id", err)
	}
}

func TestFileVersionStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	airuttest.VersionStoreContract(t, airut.FileVersionStore(path))

	// A store opened again on the file has the records.
	again := airut.FileVersionStore(path)
	if v, err := again.AnnouncedVersion(context.Background(), room("room-1")); err != nil || v != "v1.1.0" {
		t.Errorf("after a reopen, room-1 = %q, %v; want v1.1.0", v, err)
	}
	// Every save renamed its file into place, leaving nothing beside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "versions.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %q, want only versions.json", names)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := again.AnnouncedVersion(context.Background(), room("room-1")); err == nil {
		t.Error("a corrupt file read without an error")
	}
	if err := again.SaveAnnouncedVersion(context.Background(), room("room-1"), "v2.0.0"); err == nil {
		t.Error("a save over a corrupt file succeeded, which would drop every other record")
	}
}

// A release leaves out its prereleases' notes, whether the room skipped
// the prerelease or was told of it; a prerelease posts its own.
func TestChangelogDoesNotRepeatAPrereleaseInItsRelease(t *testing.T) {
	release := "📚 New in the archive (v1.5.0):\n- The archive keeps your place."
	for _, c := range []struct{ name, from, version, want string }{
		{"skipped the rc", "v1.4.0", "v1.5.0", release},
		{"to the rc", "v1.4.0", "v1.5.0-rc.1", "📚 New in the archive (v1.5.0-rc.1):\n- Trying out bookmarks."},
		{"from the rc", "v1.5.0-rc.1", "v1.5.0", release},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := room("c")
			r := announceRun(t, airut.Config{Version: c.version}, []airut.Channel{ch}, newMemStore(map[airut.Channel]string{ch: c.from}))
			if s := r.sends(); len(s) != 1 || s[0].Text != c.want {
				t.Errorf("%s to %s posted %+v, want %q", c.from, c.version, s, c.want)
			}
			r.wantRecords(map[airut.Channel]string{ch: c.version})
		})
	}
}
