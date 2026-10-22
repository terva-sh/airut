package airut_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

const (
	userFull    = "reply:You have started too many agent turns. Try again in "
	channelFull = "reply:Too many agent turns have started in this channel. Try again in "
)

// ownerGate is a bot whose "ask" command starts an agent turn, with the
// user "owner" as its owner, on a clock the test moves.
func ownerGate(t *testing.T, g airut.Gate) (*airuttest.Adapter, *airut.Bot, *airuttest.Agent, *lockedBuffer, *fakeClock) {
	t.Helper()
	g.Access = map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"owner", "user-1", "user-2"}}}
	// A settingStore keeps the overrides the settings tests set.
	st := &settingStore{Agent: airuttest.Agent{Steps: []airuttest.Step{{Text: "answer"}}}}
	agent := &st.Agent
	b, logs := newBot(airut.Config{Agent: st, Gate: g, Owners: map[airut.Platform][]string{airuttest.Platform: {"owner"}},
		Settings: airut.Settings{Owner: isOwner}})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "a secret prompt"}).StreamTo(c)
	})
	clock := &fakeClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	airut.SetGateClock(b, clock.now)
	return airuttest.Start(t, b), b, agent, logs, clock
}

func as(id string) airuttest.Option {
	return airuttest.As(airut.User{Platform: airuttest.Platform, ID: id})
}

var elsewhere = airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "channel-2"})

// wantPrefix checks that events is one reply that opens with prefix.
func wantPrefix(t *testing.T, events []airuttest.Event, prefix string) {
	t.Helper()
	if g := texts(events); len(g) != 1 || !strings.HasPrefix(g[0], prefix) {
		t.Errorf("events %q, want one opening %q", g, prefix)
	}
}

// TestGateOwnersPerUser: an owner past the members' PerUser is admitted
// up to the owners' cap, and a member is held to the members'.
func TestGateOwnersPerUser(t *testing.T) {
	a, _, agent, _, _ := ownerGate(t, airut.Gate{PerUser: 1, Window: time.Minute, Owners: &airut.GroupRate{PerUser: 3}})
	for range 3 {
		wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	}
	wantPrefix(t, a.Text("!ask", as("owner")), userFull)

	wantEvents(t, a.Text("!ask"), "reply:answer")
	wantPrefix(t, a.Text("!ask"), userFull)
	if n := len(agent.Turns()); n != 4 {
		t.Errorf("%d turns reached the agent, want the owner's 3 and the member's 1", n)
	}
}

// TestGateUncappedOwnerCounts: an owner with no caps is never refused,
// and their turns still fill the channel's shared count, so a member is
// refused after them.
func TestGateUncappedOwnerCounts(t *testing.T) {
	a, _, _, _, _ := ownerGate(t, airut.Gate{PerUser: 1, PerChannel: 2, Window: time.Minute, Owners: &airut.GroupRate{}})
	for range 5 {
		wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	}
	wantPrefix(t, a.Text("!ask"), channelFull)
	// The refusal was the channel's: the member may still ask elsewhere.
	wantEvents(t, a.Text("!ask", elsewhere), "reply:answer")
}

// TestGateOwnersPerChannel: the owners' PerChannel is checked against the
// channel's shared count, members' turns included.
func TestGateOwnersPerChannel(t *testing.T) {
	a, _, _, _, _ := ownerGate(t, airut.Gate{PerChannel: 2, Window: time.Minute, Owners: &airut.GroupRate{PerChannel: 4}})
	wantEvents(t, a.Text("!ask"), "reply:answer")
	wantEvents(t, a.Text("!ask", as("user-2")), "reply:answer")
	wantPrefix(t, a.Text("!ask"), channelFull)
	// The owner fills the channel up to their own cap, counting the
	// members' two turns.
	wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	wantPrefix(t, a.Text("!ask", as("owner")), channelFull)
	// In a room of their own they start from nothing.
	wantEvents(t, a.Text("!ask", as("owner"), elsewhere), "reply:answer")
}

