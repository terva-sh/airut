package airut_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// claimsAgent is an Agent that keeps owner claims, as the agent
// package's does in its session store. It runs no turns.
type claimsAgent struct {
	mu     sync.Mutex
	claims []airut.Claim
	err    error
	reads  int
}

func (a *claimsAgent) Run(*airut.Ctx, airut.Turn, airut.TurnOutput) error { return nil }

func (a *claimsAgent) ClaimedOwners(context.Context) ([]airut.Claim, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	if a.err != nil {
		return nil, a.err
	}
	return append([]airut.Claim(nil), a.claims...), nil
}

func (a *claimsAgent) set(claims []airut.Claim, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claims, a.err = claims, err
}

func (a *claimsAgent) readCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads
}

// ownerClock is a settable clock for the owner claims' re-reads.
type ownerClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ownerClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ownerClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var claimedAt = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)

func claimsBot(t *testing.T, cfg airut.Config, claims *claimsAgent) (*airut.Bot, *ownerClock, *lockedBuffer) {
	t.Helper()
	cfg.Agent = claims
	b, logs := newBot(cfg)
	clock := &ownerClock{t: claimedAt.Add(time.Hour)}
	airut.SetOwnerClock(b, clock.now)
	b.Command("secret", "Owners only", func(c *airut.Ctx) error {
		_, err := c.Reply("yes")
		return err
	}).RestrictTo(airut.Owner)
	return b, clock, logs
}

const refusedSecret = "reply:You are not allowed to use !secret."

func TestAClaimMadeWhileTheBotRunsIsHonouredWithinAMinute(t *testing.T) {
	claims := &claimsAgent{}
	b, clock, logs := claimsBot(t, airut.Config{}, claims)
	a := airuttest.Start(t, b)
	if !strings.Contains(logs.String(), ownerWarned) {
		t.Errorf("a bot with no owner and no claim did not warn:\n%s", logs.String())
	}

	wantEvents(t, a.Text("!secret", asOwner), refusedSecret)
	claims.set([]airut.Claim{{Platform: airuttest.Platform, ID: owner.ID, At: claimedAt}}, nil)
	// Within the minute the claims are not read again.
	reads := claims.readCount()
	clock.advance(30 * time.Second)
	wantEvents(t, a.Text("!secret", asOwner), refusedSecret)
	wantEvents(t, a.Text("!secret", asOwner), refusedSecret)
	if n := claims.readCount(); n != reads {
		t.Errorf("the claims were read %d times within the minute", n-reads)
	}
	clock.advance(30 * time.Second)
	wantEvents(t, a.Text("!secret", asOwner), "reply:yes")
	if has, err := b.HasOwner(context.Background(), airuttest.Platform); !has || err != nil {
		t.Errorf("HasOwner after a claim = %v, %v", has, err)
	}
	// Someone else is still refused, and a claim on another platform
	// admits no one here.
	wantEvents(t, a.Text("!secret"), refusedSecret)

	// An unclaimed owner is dropped within the minute too.
	claims.set(nil, nil)
	clock.advance(time.Minute)
	wantEvents(t, a.Text("!secret", asOwner), refusedSecret)
	if l := logs.String(); strings.Contains(l, "id=owner ") {
		t.Errorf("the log names an owner:\n%s", l)
	}
}

func TestAClaimedBotDoesNotWarn(t *testing.T) {
	claims := &claimsAgent{claims: []airut.Claim{{Platform: airuttest.Platform, ID: owner.ID, At: claimedAt}}}
	b, _, logs := claimsBot(t, airut.Config{}, claims)
	a := airuttest.Start(t, b)
	if l := logs.String(); strings.Contains(l, ownerWarned) {
		t.Errorf("a claimed bot warned:\n%s", l)
	}
	wantEvents(t, a.DirectMessage("hello"))
	wantEvents(t, a.Text("!secret", asOwner), "reply:yes")

	// A claim on a platform the bot does not serve leaves it unowned.
	claims = &claimsAgent{claims: []airut.Claim{{Platform: airut.Matrix, ID: "@you:example.org", At: claimedAt}}}
	b, _, logs = claimsBot(t, airut.Config{}, claims)
	a = airuttest.Start(t, b)
	if l := logs.String(); !strings.Contains(l, ownerWarned) {
		t.Errorf("a bot claimed only elsewhere did not warn:\n%s", l)
	}
	wantEvents(t, a.DirectMessage("hello"), claimReply)
}

