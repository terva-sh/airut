package airut_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// settingStore is a scripted agent that keeps setting overrides and
// changes in memory.
type settingStore struct {
	airuttest.Agent
	mu        sync.Mutex
	overrides map[string]string
	changes   []airut.SettingChange
	fail      error
}

func (s *settingStore) SettingOverrides(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.overrides {
		out[k] = v
	}
	return out, s.fail
}

func (s *settingStore) SaveSetting(_ context.Context, ch airut.SettingChange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.overrides == nil {
		s.overrides = map[string]string{}
	}
	if ch.Unset {
		delete(s.overrides, ch.Name)
	} else {
		s.overrides[ch.Name] = ch.New
	}
	s.changes = append(s.changes, ch)
	return nil
}

func (s *settingStore) SettingChanges(_ context.Context, name string, limit int) ([]airut.SettingChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []airut.SettingChange
	for i := len(s.changes) - 1; i >= 0 && len(out) < limit; i-- {
		if s.changes[i].Name == name {
			out = append(out, s.changes[i])
		}
	}
	return out, nil
}

func (s *settingStore) saved() []airut.SettingChange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.changes)
}

var (
	owner     = airut.User{Platform: "test", ID: "owner", Name: "Olive"}
	moderator = airut.User{Platform: "test", ID: "mod", Name: "Mod"}
	isOwner   = func(c *airut.Ctx) (bool, error) { return c.Caller().ID == "owner", nil }
	isMod     = func(c *airut.Ctx) (bool, error) { return c.Caller().ID == "mod", nil }
	notices   = airut.Channel{Platform: "test", ID: "mods-room"}
)

// declared are the settings settingsBot declares.
type declared struct {
	open   *airut.Setting[int]
	expire *airut.Setting[time.Duration]
	quiet  *airut.Setting[bool]
	greet  *airut.Setting[string]
	mode   *airut.Setting[string]
	secret *airut.Setting[int]
}

// settingsBot runs a bot with one setting of each kind, owned by
// "owner", with "mod" allowed to change proposals.max_open, and
// hidden.knob readable by owners only. Its "ask" command starts a turn
// with the settings tools.
func settingsBot(t *testing.T, st airut.SettingStore, ag airut.Agent, cfg airut.Config) (*airuttest.Adapter, *declared, *lockedBuffer) {
	t.Helper()
	cfg.Agent = ag
	cfg.Gate = airut.Gate{Access: map[airut.Platform]airut.Access{"test": {Users: []string{"owner", "mod", "user-1"}}},
		PerUser: 5, Window: time.Minute, Budget: airut.Budget{Turns: 8, Tokens: 1000}}
	cfg.Settings.Owner = isOwner
	cfg.Settings.Store = st
	b, logs := newBot(cfg)
	d := &declared{
		open: b.DeclareInt(airut.IntSetting{Name: "proposals.max_open", Description: "Open proposals at once",
			Default: 10, Min: 1, Max: 50, Change: airut.AnyOf(isOwner, isMod)}),
		expire: b.DeclareDuration(airut.DurationSetting{Name: "proposals.expire_after", Description: "How long a proposal stays open",
			Default: 72 * time.Hour, Min: time.Hour, Max: 720 * time.Hour}),
		quiet: b.DeclareBool(airut.BoolSetting{Name: "quiet", Description: "Answer less"}),
		greet: b.DeclareString(airut.StringSetting{Name: "greeting", Description: "What the bot says hello with",
			Default: "hello", MaxLen: 30}),
		mode: b.DeclareEnum(airut.EnumSetting{Name: "search.mode", Description: "How search matches",
			Default: "any", Choices: []string{"any", "all"}}),
		secret: b.DeclareInt(airut.IntSetting{Name: "hidden.knob", Description: "Something only owners see",
			Default: 1, Min: 0, Max: 9, Read: isOwner}),
	}
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "hi", Tools: b.Tools("settings", "set", "unset")}).StreamTo(c)
	})
	return airuttest.Start(t, b), d, logs
}