// TestGateMembersFillTheOwnersChannelCap: with only the owners capping
// channels, members' turns still enter the channel's count, so an owner
// is refused once the room's real usage reaches the owners' cap.
func TestGateMembersFillTheOwnersChannelCap(t *testing.T) {
	a, _, _, _, _ := ownerGate(t, airut.Gate{Window: time.Minute, Owners: &airut.GroupRate{PerChannel: 2}})
	wantEvents(t, a.Text("!ask"), "reply:answer")
	wantEvents(t, a.Text("!ask"), "reply:answer")
	wantEvents(t, a.Text("!ask", as("user-2")), "reply:answer")
	wantPrefix(t, a.Text("!ask", as("owner")), channelFull)
	wantEvents(t, a.Text("!ask", as("owner"), elsewhere), "reply:answer")
}

// TestGateWaitPastAnOwnersTurns: owners' turns can leave a channel's
// count above the members' cap, and the wait a member is given is until
// enough have left that the count is under it, not until the oldest has.
func TestGateWaitPastAnOwnersTurns(t *testing.T) {
	a, _, _, _, clock := ownerGate(t, airut.Gate{PerChannel: 2, Window: time.Minute, Owners: &airut.GroupRate{}})
	for i := range 4 { // the owner's turns at 0s, 10s, 20s, and 30s
		if i > 0 {
			clock.advance(10 * time.Second)
		}
		wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	}
	// At 30s the count is 4; it drops under 2 when the 20s turn leaves,
	// at 80s.
	wantEvents(t, a.Text("!ask"), channelFull+"50 seconds.")
	clock.advance(49 * time.Second)
	wantEvents(t, a.Text("!ask"), channelFull+"1 second.")
	clock.advance(time.Second)
	wantEvents(t, a.Text("!ask"), "reply:answer")
}

// TestGateOwnerCheckFails: an owner check that fails leaves the caller at
// the members' rate, never uncapped, and is logged by kind.
func TestGateOwnerCheckFails(t *testing.T) {
	a, b, _, logs, _ := ownerGate(t, airut.Gate{PerUser: 1, Window: time.Minute, Owners: &airut.GroupRate{}})
	airut.SetGateOwnerCheck(b, func(*airut.Ctx) (bool, error) {
		return false, errors.New("the owner store at secret-host is down")
	})
	wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	wantPrefix(t, a.Text("!ask", as("owner")), userFull)
	l := logs.String()
	if !strings.Contains(l, "owner check failed") || !strings.Contains(l, "error_kind=") {
		t.Errorf("the failed check was not logged by kind:\n%s", l)
	}
	if strings.Contains(l, "secret-host") || strings.Contains(l, "secret prompt") {
		t.Errorf("the log holds an error's text or chat text:\n%s", l)
	}
}

// TestGateWithoutOwnersRate: with no Owners an owner takes the members'
// rate, and the gate never asks whether they are one.
func TestGateWithoutOwnersRate(t *testing.T) {
	a, b, _, _, _ := ownerGate(t, airut.Gate{PerUser: 1, Window: time.Minute})
	airut.SetGateOwnerCheck(b, func(*airut.Ctx) (bool, error) {
		t.Error("the gate asked for an owner with no owners' rate")
		return true, nil
	})
	wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	wantPrefix(t, a.Text("!ask", as("owner")), userFull)
}

