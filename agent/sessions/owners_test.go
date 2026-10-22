package sessions

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"terva.sh/terva/packages/provider"
)

func TestOwnerClaims(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	c := newClock()
	d := open(t, path, Options{Now: c.now})
	if got, err := d.OwnerClaims(ctx); err != nil || len(got) != 0 {
		t.Fatalf("a new store's claims = %v, %v", got, err)
	}

	first := c.now()
	got, added, err := d.ClaimOwner(ctx, "matrix", "@you:example.org", "ops")
	want := OwnerClaim{Platform: "matrix", ID: "@you:example.org", At: first, Actor: "ops"}
	if err != nil || !added || !got.At.Equal(want.At) || got.Actor != "ops" {
		t.Fatalf("ClaimOwner = %+v, %v, %v; want %+v, true", got, added, err, want)
	}
	// Claiming again keeps the first claim.
	c.advance(time.Hour)
	got, added, err = d.ClaimOwner(ctx, "matrix", "@you:example.org", "someone else")
	if err != nil || added || !got.At.Equal(first) || got.Actor != "ops" {
		t.Fatalf("claiming again = %+v, %v, %v; want the first claim, not added", got, added, err)
	}
	if _, _, err := d.ClaimOwner(ctx, "discord", "123", "ops"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ClaimOwner(ctx, "discord", "45", "ops"); err != nil {
		t.Fatal(err)
	}

	// Claims survive a reopen, listed by platform and then id.
	d.Close()
	d = open(t, path, Options{Now: c.now})
	list, err := d.OwnerClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range list {
		keys = append(keys, o.Platform+" "+o.ID)
	}
	if want := []string{"discord 123", "discord 45", "matrix @you:example.org"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("OwnerClaims = %q, want %q", keys, want)
	}
	if o, ok, err := d.ClaimedOwner(ctx, "matrix", "@you:example.org"); err != nil || !ok || !o.At.Equal(first) {
		t.Errorf("ClaimedOwner = %+v, %v, %v", o, ok, err)
	}

	// The same id on another platform is another claim.
	if _, ok, err := d.ClaimedOwner(ctx, "discord", "@you:example.org"); err != nil || ok {
		t.Errorf("ClaimedOwner on another platform = %v, %v; want none", ok, err)
	}
	if removed, err := d.UnclaimOwner(ctx, "discord", "@you:example.org"); err != nil || removed {
		t.Errorf("unclaiming on another platform = %v, %v; want nothing removed", removed, err)
	}
	if removed, err := d.UnclaimOwner(ctx, "matrix", "@you:example.org"); err != nil || !removed {
		t.Errorf("UnclaimOwner = %v, %v; want removed", removed, err)
	}
	if removed, err := d.UnclaimOwner(ctx, "matrix", "@you:example.org"); err != nil || removed {
		t.Errorf("unclaiming twice = %v, %v; want nothing removed", removed, err)
	}
	if _, ok, err := d.ClaimedOwner(ctx, "matrix", "@you:example.org"); err != nil || ok {
		t.Errorf("an unclaimed owner = %v, %v", ok, err)
	}
}

// A store opened with NoSweep deletes nothing, however old the
// sessions in it are, so a short-lived process cannot expire what the
// bot keeps by a longer retention.
func TestNoSweepDeletesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	c := newClock()
	d := open(t, path, Options{Retention: time.Hour, Now: c.now})
	if err := session(t, d, "old").AppendMessage(text(provider.RoleUser, "x")); err != nil {
		t.Fatal(err)
	}
	d.Close()

	c.advance(3 * time.Hour)
	d = open(t, path, Options{Retention: time.Hour, Now: c.now, NoSweep: true})
	if _, _, err := d.ClaimOwner(context.Background(), "discord", "123", "ops"); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d = open(t, path, Options{Retention: 24 * time.Hour, Now: c.now})
	if tr, err := session(t, d, "old").Reload(); err != nil || len(tr.Messages) != 1 {
		t.Fatalf("after a NoSweep open, the old session has %d messages (%v), want 1", len(tr.Messages), err)
	}
}