func TestSettingLayers(t *testing.T) {
	st := &settingStore{overrides: map[string]string{
		"proposals.max_open": "12",
		"search.mode":        "nope", // no longer valid
		"gone.setting":       "1",    // no longer declared
	}}
	_, d, logs := settingsBot(t, st, st, airut.Config{Settings: airut.Settings{Values: map[string]string{
		"proposals.max_open": "20", "quiet": "on", "search.mode": "all",
	}}})
	if got := d.open.Get(); got != 12 {
		t.Errorf("proposals.max_open = %d, want the override, 12", got)
	}
	if got := d.quiet.Get(); !got {
		t.Error("quiet is off, want the config's on")
	}
	if got := d.mode.Get(); got != "all" {
		t.Errorf("search.mode = %q, want the config's all, since the override no longer validates", got)
	}
	if got, want := d.expire.Get(), 72*time.Hour; got != want {
		t.Errorf("proposals.expire_after = %v, want the default %v", got, want)
	}
	if d.open.Name() != "proposals.max_open" {
		t.Errorf("Name() = %q", d.open.Name())
	}
	l := logs.String()
	if !strings.Contains(l, "stored setting ignored: its value is no longer allowed") || !strings.Contains(l, "setting=search.mode") {
		t.Errorf("the invalid override was not logged by name:\n%s", l)
	}
	if !strings.Contains(l, "no setting of that name is declared") || strings.Contains(l, "gone.setting") {
		t.Errorf("the undeclared override was not logged by size only:\n%s", l)
	}
}

func TestSettingsListing(t *testing.T) {
	st := &settingStore{overrides: map[string]string{"proposals.max_open": "12"}}
	a, _, _ := settingsBot(t, st, st, airut.Config{Settings: airut.Settings{Values: map[string]string{"quiet": "true"}}})
	evs := a.Text("!settings", airuttest.As(tester))
	if len(evs) != 1 || !evs[0].Private {
		t.Fatalf("settings answered %+v, want one private reply", evs)
	}
	want := strings.Join([]string{
		"agent.max_tokens = 1000 (config). Tokens an agent turn may spend when it sets no budget",
		"agent.max_turns = 8 (config). Model requests an agent turn may make when it sets no budget",
		"agent.per_channel = 0 (config). Agent turns one channel may start in each window",
		"agent.per_user = 5 (config). Agent turns one person may start in each window",
		"agent.window = 1m (config). The window agent.per_user and agent.per_channel count turns in",
		"greeting = hello (default). What the bot says hello with",
		"proposals.expire_after = 72h (default). How long a proposal stays open",
		"proposals.max_open = 12 (override). Open proposals at once",
		"quiet = true (config). Answer less",
		"search.mode = any (default). How search matches",
		"Show one with: settings NAME",
	}, "\n")
	if evs[0].Text != want {
		t.Errorf("listing:\n%s\nwant:\n%s", evs[0].Text, want)
	}
	if got := lastText(a.Text("!settings", airuttest.As(owner))); !strings.Contains(got, "hidden.knob = 1 (default)") {
		t.Errorf("an owner's listing leaves out the setting only owners read:\n%s", got)
	}
	for _, line := range []string{"!settings hidden.knob", "!set hidden.knob 2", "!unset hidden.knob", "!settings no.such"} {
		got := lastText(a.Text(line, airuttest.As(tester)))
		name := strings.Fields(line)[1]
		if got != "There is no setting "+name+"." {
			t.Errorf("%s answered %q, as if the setting were not there", line, got)
		}
	}
}