func TestAFailedClaimReadKeepsTheClaimsLastRead(t *testing.T) {
	claims := &claimsAgent{claims: []airut.Claim{{Platform: airuttest.Platform, ID: owner.ID, At: claimedAt}}}
	b, clock, logs := claimsBot(t, airut.Config{}, claims)
	a := airuttest.Start(t, b)
	wantEvents(t, a.Text("!secret", asOwner), "reply:yes")

	claims.set(nil, errors.New("disk I/O error: the store at /srv/secret/path"))
	for range 3 {
		clock.advance(time.Minute)
		// The owner read before is kept, and a miss is refused.
		wantEvents(t, a.Text("!secret", asOwner), "reply:yes")
		wantEvents(t, a.Text("!secret"), refusedSecret)
	}
	l := logs.String()
	if n := strings.Count(l, "owner claims unreadable"); n != 1 {
		t.Errorf("a failure streak was logged %d times, want once:\n%s", n, l)
	}
	if strings.Contains(l, "/srv/secret/path") {
		t.Errorf("the log holds the error's text:\n%s", l)
	}

	// Once the store reads again, its claims replace the cache.
	claims.set(nil, nil)
	clock.advance(time.Minute)
	wantEvents(t, a.Text("!secret", asOwner), refusedSecret)
	if l := logs.String(); !strings.Contains(l, "owner claims readable again") {
		t.Errorf("the recovery was not logged:\n%s", l)
	}
	claims.set(nil, errors.New("again"))
	clock.advance(time.Minute)
	wantEvents(t, a.Text("!secret"), refusedSecret)
	if n := strings.Count(logs.String(), "owner claims unreadable"); n != 2 {
		t.Errorf("a second failure streak: logged %d times in all, want 2", n)
	}
}

func TestStatusShowsClaimedOwnersToOwners(t *testing.T) {
	claims := &claimsAgent{claims: []airut.Claim{
		{Platform: airuttest.Platform, ID: "claimed-one", At: claimedAt},
		{Platform: airut.Matrix, ID: "@you:example.org", At: claimedAt.Add(time.Minute)},
	}}
	b, _, _ := claimsBot(t, airut.Config{Version: "v1.3.0", Owners: owned}, claims)
	a := airuttest.Start(t, b)

	wantEvents(t, a.Text("!status"), "reply:Running v1.3.0.\nPlatforms:\n- test: has an owner")
	want := "reply:Running v1.3.0.\nPlatforms:\n- test: has an owner\nOwners:\n" +
		"- matrix @you:example.org (claimed, 2026-10-22T09:31:00Z)\n" +
		"- test claimed-one (claimed, 2026-10-22T09:30:00Z)\n" +
		"- test owner (config)"
	wantEvents(t, a.Text("!status", asOwner), want)
	// A claimed owner sees the same.
	wantEvents(t, a.Text("!status", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "claimed-one"})), want)
}

// No command a chat can reach adds or removes an owner: the built-ins
// a bot with an agent has name none that does.
func TestNoChatCommandChangesOwners(t *testing.T) {
	b, _, _ := claimsBot(t, airut.Config{Owners: owned, ProfileEditors: airut.Owner}, &claimsAgent{})
	a := airuttest.Start(t, b)
	for _, name := range specNames(a) {
		if strings.Contains(name, "claim") || strings.Contains(name, "owner") {
			t.Errorf("a chat command %q", name)
		}
	}
}

// gatedClaims is an OwnerClaims whose reads block, once gate is set,
// until gate is closed or the reader's context ends. Each blocked read
// says so on started.
type gatedClaims struct {
	mu      sync.Mutex
	claims  []airut.Claim
	gate    chan struct{}
	started chan struct{}
	reads   int
}

func (g *gatedClaims) Run(*airut.Ctx, airut.Turn, airut.TurnOutput) error { return nil }

func (g *gatedClaims) ClaimedOwners(ctx context.Context) ([]airut.Claim, error) {
	g.mu.Lock()
	g.reads++
	gate, claims := g.gate, append([]airut.Claim(nil), g.claims...)
	g.mu.Unlock()
	if gate != nil {
		g.started <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return claims, nil
}

func (g *gatedClaims) close() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate = make(chan struct{})
	return g.gate
}

func (g *gatedClaims) readCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// promptly fails the test when f does not return within a second, and
// returns what it returned.
func promptly(t *testing.T, what string, f func() bool) bool {
	t.Helper()
	got := make(chan bool, 1)
	go func() { got <- f() }()
	select {
	case v := <-got:
		return v
	case <-time.After(time.Second):
		t.Fatalf("%s waited on another check's read", what)
		return false
	}
}

