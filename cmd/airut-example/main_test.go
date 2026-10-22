package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/agent/sessions"
	"terva.sh/airut/airuttest"
	"terva.sh/airut/config"
	"terva.sh/airut/matrix"
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
	var names []string
	for _, tl := range tt.Tools {
		names = append(names, tl.Name())
	}
	if tt.Prompt != "roll me a d20" || tt.Session != airut.PerChannel || !reflect.DeepEqual(names, []string{"roll", "settings", "set"}) {
		t.Errorf("turn = %+v, tools %v", tt, names)
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
		n, sides, ok := parseDice(c.in, maxDice)
		if n != c.n || sides != c.sides || ok != c.ok {
			t.Errorf("parseDice(%q) = %d, %d, %v, want %d, %d, %v", c.in, n, sides, ok, c.n, c.sides, c.ok)
		}
	}
}

func TestOneBinaryServesEachConfiguredPlatform(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	platforms := func(cfg *config.Config) []airut.Platform {
		var out []airut.Platform
		for _, a := range adapters(cfg, log, newBot(airut.Config{}, threes)) {
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

// claimsOnly is an Agent that keeps owner claims and runs no turns.
type claimsOnly []airut.Claim

func (claimsOnly) Run(*airut.Ctx, airut.Turn, airut.TurnOutput) error { return nil }

func (c claimsOnly) ClaimedOwners(context.Context) ([]airut.Claim, error) { return c, nil }

func TestInvitesComeOnlyFromOwnersAndNamedUsers(t *testing.T) {
	cfg := &config.Config{Agent: &config.Agent{Access: map[string]config.Access{
		"matrix":  {Users: []string{"@named:example.org"}},
		"discord": {Users: []string{"@stranger:example.org"}},
	}}}
	b := newBot(airut.Config{
		Owners: map[airut.Platform][]string{airut.Matrix: {"@listed:example.org"}, airut.Discord: {"@discord-owner:example.org"}},
		Agent:  claimsOnly{{Platform: airut.Matrix, ID: "@claimed:example.org"}},
	}, threes)
	accept := inviters(cfg, b)
	ctx := context.Background()
	for _, direct := range []bool{false, true} {
		for inviter, want := range map[string]bool{
			"@named:example.org":         true,
			"@listed:example.org":        true,
			"@claimed:example.org":       true,
			"@stranger:example.org":      false, // named only for Discord
			"@discord-owner:example.org": false, // an owner only on Discord
			"@nobody:example.org":        false,
		} {
			got, err := accept(ctx, matrix.Invite{Room: "!r:example.org", Inviter: inviter, Direct: direct})
			if got != want || err != nil {
				t.Errorf("an invite from %s, direct %v: %v, %v; want %v", inviter, direct, got, err, want)
			}
		}
	}
	none := inviters(&config.Config{}, newBot(airut.Config{}, threes))
	if ok, err := none(ctx, matrix.Invite{Room: "!r:example.org", Inviter: "@named:example.org"}); ok || err != nil {
		t.Errorf("an invite with no [agent] table and no owners: %v, %v", ok, err)
	}
}

// TestReadKeyStopsWithItsContext reads a key from a pipe, and gives up on
// one that never comes when the context is cancelled.
func TestReadKeyStopsWithItsContext(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := readKey(ctx, r, io.Discard, "")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled read = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read went on after its context was cancelled")
	}

	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	go func() {
		_, _ = io.WriteString(w2, "  EsTc abcd  \n")
		w2.Close()
	}()
	if key, err := readKey(context.Background(), r2, io.Discard, ""); key != "EsTc abcd" || err != nil {
		t.Errorf("readKey = %q, %v", key, err)
	}
}

func TestMaxDiceSetting(t *testing.T) {
	a := airuttest.Start(t, newBot(airut.Config{Settings: airut.Settings{Values: map[string]string{"roll.max_dice": "2"}}}, threes))
	if got := texts(a.Text("!roll 3d6")); !reflect.DeepEqual(got, []string{"reply:Roll dice written like 2d6: up to 2 dice of 2 to 1000 sides."}) {
		t.Errorf("roll 3d6 under roll.max_dice 2 = %q", got)
	}
	if got := texts(a.Text("!roll 2d6")); !reflect.DeepEqual(got, []string{"reply:2d6: 3 + 3 = 6"}) {
		t.Errorf("roll 2d6 = %q", got)
	}
	if got := texts(a.Text("!settings roll.max_dice")); len(got) != 1 || !strings.HasPrefix(got[0], "reply:roll.max_dice = 2, from the config.") {
		t.Errorf("settings roll.max_dice = %q", got)
	}
}

func TestOwners(t *testing.T) {
	// The file's [bot] owners reach the core through botConfig. The fake
	// adapter's platform is not one a file may name, so the test sets it
	// the way ByPlatform would.
	cfg := &config.Config{Bot: config.Bot{Owners: map[string][]string{string(airuttest.Platform): {"owner"}}}}
	bc := botConfig(cfg, slog.New(slog.DiscardHandler))
	if !reflect.DeepEqual(bc.Owners, map[airut.Platform][]string{airuttest.Platform: {"owner"}}) || bc.ProfileEditors == nil || bc.Settings.Owner == nil {
		t.Fatalf("botConfig = owners %q, profile editors set %v, settings owner set %v", bc.Owners, bc.ProfileEditors != nil, bc.Settings.Owner != nil)
	}
	b := newBot(bc, threes)
	a := airuttest.Start(t, b)
	if got := texts(a.Text("!profile name Mallory")); !reflect.DeepEqual(got, []string{"reply:You are not allowed to use !profile."}) {
		t.Errorf("someone else's profile = %q", got)
	}
	// Nobody may change a setting without a store, but the refusal tells
	// an owner from anyone else.
	if got := texts(a.Text("!set roll.max_dice 3", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "owner"}))); !reflect.DeepEqual(got,
		[]string{"reply:Settings cannot be changed here: this bot has no store to keep them in."}) {
		t.Errorf("an owner's set = %q", got)
	}
	if got := texts(a.Text("!set roll.max_dice 3")); !reflect.DeepEqual(got, []string{"reply:You are not allowed to change roll.max_dice."}) {
		t.Errorf("someone else's set = %q", got)
	}
}