func TestSetAndUnset(t *testing.T) {
	st := &settingStore{}
	cfg := airut.Config{Settings: airut.Settings{Values: map[string]string{"proposals.max_open": "20"}, Notices: []airut.Channel{notices}}}
	a, d, logs := settingsBot(t, st, st, cfg)

	evs := a.Text("!set proposals.max_open 15", airuttest.As(moderator))
	if got := texts(evs); !slices.Contains(got, "reply:proposals.max_open: 20 → 15.") {
		t.Errorf("set answered %q", got)
	}
	if d.open.Get() != 15 {
		t.Errorf("proposals.max_open = %d after set, want 15", d.open.Get())
	}
	sent := a.Events()
	if !slices.ContainsFunc(sent, func(e airuttest.Event) bool {
		return e.Kind == "send" && e.Channel == "mods-room" && e.Text == "Mod changed setting proposals.max_open from 20 to 15."
	}) {
		t.Errorf("no notice in mods-room: %+v", sent)
	}
	ch := st.saved()
	if len(ch) != 1 || ch[0].Name != "proposals.max_open" || ch[0].Old != "20" || ch[0].New != "15" || ch[0].Unset ||
		ch[0].By != moderator || ch[0].Channel.ID != "channel-1" || ch[0].Source != airut.ByCommand || ch[0].At.IsZero() {
		t.Errorf("saved %+v", ch)
	}

	for _, c := range []struct{ line, as, reply string }{
		{"!set proposals.max_open 99", "owner", "proposals.max_open must be a whole number from 1 to 50."},
		{"!set proposals.max_open lots", "owner", "proposals.max_open must be a whole number from 1 to 50."},
		{"!set proposals.max_open 15", "owner", "proposals.max_open is already 15."},
		{"!set proposals.expire_after 1h", "mod", "You are not allowed to change proposals.expire_after."},
		{"!set proposals.expire_after soon", "owner", "proposals.expire_after must be a duration from 1h to 720h, such as 90s or 1h30m."},
		{"!set proposals.expire_after 30m", "owner", "proposals.expire_after must be a duration from 1h to 720h, such as 90s or 1h30m."},
		{"!set quiet maybe", "owner", "quiet must be true or false."},
		{"!set greeting " + strings.Repeat("x", 31), "owner", "greeting must be text of at most 30 bytes."},
		{"!set search.mode some", "owner", "search.mode must be one of any, all."},
		{"!unset quiet", "owner", "quiet has no override to drop: it is false, from the default."},
		{"!set quiet yes", "user-1", "You are not allowed to change quiet."},
	} {
		if got := lastText(a.Text(c.line, airuttest.As(airut.User{Platform: "test", ID: c.as, Name: c.as}))); got != c.reply {
			t.Errorf("%s as %s: %q, want %q", c.line, c.as, got, c.reply)
		}
	}
	if n := len(st.saved()); n != 1 {
		t.Errorf("%d changes were saved by refused commands", n-1)
	}

	for _, c := range []struct{ line, reply string }{
		{"!set proposals.expire_after 90m", "proposals.expire_after: 72h → 1h30m."},
		{"!set Quiet YES", "quiet: false → true."},
		{"!set greeting good day, secret-value", "greeting: hello → good day, secret-value."},
		{"!set search.mode All", "search.mode: any → all."},
	} {
		if got := lastText(a.Text(c.line, airuttest.As(owner))); got != c.reply {
			t.Errorf("%s: %q, want %q", c.line, got, c.reply)
		}
	}
	if d.expire.Get() != 90*time.Minute || !d.quiet.Get() || d.greet.Get() != "good day, secret-value" || d.mode.Get() != "all" {
		t.Errorf("values after set: %v %v %q %q", d.expire.Get(), d.quiet.Get(), d.greet.Get(), d.mode.Get())
	}
	if got := lastText(a.Text("!set hidden.knob 3", airuttest.As(owner))); got != "hidden.knob: 1 → 3." {
		t.Errorf("set hidden.knob: %q", got)
	}
	if evs := a.Text("!set hidden.knob 4", airuttest.As(owner)); len(evs) != 2 || evs[1].Kind != "reply" || !evs[1].Private {
		t.Errorf("a change to a setting not everyone reads was answered %+v, want privately", evs)
	}

	if got := lastText(a.Text("!unset proposals.max_open", airuttest.As(owner))); got != "proposals.max_open: 15 → 20, from the config." {
		t.Errorf("unset: %q", got)
	}
	if d.open.Get() != 20 {
		t.Errorf("proposals.max_open = %d after unset, want the config's 20", d.open.Get())
	}
	if !slices.ContainsFunc(a.Events(), func(e airuttest.Event) bool {
		return e.Kind == "send" && e.Text == "Olive dropped the override of setting proposals.max_open, so it went from 15 to 20."
	}) {
		t.Error("no notice of the unset")
	}
	if _, ok := st.overrides["proposals.max_open"]; ok {
		t.Error("the override is still stored after unset")
	}

	got := lastText(a.Text("!settings proposals.max_open", airuttest.As(tester)))
	for _, want := range []string{
		"proposals.max_open = 20, from the config.\nOpen proposals at once\nIt is a whole number from 1 to 50. The default is 10. The config sets 20.\nLatest changes:\n",
		" by Olive: 15 → 20, override dropped\n",
		" by Mod: 20 → 15",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("settings proposals.max_open:\n%s\nlacks %q", got, want)
		}
	}
	if l := logs.String(); strings.Contains(l, "secret-value") || !strings.Contains(l, "setting changed") {
		t.Errorf("a setting's value reached the log, or the change was not logged:\n%s", l)
	}
}