// gatedBot is a bot reading claims from g, not started, so that only the
// test's checks read them.
func gatedBot(t *testing.T, g *gatedClaims) (*airut.Bot, *ownerClock) {
	t.Helper()
	b, _ := newBot(airut.Config{Agent: g})
	clock := &ownerClock{t: claimedAt.Add(time.Hour)}
	airut.SetOwnerClock(b, clock.now)
	return b, clock
}

// A check whose context ends while another check's read is in flight
// returns at once, with the cache's answer, and the read it gave up on
// still lands.
func TestACancelledCheckDoesNotWaitForAnotherRead(t *testing.T) {
	g := &gatedClaims{started: make(chan struct{}, 4)}
	b, _ := gatedBot(t, g)
	g.claims = []airut.Claim{{Platform: airuttest.Platform, ID: owner.ID, At: claimedAt}}
	gate := g.close()

	first := make(chan bool, 1)
	go func() {
		has, _ := b.HasOwner(context.Background(), airuttest.Platform)
		first <- has
	}()
	<-g.started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if promptly(t, "a cancelled check", func() bool {
		has, err := b.HasOwner(ctx, airuttest.Platform)
		return has || err != nil
	}) {
		t.Error("a cancelled check answered other than the empty cache's refusal")
	}
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	promptly(t, "a check with a short deadline", func() bool {
		has, _ := b.HasOwner(short, airuttest.Platform)
		return has
	})

	close(gate)
	if !<-first {
		t.Error("the read in flight did not land")
	}
	if n := g.readCount(); n != 1 {
		t.Errorf("the claims were read %d times, want once", n)
	}
}

// A check the cached claims already answer does not wait for a read
// another check started, even when the cache is due a re-read.
func TestACachedHitDoesNotWaitForARead(t *testing.T) {
	g := &gatedClaims{started: make(chan struct{}, 4),
		claims: []airut.Claim{{Platform: airuttest.Platform, ID: owner.ID, At: claimedAt}}}
	b, clock := gatedBot(t, g)
	ctx := context.Background()
	if has, _ := b.HasOwner(ctx, airuttest.Platform); !has {
		t.Fatal("the first read found no owner")
	}

	clock.advance(time.Minute)
	gate := g.close()
	miss := make(chan bool, 1)
	go func() {
		has, _ := b.HasOwner(ctx, airut.Matrix)
		miss <- has
	}()
	<-g.started
	if !promptly(t, "a cached hit", func() bool {
		has, _ := b.HasOwner(ctx, airuttest.Platform)
		return has
	}) {
		t.Error("a cached hit was refused")
	}
	close(gate)
	if <-miss {
		t.Error("a platform with no claim has an owner")
	}
	if n := g.readCount(); n != 2 {
		t.Errorf("the claims were read %d times, want twice", n)
	}
}

// Checks that miss while a read is in flight wait for it, rather than
// each reading: one read at a time.
func TestMissesShareARead(t *testing.T) {
	g := &gatedClaims{started: make(chan struct{}, 8)}
	b, _ := gatedBot(t, g)
	g.claims = []airut.Claim{{Platform: airuttest.Platform, ID: owner.ID, At: claimedAt}}
	gate := g.close()

	results := make(chan bool, 5)
	go func() {
		has, _ := b.HasOwner(context.Background(), airuttest.Platform)
		results <- has
	}()
	<-g.started
	for range 4 {
		go func() {
			has, _ := b.HasOwner(context.Background(), airuttest.Platform)
			results <- has
		}()
	}
	// Give the waiters time to start a read of their own, if they would.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	for range 5 {
		if !<-results {
			t.Error("a waiter missed the claim the shared read found")
		}
	}
	if n := g.readCount(); n != 1 {
		t.Errorf("the claims were read %d times, want once", n)
	}
}

// isOwnerAgrees asks IsOwner about u, and checks it answers want and
// that airut.Owner, through the secret command, answers the same.
func isOwnerAgrees(t *testing.T, b *airut.Bot, a *airuttest.Adapter, u airut.User, want bool) {
	t.Helper()
	got, err := b.IsOwner(context.Background(), u)
	if got != want || err != nil {
		t.Errorf("IsOwner(%s %s) = %v, %v; want %v", u.Platform, u.ID, got, err, want)
	}
	if u.Platform != airuttest.Platform {
		return
	}
	reply := refusedSecret
	if want {
		reply = "reply:yes"
	}
	wantEvents(t, a.Text("!secret", airuttest.As(u)), reply)
}

