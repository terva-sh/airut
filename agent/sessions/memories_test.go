package sessions

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func save(t *testing.T, d *DB, m Memory, limit int) Memory {
	t.Helper()
	if m.Scope == "" {
		m.Scope, m.ScopeKey = "channel", "test/channel/c"
	}
	if m.Status == "" {
		m.Status = Active
	}
	if m.Source == "" {
		m.Source, m.Author, m.AuthorName = "user", "test/user/u", "U"
	}
	got, err := d.SaveMemory(context.Background(), m, limit)
	if err != nil {
		t.Fatalf("SaveMemory(%q): %v", m.Text, err)
	}
	return got
}

// statuses returns the scope's memories as text: status.
func statuses(t *testing.T, d *DB) map[string]string {
	t.Helper()
	ms, err := d.Memories(context.Background(), "channel", "test/channel/c")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range ms {
		out[m.Text] = m.Status
	}
	return out
}

func TestSaveMemoryReplacesAndDeduplicates(t *testing.T) {
	c := newClock()
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: c.now})
	first := save(t, d, Memory{Key: "wifi", Text: "the wifi password is hunter2"}, 0)
	c.advance(time.Minute)
	// The same key replaces it, keeping its id.
	second := save(t, d, Memory{Key: "wifi", Text: "the wifi password is now swordfish"}, 0)
	if second.ID != first.ID || second.Text != "the wifi password is now swordfish" || !second.Created.After(first.Created) {
		t.Errorf("a new text under the key gave %+v after %+v", second, first)
	}
	// Nearly the same words refresh it instead of adding a row.
	c.advance(time.Minute)
	again := save(t, d, Memory{Text: "The wifi password is now: swordfish!"}, 0)
	if again.ID != first.ID || !again.LastUsed.After(second.LastUsed) {
		t.Errorf("a near duplicate gave %+v", again)
	}
	other := save(t, d, Memory{Text: "standup is at nine on weekdays"}, 0)
	if other.ID == first.ID {
		t.Error("a different text was taken for a duplicate")
	}
	// A pending memory never replaces an active one, though one that
	// repeats it is already known and is that one.
	p := save(t, d, Memory{Key: "wifi", Text: "guests use the lobby network", Status: Pending}, 0)
	if p.ID == first.ID || p.Status != Pending {
		t.Errorf("a pending memory under an active one's key gave %+v", p)
	}
	if again := save(t, d, Memory{Text: "Guests use the lobby network.", Status: Pending}, 0); again.ID != p.ID {
		t.Errorf("a pending near duplicate gave %+v", again)
	}
	if again := save(t, d, Memory{Text: "the wifi password is now swordfish", Status: Pending}, 0); again.ID != first.ID || again.Status != Active {
		t.Errorf("a pending repeat of an active memory gave %+v", again)
	}
	if got := statuses(t, d); len(got) != 3 {
		t.Errorf("the scope holds %v", got)
	}
}

func TestNearDuplicatesShareFourFifthsOfTheirWords(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: newClock().now})
	base := save(t, d, Memory{Text: "red green blue cyan"}, 0)
	if got := save(t, d, Memory{Text: "red green blue cyan magenta"}, 0); got.ID != base.ID {
		t.Error("four words of five shared did not count as a duplicate")
	}
	if got := save(t, d, Memory{Text: "red green blue"}, 0); got.ID == base.ID {
		t.Error("three words of four shared counted as a duplicate")
	}
	// An active memory never folds into a pending one: the person it is
	// about saying it is not their consenting to someone else's note.
	p := save(t, d, Memory{Scope: "user", ScopeKey: "test/user/b", Text: "likes green tea", Status: Pending}, 0)
	if got := save(t, d, Memory{Scope: "user", ScopeKey: "test/user/b", Text: "likes green tea"}, 0); got.ID == p.ID || got.Status != Active {
		t.Errorf("an active memory like a pending one gave %+v", got)
	}
}