func TestSetWithoutAStore(t *testing.T) {
	ag := &airuttest.Agent{}
	a, d, _ := settingsBot(t, nil, ag, airut.Config{})
	if got := lastText(a.Text("!set quiet true", airuttest.As(owner))); got != "Settings cannot be changed here: this bot has no store to keep them in." {
		t.Errorf("set without a store: %q", got)
	}
	if d.quiet.Get() {
		t.Error("quiet changed without a store")
	}
	if got := lastText(a.Text("!settings quiet")); !strings.HasPrefix(got, "quiet = false, from the default.") {
		t.Errorf("settings quiet without a store: %q", got)
	}
}

func TestSetWithoutAnOwner(t *testing.T) {
	st := &settingStore{}
	b, _ := newBot(airut.Config{Agent: st, Gate: openGate})
	b.DeclareBool(airut.BoolSetting{Name: "quiet", Description: "Answer less"})
	a := airuttest.Start(t, b)
	if got := lastText(a.Text("!set quiet true")); got != "You are not allowed to change quiet." {
		t.Errorf("with no Owner: %q", got)
	}
}

func TestSettingStoreFailures(t *testing.T) {
	st := &settingStore{fail: errors.New("disk on fire")}
	b, _ := newBot(airut.Config{Agent: st, Gate: openGate})
	if err := b.Run(context.Background(), airuttest.New("")); err == nil || !strings.Contains(err.Error(), "reading the stored settings") {
		t.Errorf("Run with an unreadable store: %v", err)
	}

	// A bot with nothing declared never reads the store.
	b, _ = newBot(airut.Config{Settings: airut.Settings{Store: &settingStore{fail: errors.New("disk on fire")}}})
	airuttest.Start(t, b)

	st = &settingStore{}
	a, d, _ := settingsBot(t, st, st, airut.Config{})
	st.mu.Lock()
	st.fail = errors.New("disk on fire")
	st.mu.Unlock()
	if got := lastText(a.Text("!set quiet true", airuttest.As(owner))); got == "quiet: false → true." {
		t.Error("a change the store refused was answered as made")
	}
	if d.quiet.Get() {
		t.Error("a change the store refused took effect")
	}
}

func TestSettingsConfigErrors(t *testing.T) {
	for _, c := range []struct {
		values map[string]string
		want   string
	}{
		{map[string]string{"no.such": "1"}, `the config sets "no.such", which no setting declares`},
		{map[string]string{"quiet": "maybe"}, "the config's quiet must be true or false."},
		{map[string]string{"agent.per_user": "1"}, `"agent.per_user" is set from the agent's configuration, not from settings`},
	} {
		st := &settingStore{}
		b, _ := newBot(airut.Config{Agent: st, Gate: openGate, Settings: airut.Settings{Values: c.values}})
		b.DeclareBool(airut.BoolSetting{Name: "quiet", Description: "Answer less"})
		err := b.Run(context.Background(), airuttest.New(""))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: Run returned %v, want %q", c.values, err, c.want)
		}
		// Nothing started, so it may run again once the file is fixed.
		if err := b.Run(context.Background(), airuttest.New("")); err == nil || strings.Contains(err.Error(), "twice") {
			t.Errorf("a second Run: %v", err)
		}
	}
}

