package airut_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

var dana = airuttest.As(airut.User{Platform: airuttest.Platform, ID: "dana"})

// modBot has a tag command only members of the mods channel may use, a
// search command open to all, and an ask command that starts a turn
// naming both.
func modBot(cfg airut.Config, check airut.AccessCheck) (*airut.Bot, *lockedBuffer) {
	b, logs := newBot(cfg)
	b.Command("tag", "Apply tags", func(c *airut.Ctx) error {
		_, err := c.Reply("tagged")
		return err
	}).AsTool().RestrictTo(check)
	b.Command("search", "Search", func(c *airut.Ctx) error {
		_, err := c.Reply("found")
		return err
	}).AsTool()
	b.Command("ask", "Ask the agent", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Tools: b.Tools("tag", "search")}).StreamTo(c)
	})
	return b, logs
}

func toolNames(t airut.Turn) string {
	var names []string
	for _, tl := range t.Tools {
		names = append(names, tl.Name())
	}
	return strings.Join(names, ",")
}

func TestRestrictedCommandsInvokedDirectly(t *testing.T) {
	b, logs := modBot(airut.Config{}, airut.MemberOf(mods))
	a := airuttest.Start(t, b)
	a.SetMembers("mods", "dana")

	wantEvents(t, a.Text("!tag", dana), "reply:tagged")
	wantEvents(t, a.Text("!tag"), "reply:You are not allowed to use !tag.")
	wantEvents(t, a.Text("!search"), "reply:found")
	if !strings.Contains(logs.String(), "command refused") {
		t.Errorf("the refusal was not logged:\n%s", logs.String())
	}
	// A caller on another platform is never a member of a channel here.
	other := airuttest.As(airut.User{Platform: airut.Discord, ID: "dana"})
	wantEvents(t, a.Text("!tag", other), "reply:You are not allowed to use !tag.")
}

func TestRestrictedToolsAreLeftOutOfTheTurn(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Call: "tag", Args: `{}`}}}
	b, _ := modBot(airut.Config{Gate: openGate, Agent: agent}, airut.MemberOf(mods))
	a := airuttest.Start(t, b)
	a.SetMembers("mods", "dana")

	a.Text("!ask", dana)
	a.Text("!ask")
	turns := agent.Turns()
	if len(turns) != 2 || toolNames(turns[0]) != "tag,search" || toolNames(turns[1]) != "search" {
		t.Fatalf("the turns were offered %q; want the moderator offered tag and search, and user-1 search alone", []string{toolNames(turns[0]), toolNames(turns[1])})
	}
	if got := agent.Results(); len(got) != 2 || got[0] != "tagged" || !strings.HasPrefix(got[1], "refused: the turn does not name tool tag") {
		t.Errorf("tool results = %q", got)
	}
}

func TestRestrictedToolsAreCheckedAgainWhenCalled(t *testing.T) {
	// The check allows once, when the turn starts, and refuses after:
	// a moderator who left the room mid-turn.
	var calls atomic.Int32
	once := func(*airut.Ctx) (bool, error) { return calls.Add(1) == 1, nil }
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Call: "tag", Args: `{}`}}}
	b, _ := modBot(airut.Config{Gate: openGate, Agent: agent}, once)
	a := airuttest.Start(t, b)

	evs := a.Text("!ask", dana)
	if got := agent.Results(); len(got) != 1 || !strings.Contains(got[0], "You are not allowed to use !tag.") {
		t.Errorf("the second check did not refuse the call: %q", got)
	}
	for _, e := range evs {
		if strings.Contains(e.Text, "tagged") {
			t.Errorf("the refused tool ran: %+v", e)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the check ran %d times, want once for the offer and once for the call", n)
	}
	var rf *airut.Refusal
	if _, err := b.Tools("tag")[0].Call(nil, nil); !errors.As(err, &rf) || rf.Reason != airut.RefusedAccess {
		t.Errorf("a refused Call returned %v, want an access Refusal", err)
	}
}