// A changed proposal needs fresh consent: the answer about the old text
// names an id that no longer exists.
func TestReplacingAPendingMemoryGivesItANewID(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: newClock().now})
	ctx := context.Background()
	asked := save(t, d, Memory{Key: "drink", Text: "likes tea", Status: Pending}, 0)
	changed := save(t, d, Memory{Key: "drink", Text: "likes whisky at work", Status: Pending}, 0)
	if changed.ID == asked.ID {
		t.Fatal("the changed proposal kept the id the person was asked about")
	}
	if err := d.SetMemoryStatus(ctx, asked.ID, Active, 0); !errors.Is(err, ErrNoMemory) {
		t.Errorf("approving the old proposal: %v", err)
	}
	if got := statuses(t, d); got["likes whisky at work"] != Pending || len(got) != 1 {
		t.Errorf("after an answer about the old text: %v", got)
	}
	// The same proposal again stands as it was asked about, and a changed
	// one is saved, never lost.
	if again := save(t, d, Memory{Key: "drink", Text: "Likes whisky at work.", Status: Pending}, 0); again.ID != changed.ID {
		t.Errorf("repeating the proposal gave %+v", again)
	}
	last := save(t, d, Memory{Key: "drink", Text: "likes water", Status: Pending}, 0)
	if _, err := d.Memory(ctx, last.ID); err != nil || last.ID == changed.ID {
		t.Errorf("the replacement %+v: %v", last, err)
	}
	if got := statuses(t, d); got["likes water"] != Pending || len(got) != 1 {
		t.Errorf("after replacing again: %v", got)
	}
}

// Approving a proposal under a key replaces what the key held, so one
// key never holds two active memories.
func TestApprovingAKeyedProposalReplacesTheKey(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: newClock().now})
	save(t, d, Memory{Key: "drink", Text: "likes tea"}, 0)
	save(t, d, Memory{Key: "food", Text: "likes rice"}, 0)
	p := save(t, d, Memory{Key: "drink", Text: "now prefers coffee", Status: Pending}, 0)
	if err := d.SetMemoryStatus(context.Background(), p.ID, Active, 0); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, d); len(got) != 2 || got["now prefers coffee"] != Active || got["likes rice"] != Active {
		t.Errorf("after approving under a held key: %v", got)
	}
}

func TestApprovingCountsAsUse(t *testing.T) {
	c := newClock()
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: c.now})
	p := save(t, d, Memory{Text: "proposed long ago", Status: Pending}, 1)
	c.advance(time.Minute)
	save(t, d, Memory{Text: "written since"}, 1)
	c.advance(time.Minute)
	if err := d.SetMemoryStatus(context.Background(), p.ID, Active, 1); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, d); got["proposed long ago"] != Active || got["written since"] != Archived {
		t.Errorf("after approving the older one with a cap of one: %v", got)
	}
}

func TestSaveMemoryArchivesPastTheCap(t *testing.T) {
	c := newClock()
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: c.now})
	ctx := context.Background()
	var ms []Memory
	for _, text := range []string{"alpha one", "bravo two", "charlie three"} {
		ms = append(ms, save(t, d, Memory{Text: text}, 2))
		c.advance(time.Minute)
	}
	if got := statuses(t, d); got["alpha one"] != Archived || got["bravo two"] != Active || got["charlie three"] != Active {
		t.Fatalf("after three with a cap of two: %v", got)
	}
	// Using bravo makes charlie the least recently used.
	if err := d.TouchMemories(ctx, ms[1].ID); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	save(t, d, Memory{Text: "delta four"}, 2)
	if got := statuses(t, d); got["charlie three"] != Archived || got["bravo two"] != Active {
		t.Fatalf("after a touch: %v", got)
	}
	// Approving one past the cap archives another.
	c.advance(time.Minute)
	p := save(t, d, Memory{Text: "echo five", Status: Pending}, 2)
	c.advance(time.Minute)
	if err := d.SetMemoryStatus(ctx, p.ID, Active, 2); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, d); got["echo five"] != Active || got["bravo two"] != Archived {
		t.Fatalf("after an approval: %v", got)
	}
	// The archive keeps four times the cap, dropping its oldest.
	for i := range 8 {
		c.advance(time.Minute)
		save(t, d, Memory{Text: "filler " + string(rune('a'+i))}, 2)
	}
	got := statuses(t, d)
	archived := 0
	for _, s := range got {
		if s == Archived {
			archived++
		}
	}
	if archived != 8 || got["alpha one"] != "" {
		t.Errorf("the archive holds %d, alpha %q: %v", archived, got["alpha one"], got)
	}
	if err := d.SetMemoryStatus(ctx, 9999, Active, 2); !errors.Is(err, ErrNoMemory) {
		t.Errorf("approving a memory that does not exist: %v", err)
	}
	if _, err := d.Memory(ctx, 9999); !errors.Is(err, ErrNoMemory) {
		t.Errorf("reading a memory that does not exist: %v", err)
	}
}