func TestSetByTheAgent(t *testing.T) {
	for _, c := range []struct {
		name, args, answer string
		want               int // the value after
		result             string
	}{
		{"confirmed", `{"name": "proposals.max_open", "value": "7"}`, "Change", 7, "proposals.max_open: 10 → 7."},
		{"declined", `{"name": "proposals.max_open", "value": "7"}`, "Don't", 10, "error: The caller did not agree to change proposals.max_open."},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &settingStore{Agent: airuttest.Agent{Steps: []airuttest.Step{{Call: "set", Args: c.args}}}}
			a, d, _ := settingsBot(t, st, st, airut.Config{Settings: airut.Settings{Notices: []airut.Channel{notices}}})
			var asked []airut.Ask
			a.OnAsk(func(q airut.Ask) (string, error) { asked = append(asked, q); return c.answer, nil })
			a.Text("!ask", airuttest.As(owner))
			if len(asked) != 1 || asked[0].Question != "Change proposals.max_open from 10 to 7?" || asked[0].Who != owner {
				t.Fatalf("asked %+v", asked)
			}
			if d.open.Get() != c.want {
				t.Errorf("proposals.max_open = %d, want %d", d.open.Get(), c.want)
			}
			if res := st.Results(); len(res) != 1 || res[0] != c.result {
				t.Errorf("tool results %q, want %q", res, c.result)
			}
			if c.want != 10 {
				if ch := st.saved(); len(ch) != 1 || ch[0].Source != airut.ByAgent || ch[0].By != owner {
					t.Errorf("saved %+v", ch)
				}
				if !slices.ContainsFunc(a.Events(), func(e airuttest.Event) bool {
					return e.Kind == "send" && e.Text == "Olive changed setting proposals.max_open from 10 to 7, through the assistant."
				}) {
					t.Errorf("no notice that the assistant made the change: %+v", a.Events())
				}
			}
		})
	}
}

func TestSetByTheAgentForSomeoneNotAllowed(t *testing.T) {
	st := &settingStore{Agent: airuttest.Agent{Steps: []airuttest.Step{{Call: "set", Args: `{"name": "quiet", "value": "true"}`}}}}
	a, d, _ := settingsBot(t, st, st, airut.Config{})
	asked := 0
	a.OnAsk(func(airut.Ask) (string, error) { asked++; return "Change", nil })
	a.Text("!ask", airuttest.As(tester))
	if asked != 0 || d.quiet.Get() {
		t.Errorf("a turn of someone not allowed asked %d times and set quiet to %v", asked, d.quiet.Get())
	}
	if res := st.Results(); len(res) != 1 || res[0] != "error: You are not allowed to change quiet." {
		t.Errorf("tool results %q", res)
	}
}

func TestSetWhileTheValueMoved(t *testing.T) {
	st := &settingStore{Agent: airuttest.Agent{Steps: []airuttest.Step{{Call: "set", Args: `{"name": "quiet", "value": "true"}`}}}}
	a, d, _ := settingsBot(t, st, st, airut.Config{})
	a.OnAsk(func(airut.Ask) (string, error) {
		// Someone else changes it while the caller decides.
		done := make(chan struct{})
		go func() { defer close(done); a.Text("!set quiet true", airuttest.As(owner)) }()
		<-done
		return "Change", nil
	})
	a.Text("!ask", airuttest.As(owner))
	if res := st.Results(); len(res) != 1 || res[0] != "error: quiet changed while this was being decided, so nothing was changed. Ask again." {
		t.Errorf("tool results %q", res)
	}
	if n := len(st.saved()); n != 1 || !d.quiet.Get() {
		t.Errorf("%d changes saved, quiet = %v; want only the other one", n, d.quiet.Get())
	}
}

