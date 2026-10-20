package main

import (
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
	"terva.sh/airut/config"
)

// threes is a die that always lands on 3.
func threes(int) int { return 3 }

func texts(evs []airuttest.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind+":"+e.Text)
	}
	return out
}

func TestRoll(t *testing.T) {
	a := airuttest.Start(t, newBot(airut.Config{}, threes))
	if got, want := texts(a.Slash("roll", map[string]any{"dice": "2d6"})), []string{"reply:2d6: 3 + 3 = 6"}; !reflect.DeepEqual(got, want) {
		t.Errorf("roll 2d6 = %q, want %q", got, want)
	}
	if got, want := texts(a.Text("!roll d20")), []string{"reply:d20: 3 = 3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("!roll d20 = %q, want %q", got, want)
	}
	evs := a.Text("!roll 500d6")
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "up to 20 dice") {
		t.Errorf("!roll 500d6 = %q, want the dice rule", texts(evs))
	}
}

func TestAskRollsThroughTheAgent(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{
		{Text: "Rolling. "},
		{Call: "roll", Args: `{"dice":"1d20"}`},
		{Text: "You got 3."},
	}}
	gate := airut.Gate{Access: map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1"}}}}
	a := airuttest.Start(t, newBot(airut.Config{Agent: agent, Gate: gate}, threes))
	evs := a.Text("!ask roll me a d20")
	if len(evs) == 0 || !strings.HasSuffix(evs[len(evs)-1].Text, "Rolling. You got 3.") {
		t.Errorf("ask = %q, want the answer in the chat", texts(evs))
	}
	if got, want := agent.Results(), []string{"1d20: 3 = 3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tool results = %q, want %q", got, want)
	}
	turns := agent.Turns()
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	tt := turns[0]
	if tt.Prompt != "roll me a d20" || tt.Session != airut.PerChannel || len(tt.Tools) != 1 || tt.Tools[0].Name() != "roll" {
		t.Errorf("turn = %+v", tt)
	}
	if _, ok := personas[tt.Persona]; !ok {
		t.Errorf("the turn names persona %q, which personas lacks", tt.Persona)
	}
}

func TestAskWithoutAnAgent(t *testing.T) {
	a := airuttest.Start(t, newBot(airut.Config{}, threes))
	evs := a.Text("!ask anything")
	if len(evs) != 1 || evs[0].Kind != "reply" {
		t.Errorf("ask without an agent = %q, want one reply", texts(evs))
	}
}

func TestParseDice(t *testing.T) {
	for _, c := range []struct {
		in       string
		n, sides int
		ok       bool
	}{
		{"2d6", 2, 6, true},
		{"d20", 1, 20, true},
		{" 3D8 ", 3, 8, true},
		{"20d1000", 20, 1000, true},
		{"21d6", 0, 0, false},
		{"2d1", 0, 0, false},
		{"2d1001", 0, 0, false},
		{"0d6", 0, 0, false},
		{"-1d6", 0, 0, false},
		{"2x6", 0, 0, false},
		{"d", 0, 0, false},
		{"", 0, 0, false},
	} {
		n, sides, ok := parseDice(c.in)
		if n != c.n || sides != c.sides || ok != c.ok {
			t.Errorf("parseDice(%q) = %d, %d, %v, want %d, %d, %v", c.in, n, sides, ok, c.n, c.sides, c.ok)
		}
	}
}

func TestOneBinaryServesEachConfiguredPlatform(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	platforms := func(cfg *config.Config) []airut.Platform {
		var out []airut.Platform
		for _, a := range adapters(cfg, log) {
			out = append(out, a.Platform())
		}
		return out
	}
	both := &config.Config{Discord: &config.Discord{}, Matrix: &config.Matrix{}}
	if got, want := platforms(both), []airut.Platform{airut.Discord, airut.Matrix}; !reflect.DeepEqual(got, want) {
		t.Errorf("both tables serve %v, want %v", got, want)
	}
	if got := platforms(&config.Config{Matrix: &config.Matrix{}}); !reflect.DeepEqual(got, []airut.Platform{airut.Matrix}) {
		t.Errorf("a matrix table alone serves %v", got)
	}
	if got := platforms(&config.Config{}); len(got) != 0 {
		t.Errorf("no tables serve %v", got)
	}
}

func TestInvitesComeOnlyFromNamedUsers(t *testing.T) {
	cfg := &config.Config{Agent: &config.Agent{Access: map[string]config.Access{
		"matrix":  {Users: []string{"@owner:example.org"}},
		"discord": {Users: []string{"@stranger:example.org"}},
	}}}
	accept := inviters(cfg)
	if !accept("!r:example.org", "@owner:example.org") {
		t.Error("refused an invite from a named user")
	}
	if accept("!r:example.org", "@stranger:example.org") {
		t.Error("accepted an invite from a user named only for Discord")
	}
	if inviters(&config.Config{})("!r:example.org", "@owner:example.org") {
		t.Error("accepted an invite with no [agent] table")
	}
}