func TestIsOwnerAnswersAsOwnerDoes(t *testing.T) {
	claimed := airut.User{Platform: airuttest.Platform, ID: "claimed-one"}
	claims := &claimsAgent{claims: []airut.Claim{
		{Platform: claimed.Platform, ID: claimed.ID, At: claimedAt},
		{Platform: airut.Matrix, ID: "@claimed:example.org", At: claimedAt},
	}}
	b, clock, logs := claimsBot(t, airut.Config{Owners: map[airut.Platform][]string{
		airuttest.Platform: {owner.ID}, airut.Matrix: {"@listed:example.org"},
	}}, claims)
	a := airuttest.Start(t, b)

	// Listed and claimed owners, on the test platform and on Matrix.
	isOwnerAgrees(t, b, a, owner, true)
	isOwnerAgrees(t, b, a, claimed, true)
	isOwnerAgrees(t, b, a, airut.User{Platform: airut.Matrix, ID: "@listed:example.org"}, true)
	isOwnerAgrees(t, b, a, airut.User{Platform: airut.Matrix, ID: "@claimed:example.org"}, true)
	// Someone else, and an owner's id on a platform it is neither listed
	// nor claimed for, are not owners.
	isOwnerAgrees(t, b, a, airut.User{Platform: airuttest.Platform, ID: "stranger"}, false)
	isOwnerAgrees(t, b, a, airut.User{Platform: airuttest.Platform, ID: "@listed:example.org"}, false)
	isOwnerAgrees(t, b, a, airut.User{Platform: airut.Discord, ID: owner.ID}, false)

	// A claim made while the bot runs counts once the claims are due a
	// read, within the minute, and not before.
	late := airut.User{Platform: airuttest.Platform, ID: "late"}
	claims.set([]airut.Claim{
		{Platform: claimed.Platform, ID: claimed.ID, At: claimedAt},
		{Platform: late.Platform, ID: late.ID, At: claimedAt},
	}, nil)
	isOwnerAgrees(t, b, a, late, false)
	clock.advance(time.Minute)
	isOwnerAgrees(t, b, a, late, true)

	// A claims read that fails keeps the claims last read: a claimed
	// owner still is one, and an id they do not hold is refused.
	reads := claims.readCount()
	claims.set(nil, errors.New("disk I/O error: the store at /srv/secret/path"))
	clock.advance(time.Minute)
	isOwnerAgrees(t, b, a, claimed, true)
	isOwnerAgrees(t, b, a, owner, true)
	isOwnerAgrees(t, b, a, airut.User{Platform: airuttest.Platform, ID: "stranger"}, false)
	if claims.readCount() == reads {
		t.Error("the failing claims read never ran")
	}
	if l := logs.String(); strings.Contains(l, "/srv/secret/path") || strings.Contains(l, "claimed-one") {
		t.Errorf("the log holds the error's text or an owner's id:\n%s", l)
	}
}

// A bot whose first claims read fails has no claimed owner, so IsOwner
// refuses everyone the config does not list until a read succeeds.
func TestIsOwnerRefusesWhenTheClaimsWereNeverRead(t *testing.T) {
	claimed := airut.User{Platform: airuttest.Platform, ID: "claimed-one"}
	list := []airut.Claim{{Platform: claimed.Platform, ID: claimed.ID, At: claimedAt}}
	claims := &claimsAgent{claims: list, err: errors.New("database is locked")}
	b, clock, _ := claimsBot(t, airut.Config{Owners: owned}, claims)
	a := airuttest.Start(t, b)

	isOwnerAgrees(t, b, a, claimed, false)
	isOwnerAgrees(t, b, a, owner, true)
	claims.set(list, nil)
	clock.advance(time.Minute)
	isOwnerAgrees(t, b, a, claimed, true)
}

// Owners lists one platform's owners from the config and the claims,
// each once and sorted, for a Matrix adapter that admits them to
// verify the bot from chat.
func TestOwnersListsConfiguredAndClaimed(t *testing.T) {
	ag := &claimsAgent{claims: []airut.Claim{
		{Platform: airut.Matrix, ID: "@claimed:example.org", At: time.Unix(1, 0)},
		{Platform: airut.Matrix, ID: "@listed:example.org", At: time.Unix(2, 0)},
		{Platform: airut.Discord, ID: "123", At: time.Unix(3, 0)},
	}}
	b := airut.New(airut.Config{
		Agent:  ag,
		Owners: map[airut.Platform][]string{airut.Matrix: {"@listed:example.org", "@another:example.org"}, airut.Discord: {"456"}},
	})
	got, err := b.Owners(context.Background(), airut.Matrix)
	want := []string{"@another:example.org", "@claimed:example.org", "@listed:example.org"}
	if err != nil || strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("Owners(matrix) = %v, %v; want %v", got, err, want)
	}
	if got, _ := b.Owners(context.Background(), airut.Platform("irc")); len(got) != 0 {
		t.Errorf("Owners(irc) = %v", got)
	}
}