func TestUserMemoriesExpire(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "s.db")
	d := open(t, path, Options{Retention: time.Hour, Now: c.now})
	ctx := context.Background()
	old := save(t, d, Memory{Scope: "user", ScopeKey: "test/user/u", Text: "old"}, 0)
	pending := save(t, d, Memory{Scope: "user", ScopeKey: "test/user/u", Text: "unanswered", Status: Pending}, 0)
	room := save(t, d, Memory{Text: "a channel's"}, 0)
	bot := save(t, d, Memory{Scope: "bot", Text: "the bot's"}, 0)
	c.advance(50 * time.Minute)
	used := save(t, d, Memory{Scope: "user", ScopeKey: "test/user/u", Text: "recent"}, 0)
	c.advance(20 * time.Minute)
	if _, err := d.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		m    Memory
		kept bool
	}{{old, false}, {pending, false}, {room, true}, {bot, true}, {used, true}} {
		if _, err := d.Memory(ctx, c.m.ID); (err == nil) != c.kept {
			t.Errorf("%s: %v", c.m.Text, err)
		}
	}
	// A removed memory's id never names a later one.
	if ok, err := d.DeleteMemory(ctx, used); err != nil || !ok {
		t.Fatalf("DeleteMemory = %v, %v", ok, err)
	}
	if next := save(t, d, Memory{Text: "next"}, 0); next.ID <= used.ID {
		t.Errorf("a new memory reused id %d", next.ID)
	}
}

func TestWords(t *testing.T) {
	if got := Words("The wifi, the WIFI: café-2!"); !slices.Equal(got, []string{"the", "wifi", "café", "2"}) {
		t.Errorf("Words = %q", got)
	}
}

// Removal takes the memory as it was read, so one replaced or approved
// since is left alone.
func TestDeleteMemoryTakesItAsRead(t *testing.T) {
	c := newClock()
	d := open(t, filepath.Join(t.TempDir(), "s.db"), Options{Now: c.now})
	ctx := context.Background()
	read := save(t, d, Memory{Key: "wifi", Text: "the wifi is open"}, 0)
	c.advance(time.Minute)
	save(t, d, Memory{Key: "wifi", Text: "the wifi is closed", Author: "test/user/b", AuthorName: "B", Source: "user"}, 0)
	if ok, err := d.DeleteMemory(ctx, read); err != nil || ok {
		t.Errorf("deleting a replaced memory as read: %v, %v", ok, err)
	}
	pending := save(t, d, Memory{Text: "a proposal", Status: Pending}, 0)
	if err := d.SetMemoryStatus(ctx, pending.ID, Active, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.DeleteMemory(ctx, pending); err != nil || ok {
		t.Errorf("deleting an approved memory as it was pending: %v, %v", ok, err)
	}
	if got := statuses(t, d); got["the wifi is closed"] != Active || got["a proposal"] != Active {
		t.Errorf("after both: %v", got)
	}
	// Read again, it goes.
	now, err := d.Memory(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.DeleteMemory(ctx, now); err != nil || !ok {
		t.Errorf("deleting it as read now: %v, %v", ok, err)
	}
}