func TestAccessCheckErrors(t *testing.T) {
	broken := func(*airut.Ctx) (bool, error) {
		return false, errors.New("the member list at secret-host is unreachable")
	}
	agent := &airuttest.Agent{}
	b, logs := modBot(airut.Config{Gate: openGate, Agent: agent}, broken)
	a := airuttest.Start(t, b)

	// A check that fails is not a refusal: the caller may well be allowed.
	wantEvents(t, a.Text("!tag", dana), "reply:Something went wrong running tag.")
	a.Text("!ask", dana)
	if turns := agent.Turns(); len(turns) != 1 || toolNames(turns[0]) != "search" {
		t.Errorf("a tool whose check failed was offered: %+v", turns)
	}
	l := logs.String()
	for _, want := range []string{"command access check failed", "tool access check failed", "tool=tag"} {
		if !strings.Contains(l, want) {
			t.Errorf("the log lacks %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "secret-host") {
		t.Errorf("the log holds the check's error text:\n%s", l)
	}
}

func TestAccessHelpers(t *testing.T) {
	listed := airut.Listed(map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"dana"}, Roles: []string{"mod"}}})
	fail := func(*airut.Ctx) (bool, error) { return false, errors.New("down") }
	cases := []struct {
		name  string
		check airut.AccessCheck
		opts  []airuttest.Option
		want  string
	}{
		{"listed user", listed, []airuttest.Option{dana}, "reply:tagged"},
		{"listed role", listed, []airuttest.Option{airuttest.WithRoles(airut.Role{Platform: airuttest.Platform, ID: "mod"})}, "reply:tagged"},
		{"not listed", listed, nil, "reply:You are not allowed to use !tag."},
		{"any of, second allows", airut.AnyOf(airut.MemberOf(mods), listed), []airuttest.Option{dana}, "reply:tagged"},
		{"any of, an error and an allow", airut.AnyOf(fail, listed), []airuttest.Option{dana}, "reply:tagged"},
		{"any of, an error and a refusal", airut.AnyOf(fail, listed), nil, "reply:Something went wrong running tag."},
		{"any of, refusals", airut.AnyOf(airut.MemberOf(mods), listed), nil, "reply:You are not allowed to use !tag."},
		{"a member of a listed channel", airut.Listed(map[airut.Platform]airut.Access{airuttest.Platform: {Channels: []string{"mods"}}}),
			[]airuttest.Option{dana}, "reply:tagged"},
		{"not a member of a listed channel", airut.Listed(map[airut.Platform]airut.Access{airuttest.Platform: {Channels: []string{"mods"}}}),
			nil, "reply:You are not allowed to use !tag."},
	}
	for _, c := range cases {
		b, _ := modBot(airut.Config{}, c.check)
		a := airuttest.Start(t, b)
		a.SetMembers("mods", "dana")
		evs := a.Text("!tag", c.opts...)
		if len(evs) != 1 || evs[0].Kind+":"+evs[0].Text != c.want {
			t.Errorf("%s: got %q, want %q", c.name, texts(evs), c.want)
		}
	}
}

// plainTool is a Tool that is not a Command and restricts itself.
type plainTool struct {
	checks   atomic.Int32
	approve  bool
	ran      atomic.Int32
	allowFor int32 // how many checks allow before the rest refuse
}

func (p *plainTool) Name() string            { return "plain" }
func (p *plainTool) Description() string     { return "A plain tool" }
func (p *plainTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *plainTool) NeedsApproval() bool     { return p.approve }
func (p *plainTool) Allows(*airut.Ctx) (bool, error) {
	return p.checks.Add(1) <= p.allowFor, nil
}
func (p *plainTool) Call(*airut.Ctx, json.RawMessage) (string, error) {
	p.ran.Add(1)
	return "ran", nil
}

func TestPlainRestrictedToolsAreCheckedAgainWhenCalled(t *testing.T) {
	pt := &plainTool{allowFor: 1, approve: true}
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Call: "plain", Args: `{}`}}}
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent})
	b.Command("ask", "Ask the agent", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Tools: []airut.Tool{pt}}).StreamTo(c)
	})
	a := airuttest.Start(t, b)
	a.Text("!ask")

	if got := agent.Results(); len(got) != 1 || !strings.Contains(got[0], "You are not allowed to use plain.") {
		t.Errorf("the call after access was lost returned %q", got)
	}
	if pt.ran.Load() != 0 {
		t.Error("the plain tool ran after its check refused")
	}
	turns := agent.Turns()
	if len(turns) != 1 || len(turns[0].Tools) != 1 {
		t.Fatalf("turns = %+v", turns)
	}
	if at, ok := turns[0].Tools[0].(airut.ApprovalTool); !ok || !at.NeedsApproval() {
		t.Error("the offered tool lost the plain tool's need for approval")
	}
}