// The example mounts claim and unclaim as its own subcommands, which
// record the claim in the session store the file names.
func TestClaimSubcommand(t *testing.T) {
	if _, ok := subcommand([]string{"-config", "x"}, io.Discard, io.Discard); ok {
		t.Error("a flag was taken for a subcommand")
	}
	if _, ok := subcommand(nil, io.Discard, io.Discard); ok {
		t.Error("no arguments were taken for a subcommand")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "airut.toml")
	src := "[discord]\ntoken = { env = \"NO_SUCH_TOKEN\" }\n[agent]\nmodel = \"local/m\"\n[store]\npath = \"sessions.db\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code, ok := subcommand([]string{"claim", "-config", path, "-actor", "ops", "123"}, &out, &errOut)
	if !ok || code != 0 || !strings.Contains(out.String(), "claimed discord 123") {
		t.Fatalf("airut-example claim = %d, %v, %q %q", code, ok, out.String(), errOut.String())
	}
	errOut.Reset()
	if code, _ := subcommand([]string{"claim"}, io.Discard, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "usage: airut-example claim") || !strings.Contains(errOut.String(), defaultConfig) {
		t.Errorf("airut-example claim with no user = %d, %q", code, errOut.String())
	}

	db, err := sessions.Open(context.Background(), filepath.Join(dir, "sessions.db"), sessions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, err := db.OwnerClaims(context.Background())
	if err != nil || len(list) != 1 || list[0].Platform != "discord" || list[0].ID != "123" || list[0].Actor != "ops" {
		t.Errorf("the store's claims = %+v, %v", list, err)
	}
	out.Reset()
	if code, _ := subcommand([]string{"unclaim", "-config", path, "discord", "123"}, &out, io.Discard); code != 0 || !strings.Contains(out.String(), "unclaimed discord 123") {
		t.Errorf("airut-example unclaim = %d, %q", code, out.String())
	}
}
