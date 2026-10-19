package airut_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// gateBot is a bot whose "ask" command starts an agent turn with budget.
func gateBot(t *testing.T, g airut.Gate, budget airut.Budget) (*airut.Bot, *airuttest.Agent, *lockedBuffer) {
	t.Helper()
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Text: "answer"}}}
	b, logs := newBot(airut.Config{Agent: agent, Gate: g})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "a secret prompt", Budget: budget}).StreamTo(c)
	})
	return b, agent, logs
}

func TestGateAccess(t *testing.T) {
	const denied = "reply:You are not allowed to start an agent turn on this bot."
	for _, c := range []struct {
		name   string
		access map[airut.Platform]airut.Access
		opts   []airuttest.Option
		want   string
	}{
		{"no gate refuses everyone", nil, nil, denied},
		{"an empty list refuses everyone", map[airut.Platform]airut.Access{airuttest.Platform: {}}, nil, denied},
		{"a listed user is allowed", map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1"}}}, nil, "reply:answer"},
		{"another user is refused", map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-2"}}}, nil, denied},
		{
			"a listed role is allowed",
			map[airut.Platform]airut.Access{airuttest.Platform: {Roles: []string{"mods"}}},
			[]airuttest.Option{airuttest.WithRoles(airut.Role{Platform: airuttest.Platform, ID: "mods"})},
			"reply:answer",
		},
		{
			"a role id is not a user id",
			map[airut.Platform]airut.Access{airuttest.Platform: {Roles: []string{"user-1"}}},
			nil, denied,
		},
		{
			"another platform's list does not count",
			map[airut.Platform]airut.Access{airut.Matrix: {Users: []string{"user-1"}}},
			nil, denied,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, agent, logs := gateBot(t, airut.Gate{Access: c.access}, airut.Budget{})
			a := airuttest.Start(t, b)
			wantEvents(t, a.Text("!ask", c.opts...), c.want)
			if c.want != denied {
				return
			}
			if len(agent.Turns()) != 0 {
				t.Error("a refused turn reached the agent")
			}
			l := logs.String()
			if !strings.Contains(l, "agent turn refused") || !strings.Contains(l, "reason=access") || !strings.Contains(l, "kind=refused_access") {
				t.Errorf("the refusal was not logged by reason:\n%s", l)
			}
			if strings.Contains(l, "secret prompt") || strings.Contains(l, "not allowed") {
				t.Errorf("the log holds chat text:\n%s", l)
			}
		})
	}
}

// TestGateChannelMembers: the members of a listed channel may start a
// turn, and only they; a failed membership check refuses and is logged by
// kind; and a listed user is admitted without the member list being read.
func TestGateChannelMembers(t *testing.T) {
	const denied = "reply:You are not allowed to start an agent turn on this bot."
	g := airut.Gate{Access: map[airut.Platform]airut.Access{
		airuttest.Platform: {Users: []string{"owner"}, Channels: []string{"main", "mods"}},
	}}
	b, agent, logs := gateBot(t, g, airut.Budget{})
	a := airuttest.Start(t, b)
	a.SetMembers("main", "user-1")
	a.SetMembers("mods", "dana")
	as := func(id string) airuttest.Option {
		return airuttest.As(airut.User{Platform: airuttest.Platform, ID: id})
	}
	wantEvents(t, a.Text("!ask"), "reply:answer")
	wantEvents(t, a.Text("!ask", as("dana")), "reply:answer")
	wantEvents(t, a.Text("!ask", as("stranger")), denied)
	if n := len(agent.Turns()); n != 2 {
		t.Errorf("%d turns reached the agent, want the two members'", n)
	}

	// The first channel's list cannot be read: a member of the second is
	// still admitted, and everyone else is refused, never let through.
	// The failure is logged even when the second channel admits.
	a.FailMember("main", errors.New("the member list at secret-host is unreachable"))
	wantEvents(t, a.Text("!ask", as("dana")), "reply:answer")
	if l := logs.String(); !strings.Contains(l, "channel membership check failed") || !strings.Contains(l, "channel=main") {
		t.Errorf("a failed check was not logged when another channel admitted the caller:\n%s", l)
	}
	wantEvents(t, a.Text("!ask"), denied)
	if l := logs.String(); strings.Contains(l, "secret-host") {
		t.Errorf("the failed check was logged with its text:\n%s", l)
	}

	// A listed user needs no member list, even one that cannot be read.
	a.FailMember("mods", errors.New("down"))
	wantEvents(t, a.Text("!ask", as("owner")), "reply:answer")
}

func TestGateDefaultBudget(t *testing.T) {
	g := openGate
	g.Budget = airut.Budget{Turns: 3, Tokens: 500}
	for _, c := range []struct {
		set, want airut.Budget
	}{
		{airut.Budget{}, airut.Budget{Turns: 3, Tokens: 500}},
		{airut.Budget{Turns: 1}, airut.Budget{Turns: 1, Tokens: 500}},
		{airut.Budget{Tokens: 40}, airut.Budget{Turns: 3, Tokens: 40}},
		{airut.Budget{Turns: 9, Tokens: 9}, airut.Budget{Turns: 9, Tokens: 9}},
	} {
		b, agent, _ := gateBot(t, g, c.set)
		airuttest.Start(t, b).Text("!ask")
		if turns := agent.Turns(); len(turns) != 1 || turns[0].Budget != c.want {
			t.Errorf("a turn setting %+v reached the agent as %+v, want %+v", c.set, turns, c.want)
		}
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestGateRateLimit(t *testing.T) {
	g := airut.Gate{
		Access:     map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1", "user-2", "user-3"}}},
		PerUser:    2,
		PerChannel: 3,
		Window:     time.Minute,
	}
	b, agent, logs := gateBot(t, g, airut.Budget{})
	clock := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	airut.SetGateClock(b, clock.now)
	a := airuttest.Start(t, b)
	as := func(id string) airuttest.Option {
		return airuttest.As(airut.User{Platform: airuttest.Platform, ID: id})
	}
	elsewhere := airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "channel-2"})

	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(20 * time.Second)
	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(5 * time.Second)
	// The first turn leaves the window 35 seconds from now.
	wantEvents(t, a.Text("!ask"), "reply:You have started too many agent turns. Try again in 35 seconds.")
	// The cap is the caller's, wherever they ask.
	wantEvents(t, a.Text("!ask", elsewhere), "reply:You have started too many agent turns. Try again in 35 seconds.")

	// Another caller fills the channel's third slot, and then the channel is full.
	wantEvents(t, a.Text("!ask", as("user-2")), "reply:answer")
	wantEvents(t, a.Text("!ask", as("user-3")), "reply:Too many agent turns have started in this channel. Try again in 35 seconds.")
	// The refusals counted against nobody: user-3 can still ask elsewhere.
	wantEvents(t, a.Text("!ask", as("user-3"), elsewhere), "reply:answer")

	clock.advance(35 * time.Second)
	wantEvents(t, a.Text("!ask"), "reply:answer")
	if n := len(agent.Turns()); n != 5 {
		t.Errorf("%d turns reached the agent, want the 5 allowed", n)
	}
	if !strings.Contains(logs.String(), "reason=rate") {
		t.Errorf("no rate refusal was logged:\n%s", logs.String())
	}
}

// TestGateWaitCoversBothCaps starts a turn when both caps are full with
// different release times: the wait given must be the longer one.
func TestGateWaitCoversBothCaps(t *testing.T) {
	g := airut.Gate{
		Access:     map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1", "user-2", "user-3"}}},
		PerUser:    1,
		PerChannel: 2,
		Window:     time.Minute,
	}
	b, _, _ := gateBot(t, g, airut.Budget{})
	clock := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	airut.SetGateClock(b, clock.now)
	a := airuttest.Start(t, b)
	as := func(id string) airuttest.Option {
		return airuttest.As(airut.User{Platform: airuttest.Platform, ID: id})
	}
	elsewhere := airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "channel-2"})
	const channelFull = "reply:Too many agent turns have started in this channel. Try again in "
	const userFull = "reply:You have started too many agent turns. Try again in "

	wantEvents(t, a.Text("!ask", elsewhere), "reply:answer") // user-1 at 0s
	clock.advance(20 * time.Second)
	wantEvents(t, a.Text("!ask", as("user-2")), "reply:answer") // channel-1 at 20s
	clock.advance(10 * time.Second)
	wantEvents(t, a.Text("!ask", as("user-3")), "reply:answer") // channel-1 at 30s
	clock.advance(10 * time.Second)
	// At 40s user-1 is free at 60s, but channel-1 not until 80s.
	wantEvents(t, a.Text("!ask"), channelFull+"40 seconds.")
	clock.advance(20 * time.Second)
	wantEvents(t, a.Text("!ask"), channelFull+"20 seconds.")
	clock.advance(20 * time.Second)
	wantEvents(t, a.Text("!ask"), "reply:answer") // user-1 in channel-1 at 80s

	// With only the caller's cap full, its own wait is given.
	clock.advance(50 * time.Second) // 130s: channel-1 holds only user-1's 80s turn
	wantEvents(t, a.Text("!ask", as("user-2")), "reply:answer")
	wantEvents(t, a.Text("!ask", elsewhere), userFull+"10 seconds.")
}

func TestGateWaitIsReadable(t *testing.T) {
	g := openGate
	g.PerUser, g.Window = 1, 90*time.Minute
	b, _, _ := gateBot(t, g, airut.Budget{})
	clock := &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	airut.SetGateClock(b, clock.now)
	a := airuttest.Start(t, b)
	a.Text("!ask")
	for _, c := range []struct {
		advance time.Duration
		want    string
	}{
		{0, "90 minutes"},
		{29*time.Minute + 30*time.Second + time.Millisecond, "60 minutes 30 seconds"},
		{59*time.Minute + 29*time.Second + 999*time.Millisecond, "1 minute"},
		{59*time.Second + 500*time.Millisecond, "1 second"},
	} {
		clock.advance(c.advance)
		want := "reply:You have started too many agent turns. Try again in " + c.want + "."
		wantEvents(t, a.Text("!ask"), want)
	}
}

func TestGateRefusesNonsense(t *testing.T) {
	for name, g := range map[string]airut.Gate{
		"a per-user cap with no window":    {PerUser: 1},
		"a per-channel cap with no window": {PerChannel: 1},
		"a negative cap":                   {PerUser: -1, Window: time.Minute},
		"a negative budget":                {Budget: airut.Budget{Tokens: -1}},
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