func TestGateSettings(t *testing.T) {
	st := &settingStore{Agent: airuttest.Agent{Steps: []airuttest.Step{{Text: "hi"}}}}
	b, _ := newBot(airut.Config{Agent: st, Gate: airut.Gate{
		Access:  map[airut.Platform]airut.Access{"test": {Users: []string{"owner"}}},
		PerUser: 3, Window: time.Minute, Budget: airut.Budget{Turns: 8},
	}, Settings: airut.Settings{Owner: isOwner}})
	var budgets []airut.Budget
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "hi"}).StreamTo(c)
	})
	a := airuttest.Start(t, b)
	for _, c := range []struct{ line, reply string }{
		{"!set agent.per_user 4", "agent.per_user must be from 1 to 3: a setting may tighten the configured limit, not loosen it."},
		{"!set agent.per_user 0", "agent.per_user must be from 1 to 3: a setting may tighten the configured limit, not loosen it."},
		{"!set agent.window 30s", "agent.window must be at least 1m: a setting may tighten the configured limit, not loosen it."},
		{"!set agent.max_turns 9", "agent.max_turns must be from 1 to 8: a setting may tighten the configured limit, not loosen it."},
		{"!set agent.per_channel 2", "agent.per_channel: 0 → 2."},
		{"!set agent.per_user 1", "agent.per_user: 3 → 1."},
		{"!set agent.window 2m", "agent.window: 1m → 2m."},
		{"!set agent.max_turns 2", "agent.max_turns: 8 → 2."},
		{"!set agent.max_tokens 500", "agent.max_tokens: 0 → 500."},
	} {
		if got := lastText(a.Text(c.line, airuttest.As(owner))); got != c.reply {
			t.Errorf("%s: %q, want %q", c.line, got, c.reply)
		}
	}
	a.Text("!ask", airuttest.As(owner))
	if got := lastText(a.Text("!ask", airuttest.As(owner))); !strings.HasPrefix(got, "You have started too many agent turns.") {
		t.Errorf("a second turn under agent.per_user 1: %q", got)
	}
	for _, turn := range st.Turns() {
		budgets = append(budgets, turn.Budget)
	}
	if len(budgets) != 1 || budgets[0] != (airut.Budget{Turns: 2, Tokens: 500}) {
		t.Errorf("turn budgets %+v, want the settings' 2 turns and 500 tokens", budgets)
	}
}

func TestGateSettingsNeedAWindow(t *testing.T) {
	st := &settingStore{}
	b, _ := newBot(airut.Config{Agent: st, Gate: openGate, Settings: airut.Settings{Owner: isOwner}})
	a := airuttest.Start(t, b)
	for _, c := range []struct{ line, reply string }{
		{"!set agent.per_user 2", "Set agent.window first: agent.per_user needs a window to count turns in."},
		{"!set agent.window 1m", "agent.window: 0s → 1m."},
		{"!set agent.per_user 2", "agent.per_user: 0 → 2."},
		{"!set agent.window 0s", "agent.window must be positive while agent.per_user or agent.per_channel is set."},
		{"!set agent.per_user 0", "agent.per_user: 2 → 0."},
		{"!set agent.per_channel 1", "agent.per_channel: 0 → 1."},
		{"!set agent.window 0s", "agent.window must be positive while agent.per_user or agent.per_channel is set."},
		// Unsetting restores the configured 0s, which is no window either.
		{"!unset agent.window", "agent.window must be positive while agent.per_user or agent.per_channel is set."},
		{"!unset agent.per_channel", "agent.per_channel: 1 → 0, from the config."},
		{"!unset agent.window", "agent.window: 1m → 0s, from the config."},
	} {
		if got := lastText(a.Text(c.line, airuttest.As(owner))); got != c.reply {
			t.Errorf("%s: %q, want %q", c.line, got, c.reply)
		}
	}
}

func TestNoGateSettingsWithoutAnAgent(t *testing.T) {
	b, _ := newBot(airut.Config{})
	a := airuttest.Start(t, b)
	for _, s := range a.Specs() {
		if s.Name == "settings" {
			t.Error("a bot with no agent and no settings has the settings command")
		}
	}
}