// TestGateOwnersSettings: the owners' caps are settings whose ceiling is
// the configured value, and a tightened cap holds.
func TestGateOwnersSettings(t *testing.T) {
	a, _, _, _, _ := ownerGate(t, airut.Gate{PerUser: 1, Window: time.Minute, Owners: &airut.GroupRate{PerUser: 3}})
	for _, c := range []struct{ line, reply string }{
		{"!set agent.for.owners.per_user 4", "agent.for.owners.per_user must be from 1 to 3: a setting may tighten the configured limit, not loosen it."},
		{"!set agent.for.owners.per_user 0", "agent.for.owners.per_user must be from 1 to 3: a setting may tighten the configured limit, not loosen it."},
		{"!set agent.for.owners.per_user 2", "agent.for.owners.per_user: 3 → 2."},
		{"!set agent.for.owners.per_channel 5", "agent.for.owners.per_channel: 0 → 5."},
	} {
		if got := lastText(a.Text(c.line, airuttest.As(owner))); got != c.reply {
			t.Errorf("%s: %q, want %q", c.line, got, c.reply)
		}
	}
	wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
	wantPrefix(t, a.Text("!ask", as("owner")), userFull)
}

// TestGateOwnersSettingsNeedAWindow: an owners' cap set from chat needs a
// positive window, and the window cannot drop to zero under one.
func TestGateOwnersSettingsNeedAWindow(t *testing.T) {
	a, _, _, _, _ := ownerGate(t, airut.Gate{Owners: &airut.GroupRate{}})
	for _, c := range []struct{ line, reply string }{
		{"!set agent.for.owners.per_user 2", "Set agent.window first: agent.for.owners.per_user needs a window to count turns in."},
		{"!set agent.window 1m", "agent.window: 0s → 1m."},
		{"!set agent.for.owners.per_channel 2", "agent.for.owners.per_channel: 0 → 2."},
		{"!set agent.window 0s", "agent.window must be positive while agent.for.owners.per_user or agent.for.owners.per_channel is set."},
	} {
		if got := lastText(a.Text(c.line, airuttest.As(owner))); got != c.reply {
			t.Errorf("%s: %q, want %q", c.line, got, c.reply)
		}
	}
}

func TestGateRefusesNonsenseOwners(t *testing.T) {
	for name, g := range map[string]airut.Gate{
		"an owners' cap with no window": {Owners: &airut.GroupRate{PerUser: 1}},
		"a negative owners' cap":        {Window: time.Minute, Owners: &airut.GroupRate{PerChannel: -1}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("New accepted it")
				}
			}()
			airut.New(airut.Config{Gate: g})
		})
	}
}

// TestChimeFollowUpTakesTheOwnersRate: a follow-up is the sender's, so an
// owner's takes the owners' PerUser, both when Chime asks whether it
// would start and when it does.
func TestChimeFollowUpTakesTheOwnersRate(t *testing.T) {
	limited := make(chan airut.MessageEvent, 4)
	gate := airut.Gate{
		Access:  map[airut.Platform]airut.Access{"test": {Users: []string{"alice"}}},
		PerUser: 1, PerChannel: 1, Window: time.Hour, Owners: &airut.GroupRate{PerUser: 2},
	}
	a, j, ran, b, _ := startChime(t, airut.Config{Agent: &airuttest.Agent{}, Gate: gate, Owners: map[airut.Platform][]string{"test": {"alice"}}},
		airut.ChimeOptions{
			EngagedFor: time.Hour,
			Handler: func(c *airut.Ctx, _ airut.Chimed) error {
				_, err := c.Agent(airut.Turn{Prompt: "follow up"}).Text()
				return err
			},
			OnLimited: func(c *airut.Ctx, ev airut.MessageEvent) error { limited <- ev; return nil },
		})
	a.Text("!roll 1d6", airuttest.As(alice))
	follow := func(text string) {
		t.Helper()
		a.Text(text, airuttest.As(alice))
		got := j.next(t)
		j.answers <- yes(got.Batch[len(got.Batch)-1])
	}
	for _, text := range []string{"and again?", "and once more?"} {
		follow(text)
		if r := waitRan(t, ran); r.err != nil {
			t.Fatalf("the owner's follow-up %q, past the members' PerUser: %v", text, r.err)
		}
	}
	follow("a third?")
	waitOutcome(t, b, "limited", 1)
	if ev := <-limited; ev.Text != "a third?" {
		t.Errorf("OnLimited saw %q", ev.Text)
	}
	noRun(t, ran)
}