func TestSettingOnChange(t *testing.T) {
	st := &settingStore{}
	b, logs := newBot(airut.Config{Agent: st, Gate: openGate, Settings: airut.Settings{Owner: isOwner}})
	var seen [][2]int
	b.DeclareInt(airut.IntSetting{Name: "size", Description: "Size", Default: 1, Min: 0, Max: 9,
		OnChange: func(old, new int) { seen = append(seen, [2]int{old, new}) }})
	b.DeclareBool(airut.BoolSetting{Name: "broken", Description: "Broken",
		OnChange: func(bool, bool) { panic("rebuild failed") }})
	a := airuttest.Start(t, b)
	a.Text("!set size 4", airuttest.As(owner))
	a.Text("!unset size", airuttest.As(owner))
	if want := [][2]int{{1, 4}, {4, 1}}; !slices.Equal(seen, want) {
		t.Errorf("OnChange saw %v, want %v", seen, want)
	}
	if got := lastText(a.Text("!set broken on", airuttest.As(owner))); got != "broken: false → true." {
		t.Errorf("a change whose OnChange panicked: %q", got)
	}
	if !strings.Contains(logs.String(), "setting OnChange panicked") {
		t.Error("the panic was not logged")
	}
}

func TestDeclarePanics(t *testing.T) {
	for _, c := range []struct {
		name    string
		declare func(*airut.Bot)
		want    string
	}{
		{"a bad name", func(b *airut.Bot) { b.DeclareBool(airut.BoolSetting{Name: "Quiet", Description: "d"}) }, "a name is lowercase"},
		{"a dotted end", func(b *airut.Bot) { b.DeclareBool(airut.BoolSetting{Name: "quiet.", Description: "d"}) }, "a name is lowercase"},
		{"no description", func(b *airut.Bot) { b.DeclareBool(airut.BoolSetting{Name: "quiet"}) }, "description"},
		{"a default outside the bounds", func(b *airut.Bot) {
			b.DeclareInt(airut.IntSetting{Name: "n", Description: "d", Default: 0, Min: 1, Max: 2})
		}, "the default is not one of its values"},
		{"min above max", func(b *airut.Bot) {
			b.DeclareDuration(airut.DurationSetting{Name: "d", Description: "d", Min: 2, Max: 1})
		}, "Min is above Max"},
		{"no choices", func(b *airut.Bot) { b.DeclareEnum(airut.EnumSetting{Name: "e", Description: "d"}) }, "choices"},
		{"a default not a choice", func(b *airut.Bot) {
			b.DeclareEnum(airut.EnumSetting{Name: "e", Description: "d", Default: "c", Choices: []string{"a"}})
		}, "the default is not one of its values"},
		{"no MaxLen", func(b *airut.Bot) { b.DeclareString(airut.StringSetting{Name: "s", Description: "d"}) }, "MaxLen"},
		{"twice", func(b *airut.Bot) {
			b.DeclareBool(airut.BoolSetting{Name: "q", Description: "d"})
			b.DeclareBool(airut.BoolSetting{Name: "q", Description: "d"})
		}, "declared twice"},
		{"a Gate setting's name", func(b *airut.Bot) { b.DeclareBool(airut.BoolSetting{Name: "agent.window", Description: "d"}) }, "declared twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r, _ := recover().(string); !strings.Contains(r, c.want) {
					t.Errorf("panic %q, want one naming %q", r, c.want)
				}
			}()
			b, _ := newBot(airut.Config{Agent: &airuttest.Agent{}})
			c.declare(b)
		})
	}
}

func TestDeclareAfterRun(t *testing.T) {
	b, _ := newBot(airut.Config{})
	airuttest.Start(t, b)
	defer func() {
		if r, _ := recover().(string); !strings.Contains(r, "declared after Run") {
			t.Errorf("panic %q", r)
		}
	}()
	b.DeclareBool(airut.BoolSetting{Name: "late", Description: "d"})
}

func TestStoredGateOverridesStayUnderTheConfig(t *testing.T) {
	st := &settingStore{overrides: map[string]string{"agent.per_user": "9", "agent.max_turns": "2"}}
	b, logs := newBot(airut.Config{Agent: st, Gate: airut.Gate{
		Access: map[airut.Platform]airut.Access{"test": {Users: []string{"owner"}}}, PerUser: 3, Window: time.Minute,
		Budget: airut.Budget{Turns: 8},
	}})
	a := airuttest.Start(t, b)
	got := lastText(a.Text("!settings"))
	if !strings.Contains(got, "agent.per_user = 3 (config)") || !strings.Contains(got, "agent.max_turns = 2 (override)") {
		t.Errorf("a stored override above the configured cap was applied, or one under it was not:\n%s", got)
	}
	if !strings.Contains(logs.String(), "setting=agent.per_user") {
		t.Error("the refused override was not logged")
	}
}

// A change checked against another setting is checked again once the
// caller confirms, since that setting may have changed meanwhile.
func TestSetChecksAgainAfterTheCallerDecides(t *testing.T) {
	st := &settingStore{Agent: airuttest.Agent{Steps: []airuttest.Step{{Call: "set", Args: `{"name": "agent.per_user", "value": "2"}`}}}}
	b, _ := newBot(airut.Config{Agent: st, Gate: airut.Gate{Access: map[airut.Platform]airut.Access{"test": {Users: []string{"owner"}}}},
		Settings: airut.Settings{Owner: isOwner}})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "hi", Tools: b.Tools("set")}).StreamTo(c)
	})
	a := airuttest.Start(t, b)
	a.Text("!set agent.window 1m", airuttest.As(owner))
	a.OnAsk(func(airut.Ask) (string, error) {
		done := make(chan struct{})
		go func() { defer close(done); a.Text("!set agent.window 0s", airuttest.As(owner)) }()
		<-done
		return "Change", nil
	})
	a.Text("!ask", airuttest.As(owner))
	if res := st.Results(); len(res) != 1 || res[0] != "error: Set agent.window first: agent.per_user needs a window to count turns in." {
		t.Errorf("tool results %q", res)
	}
	if got := lastText(a.Text("!settings agent.per_user")); !strings.HasPrefix(got, "agent.per_user = 0,") {
		t.Errorf("agent.per_user was set without a window:\n%s", got)
	}
}

// Stored overrides that depend on one another are checked together, so
// a cap stored with its window survives a restart whichever is read
// first. One whose window is not stored falls back to the config.
func TestStoredGateOverridesAreCheckedTogether(t *testing.T) {
	for _, c := range []struct {
		stored  map[string]string
		perUser string
	}{
		{map[string]string{"agent.per_user": "2", "agent.window": "1m"}, "agent.per_user = 2 (override)"},
		{map[string]string{"agent.per_user": "2", "agent.window": "0s"}, "agent.per_user = 0 (config)"},
	} {
		st := &settingStore{overrides: c.stored}
		b, _ := newBot(airut.Config{Agent: st, Gate: openGate})
		a := airuttest.Start(t, b)
		if got := lastText(a.Text("!settings")); !strings.Contains(got, c.perUser) {
			t.Errorf("stored %v: listing\n%s\nlacks %q", c.stored, got, c.perUser)
		}
	}
}

// A set and an unset while the caller decides bring the value back to
// where it was, and the change is still refused, since the question was
// about a value that has since moved twice.
func TestSetWhileTheValueMovedAndCameBack(t *testing.T) {
	st := &settingStore{Agent: airuttest.Agent{Steps: []airuttest.Step{{Call: "set", Args: `{"name": "proposals.max_open", "value": "7"}`}}}}
	a, d, _ := settingsBot(t, st, st, airut.Config{})
	a.OnAsk(func(airut.Ask) (string, error) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.Text("!set proposals.max_open 30", airuttest.As(owner))
			a.Text("!unset proposals.max_open", airuttest.As(owner))
		}()
		<-done
		return "Change", nil
	})
	a.Text("!ask", airuttest.As(owner))
	if res := st.Results(); len(res) != 1 || res[0] != "error: proposals.max_open changed while this was being decided, so nothing was changed. Ask again." {
		t.Errorf("tool results %q", res)
	}
	if d.open.Get() != 10 || len(st.saved()) != 2 {
		t.Errorf("proposals.max_open = %d after %d changes; want 10 after the other two", d.open.Get(), len(st.saved()))
	}
}
