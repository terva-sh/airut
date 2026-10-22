package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/agent/sessions"
	"terva.sh/airut/airuttest"
	"terva.sh/airut/config"
)

// reply is one scripted model response.
type reply struct {
	content []provider.Content
	stop    provider.StopReason
	tokens  int
	// delay holds the reply back after its first event, and gap between
	// its text deltas.
	delay, gap time.Duration
	// toolStarts announces each tool call as it begins, as a streaming
	// provider does.
	toolStarts bool
	// err fails the request before it streams anything.
	err error
}

func say(text string) reply {
	return reply{content: []provider.Content{provider.TextBlock{Text: text}}, stop: provider.StopEnd, tokens: 100}
}

func call(id, name, args string) reply {
	return reply{
		content: []provider.Content{provider.ToolCallBlock{ID: id, Name: name, Arguments: json.RawMessage(args)}},
		stop:    provider.StopToolUse,
		tokens:  100,
	}
}

// fake is a scripted provider.Client, as in terva's examples/harness. It
// plays its replies in order and records every request.
type fake struct {
	mu     sync.Mutex
	script []reply
	reqs   []provider.Request
}

func (f *fake) Name() string { return "fake" }

func (f *fake) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, snapshot(req))
	r := say("the script has ended")
	if len(f.script) > 0 {
		r, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	ch := make(chan provider.Event, len(r.content)+3)
	go func() {
		defer close(ch)
		ch <- provider.EventStart{Model: req.Model, Provider: "fake"}
		time.Sleep(r.delay)
		for i, c := range r.content {
			switch c := c.(type) {
			case provider.TextBlock:
				if i > 0 {
					time.Sleep(r.gap)
				}
				ch <- provider.EventTextDelta{Delta: c.Text}
			case provider.ToolCallBlock:
				if r.toolStarts {
					ch <- provider.EventToolStart{ID: c.ID, Name: c.Name}
				}
			}
		}
		ch <- provider.EventUsage{Usage: provider.Usage{InputTokens: r.tokens / 2, OutputTokens: r.tokens - r.tokens/2}}
		select {
		case ch <- provider.EventDone{Stop: r.stop, Message: provider.Message{Role: provider.RoleAssistant, Content: r.content}}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// snapshot copies req's messages' content, as the engine may change the
// live transcript in place after the request, as when it takes out an
// image the provider refused.
func snapshot(req provider.Request) provider.Request {
	ms := slices.Clone(req.Messages)
	for i := range ms {
		ms[i].Content = slices.Clone(ms[i].Content)
		for j, c := range ms[i].Content {
			if r, ok := c.(provider.ToolResultBlock); ok {
				r.Content = slices.Clone(r.Content)
				ms[i].Content[j] = r
			}
		}
	}
	req.Messages = ms
	return req
}

func (f *fake) requests() []provider.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Request(nil), f.reqs...)
}

func (f *fake) play(rs ...reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, rs...)
}

var allow = airut.Gate{Access: map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1"}}}}

type rollArgs struct {
	Dice string `desc:"Dice to roll, like 2d6"`
}

// env is a bot on the fake adapter whose agent talks to a fake model.
type env struct {
	model *fake
	agent *Agent
	bot   *airut.Bot
	ran   []string // the dice each roll was called with
	mu    sync.Mutex
}

func newEnv(t *testing.T, db *sessions.DB, personas map[string]string, cfg airut.Config) *env {
	t.Helper()
	if db == nil {
		db = openDB(t, filepath.Join(t.TempDir(), "sessions.db"))
	}
	e := &env{model: &fake{}}
	reg := provider.NewRegistry()
	reg.RegisterExtraModel(provider.Model{Provider: "fake", ID: "m", ContextWindow: 32000, MaxOutput: 1024})
	ag, err := New(Options{
		Clients:  map[string]provider.Client{"fake": e.model},
		Catalog:  reg,
		Model:    config.ModelRef{Provider: "fake", Model: "m"},
		Personas: personas,
		Sessions: db,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.agent = ag
	cfg.Agent, cfg.EditInterval = ag, time.Nanosecond
	if cfg.Gate.Access == nil {
		cfg.Gate = allow
	}
	e.bot = airut.New(cfg)
	e.bot.Command("roll", "Roll dice", func(c *airut.Ctx, a rollArgs) error {
		e.mu.Lock()
		e.ran = append(e.ran, a.Dice)
		e.mu.Unlock()
		if a.Dice == "fail" {
			return errors.New("internal detail: connection string postgres://secret")
		}
		if a.Dice == "bad" {
			return airut.Errorf("No such dice.")
		}
		if a.Dice == "wait" {
			time.Sleep(30 * time.Millisecond)
		}
		if a.Dice == "slow" {
			// A timeout of the tool's own, while the turn goes on.
			return fmt.Errorf("dice service: %w", context.DeadlineExceeded)
		}
		_, err := c.Reply("rolled " + a.Dice)
		return err
	}).AsTool()
	return e
}

func (e *env) rolls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ran...)
}

// ask registers an "ask" command that starts turn and streams it.
func (e *env) ask(turn airut.Turn) {
	e.bot.Command("ask", "Ask", func(c *airut.Ctx) error {
		t := turn
		t.Tools = make([]airut.Tool, len(turn.Tools))
		copy(t.Tools, turn.Tools)
		return c.Agent(t).StreamTo(c)
	})
}

func openDB(t *testing.T, path string) *sessions.DB {
	t.Helper()
	db, err := sessions.Open(context.Background(), path, sessions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// final is the text of the bot's answer after its last edit.
func final(evs []airuttest.Event) string {
	var last string
	for _, e := range evs {
		if e.Kind == "reply" || e.Kind == "edit" {
			last = e.Text
		}
	}
	return last
}

// toolResults returns the text of every tool result the model was sent
// in its last request.
func toolResults(req provider.Request) map[string]string {
	out := map[string]string{}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if r, ok := c.(provider.ToolResultBlock); ok {
				var b strings.Builder
				for _, rc := range r.Content {
					if tb, ok := rc.(provider.TextBlock); ok {
						b.WriteString(tb.Text)
					}
				}
				out[r.CallID] = b.String()
			}
		}
	}
	return out
}

func TestTurnCallsOnlyTheToolsItNames(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.bot.Command("secret", "Not for this turn", func(c *airut.Ctx) error {
		t.Error("the agent ran a tool its turn did not name")
		return nil
	}).AsTool()
	e.ask(airut.Turn{Prompt: "roll 2d6", Tools: e.bot.Tools("roll")})
	e.model.play(
		call("c1", "roll", `{"dice":"2d6"}`),
		call("c2", "secret", `{}`),
		say("You rolled."),
	)
	evs := airuttest.Start(t, e.bot).Text("!ask")

	if got := final(evs); got != "You rolled." {
		t.Errorf("answer = %q", got)
	}
	reqs := e.model.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d model requests, want 3", len(reqs))
	}
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "roll" {
		t.Errorf("the model was offered %+v, want only roll", reqs[0].Tools)
	}
	res := toolResults(reqs[2])
	if res["c1"] != "rolled 2d6" {
		t.Errorf("roll's result = %q; its reply should go to the model, not the chat", res["c1"])
	}
	if res["c2"] == "" || strings.Contains(res["c2"], "rolled") {
		t.Errorf("an unnamed tool's result = %q, want a refusal", res["c2"])
	}
	for _, ev := range evs {
		if strings.Contains(ev.Text, "rolled 2d6") {
			t.Error("a tool's reply reached the chat")
		}
	}
}

func TestToolErrorsReachTheModelOnlyAsPeopleWouldSeeThem(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "roll", Tools: e.bot.Tools("roll")})
	e.model.play(
		call("c1", "roll", `{"dice":"fail"}`),
		call("c2", "roll", `{"dice":"bad"}`),
		call("c3", "roll", `{}`),
		say("ok"),
	)
	airuttest.Start(t, e.bot).Text("!ask")
	reqs := e.model.requests()
	res := toolResults(reqs[len(reqs)-1])
	if strings.Contains(res["c1"], "postgres") || !strings.Contains(res["c1"], "the command failed") {
		t.Errorf("an internal error reached the model as %q", res["c1"])
	}
	if !strings.Contains(res["c2"], "No such dice.") {
		t.Errorf("a user error reached the model as %q, want its text", res["c2"])
	}
	if !strings.Contains(res["c3"], "dice") {
		t.Errorf("an argument error reached the model as %q, want the parameter named", res["c3"])
	}
}

func TestAToolsOwnTimeoutDoesNotEndTheTurn(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "roll", Tools: e.bot.Tools("roll")})
	e.model.play(call("c1", "roll", `{"dice":"slow"}`), say("The dice service timed out."))
	evs := airuttest.Start(t, e.bot).Text("!ask")
	reqs := e.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model requests, want the turn to go on after the tool's timeout", len(reqs))
	}
	if got := toolResults(reqs[1])["c1"]; got != "error: the command failed" {
		t.Errorf("the model read %q, want the filtered failure", got)
	}
	if got := final(evs); got != "The dice service timed out." {
		t.Errorf("answer = %q", got)
	}
}

func TestApprovalRequiredTools(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(airut.Ask) (string, error)
		ran    bool
		want   string
	}{
		{"yes runs it", func(airut.Ask) (string, error) { return "yes", nil }, true, "rolled 1d20"},
		{"no refuses it", func(airut.Ask) (string, error) { return "no", nil }, false, "declined"},
		{"a timeout refuses it", func(airut.Ask) (string, error) { return "", context.DeadlineExceeded }, false, "in time"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil, nil, airut.Config{})
			e.bot.Command("risky", "Roll, with approval", func(cx *airut.Ctx, a rollArgs) error {
				e.mu.Lock()
				e.ran = append(e.ran, a.Dice)
				e.mu.Unlock()
				_, err := cx.Reply("rolled " + a.Dice)
				return err
			}).AsTool().RequireApproval()
			e.ask(airut.Turn{Prompt: "roll", Tools: e.bot.Tools("risky", "roll")})
			e.model.play(call("c1", "risky", `{"dice":"1d20"}`), call("c2", "roll", `{"dice":"1d4"}`), say("done"))
			a := airuttest.Start(t, e.bot)
			var asked []airut.Ask
			a.OnAsk(func(q airut.Ask) (string, error) {
				asked = append(asked, q)
				return c.answer(q)
			})
			a.Text("!ask")

			// Only the tool that needs approval asked, and it asked the caller.
			if len(asked) != 1 || !strings.Contains(asked[0].Question, "risky") || !strings.Contains(asked[0].Question, `"1d20"`) || asked[0].Who.ID != "user-1" {
				t.Fatalf("asks = %+v", asked)
			}
			want := []string{"1d4"}
			if c.ran {
				want = []string{"1d20", "1d4"}
			}
			if got := e.rolls(); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("ran %v, want %v", got, want)
			}
			reqs := e.model.requests()
			if got := toolResults(reqs[len(reqs)-1])["c1"]; !strings.Contains(got, c.want) {
				t.Errorf("the model read %q, want it to mention %q", got, c.want)
			}
		})
	}
}

func TestBudgetStopsTheTurn(t *testing.T) {
	for _, c := range []struct {
		name     string
		budget   airut.Budget
		requests int
		note     string
	}{
		{"model requests", airut.Budget{Turns: 2}, 2, "(Stopped: this turn used all 2 of its model requests.)"},
		// Each response reports 100 tokens: the third request would start at 200.
		{"tokens", airut.Budget{Tokens: 200}, 2, "(Stopped: this turn used its budget of 200 tokens.)"},
		{"tokens, from the gate's default", airut.Budget{}, 3, "(Stopped: this turn used its budget of 300 tokens.)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := allow
			g.Budget = airut.Budget{Tokens: 300}
			e := newEnv(t, nil, nil, airut.Config{Gate: g})
			e.ask(airut.Turn{Prompt: "keep rolling", Tools: e.bot.Tools("roll"), Budget: c.budget})
			e.model.play(
				reply{content: []provider.Content{provider.TextBlock{Text: "Rolling. "}, provider.ToolCallBlock{ID: "c1", Name: "roll", Arguments: json.RawMessage(`{"dice":"1d6"}`)}}, stop: provider.StopToolUse, tokens: 100},
				call("c2", "roll", `{"dice":"2d6"}`),
				call("c3", "roll", `{"dice":"3d6"}`),
				call("c4", "roll", `{"dice":"4d6"}`),
			)
			evs := airuttest.Start(t, e.bot).Text("!ask")
			if n := len(e.model.requests()); n != c.requests {
				t.Errorf("%d model requests, want %d", n, c.requests)
			}
			if got, want := final(evs), "Rolling. \n\n"+c.note; got != want {
				t.Errorf("answer = %q, want %q", got, want)
			}
			m := e.bot.Metrics()
			if s, _ := m.Get("airut_agent_requests_total", "command", "ask"); s.Value != float64(c.requests) {
				t.Errorf("requests metric = %v, want %d", s.Value, c.requests)
			}
			if s, _ := m.Get("airut_agent_tokens_total", "command", "ask"); s.Value != float64(100*c.requests) {
				t.Errorf("tokens metric = %v, want %d", s.Value, 100*c.requests)
			}
			if s, _ := m.Get("airut_agent_stops_total", "command", "ask", "stop", "budget"); s.Value != 1 {
				t.Errorf("budget stops = %v, want 1", s.Value)
			}
		})
	}
}

func TestSessionsResumeAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db := openDB(t, path)
	e := newEnv(t, db, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "remember 7", Session: airut.PerChannel})
	e.model.play(say("I will remember 7."))
	airuttest.Start(t, e.bot).Text("!ask")
	db.Close()

	// A new bot on the same database, as after a restart.
	e2 := newEnv(t, openDB(t, path), nil, airut.Config{})
	e2.ask(airut.Turn{Prompt: "what was it?", Session: airut.PerChannel})
	e2.model.play(say("7."))
	a := airuttest.Start(t, e2.bot)
	a.Text("!ask")
	msgs := e2.model.requests()[0].Messages
	if len(msgs) != 3 || text(msgs[0]) != "remember 7" || text(msgs[1]) != "I will remember 7." || text(msgs[2]) != "what was it?" {
		t.Fatalf("the resumed turn sent %d messages: %+v", len(msgs), msgs)
	}

	// Another channel has its own session, and a turn with none starts fresh.
	a.Text("!ask", airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "elsewhere"}))
	if msgs := e2.model.requests()[1].Messages; len(msgs) != 1 {
		t.Errorf("another channel's turn saw %d messages, want only its prompt", len(msgs))
	}
}

func TestNoSessionStartsFresh(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "hello"})
	a := airuttest.Start(t, e.bot)
	a.Text("!ask")
	a.Text("!ask")
	if msgs := e.model.requests()[1].Messages; len(msgs) != 1 {
		t.Errorf("a turn with no session saw %d messages, want only its prompt", len(msgs))
	}
}

func TestForgetClearsTheCallersSession(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "remember me", Session: airut.PerUser})
	a := airuttest.Start(t, e.bot)
	a.Text("!ask")
	a.Text("!ask")
	if msgs := e.model.requests()[1].Messages; len(msgs) != 3 {
		t.Fatalf("the second turn saw %d messages, want the first turn's two and its own", len(msgs))
	}
	evs := a.Text("!forget")
	if len(evs) != 1 || !evs[0].Private || !strings.Contains(evs[0].Text, "forgotten") {
		t.Fatalf("forget answered %+v", evs)
	}
	a.Text("!ask")
	if msgs := e.model.requests()[2].Messages; len(msgs) != 1 {
		t.Errorf("after forget the turn saw %d messages, want only its prompt", len(msgs))
	}
}

func TestPersonas(t *testing.T) {
	e := newEnv(t, nil, map[string]string{"": "You are terse.", "dm": "You narrate."}, airut.Config{})
	persona := ""
	e.bot.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "hi", Persona: persona}).StreamTo(c)
	})
	a := airuttest.Start(t, e.bot)
	a.Text("!ask")
	persona = "dm"
	a.Text("!ask")
	persona = "nobody"
	evs := a.Text("!ask")
	reqs := e.model.requests()
	if len(reqs) != 2 || reqs[0].System != "You are terse." || reqs[1].System != "You narrate." {
		t.Fatalf("system prompts = %q", systems(reqs))
	}
	if final(evs) == "" || strings.Contains(final(evs), "nobody") {
		t.Errorf("an unknown persona answered %q, want the generic failure", final(evs))
	}
}

func TestCharacterOpensEveryPersona(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "sessions.db"))
	model := &fake{}
	reg := provider.NewRegistry()
	reg.RegisterExtraModel(provider.Model{Provider: "fake", ID: "m", ContextWindow: 32000, MaxOutput: 1024})
	ag, err := New(Options{
		Clients: map[string]provider.Client{"fake": model}, Catalog: reg,
		Model:     config.ModelRef{Provider: "fake", Model: "m"},
		Personas:  map[string]string{"search": "Find posts."},
		Character: "You are the archive's librarian.",
		Sessions:  db,
	})
	if err != nil {
		t.Fatal(err)
	}
	bot := airut.New(airut.Config{Agent: ag, Gate: allow, EditInterval: time.Nanosecond})
	persona := "search"
	bot.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "hi", Persona: persona}).StreamTo(c)
	})
	a := airuttest.Start(t, bot)
	a.Text("!ask")
	persona = ""
	a.Text("!ask")
	want := []string{"You are the archive's librarian.\n\nFind posts.", "You are the archive's librarian."}
	if got := systems(model.requests()); !slices.Equal(got, want) {
		t.Fatalf("system prompts = %q, want %q", got, want)
	}
}

func TestNew(t *testing.T) {
	reg := provider.NewRegistry()
	reg.RegisterExtraModel(provider.Model{Provider: "fake", ID: "m"})
	db := openDB(t, filepath.Join(t.TempDir(), "s.db"))
	ok := Options{Clients: map[string]provider.Client{"fake": &fake{}}, Catalog: reg, Model: config.ModelRef{Provider: "fake", Model: "m"}, Sessions: db}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Options){
		"no client":   func(o *Options) { o.Model.Provider = "other" },
		"no model":    func(o *Options) { o.Model.Model = "absent" },
		"no catalog":  func(o *Options) { o.Catalog = nil },
		"no sessions": func(o *Options) { o.Sessions = nil },
	} {
		o := ok
		mod(&o)
		if _, err := New(o); err == nil {
			t.Errorf("New accepted options with %s", name)
		}
	}
}

func TestOneTurnAtATimePerSession(t *testing.T) {
	var l keyLocks
	unlock, err := l.lock(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.lock(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second lock on a held key: %v, want it to wait until its context ends", err)
	}
	other, err := l.lock(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	other()
	unlock()
	again, err := l.lock(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	again()
	if len(l.m) != 0 {
		t.Errorf("%d keys are still held after every lock was released", len(l.m))
	}
}

func text(m provider.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if t, ok := c.(provider.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func systems(reqs []provider.Request) []string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.System)
	}
	return out
}

func TestRestrictedToolsThroughTheEngine(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	// The check allows when the turn starts and refuses after, as for a
	// moderator who left the room mid-turn.
	var checks int
	var mu sync.Mutex
	e.bot.Command("tag", "Apply tags", func(c *airut.Ctx) error {
		_, err := c.Reply("tagged")
		return err
	}).AsTool().RestrictTo(func(*airut.Ctx) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		checks++
		return checks == 1, nil
	})
	e.ask(airut.Turn{Prompt: "tag it", Tools: e.bot.Tools("roll", "tag")})
	e.model.play(call("c1", "tag", `{}`), say("ok"))
	airuttest.Start(t, e.bot).Text("!ask")
	reqs := e.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model requests, want 2", len(reqs))
	}
	var offered []string
	for _, tl := range reqs[0].Tools {
		offered = append(offered, tl.Name)
	}
	if !slices.Contains(offered, "tag") {
		t.Errorf("the model was offered %q; the check allowed tag when the turn started", offered)
	}
	if got := toolResults(reqs[1])["c1"]; got != "error: the caller is not allowed to use this tool" {
		t.Errorf("the model read %q for a call the second check refused", got)
	}

	// A turn whose caller the check refuses from the start is never
	// offered the tool.
	e2 := newEnv(t, nil, nil, airut.Config{})
	e2.bot.Command("tag", "Apply tags", func(c *airut.Ctx) error { return nil }).AsTool().
		RestrictTo(func(*airut.Ctx) (bool, error) { return false, nil })
	e2.ask(airut.Turn{Prompt: "tag it", Tools: e2.bot.Tools("roll", "tag")})
	e2.model.play(say("ok"))
	airuttest.Start(t, e2.bot).Text("!ask")
	for _, tl := range e2.model.requests()[0].Tools {
		if tl.Name == "tag" {
			t.Error("a tool the caller may not use was offered to the model")
		}
	}
}

// TestAskToolIsNoApproval has the model ask, through the ask tool,
// whether it may run a tool that needs approval, and get a yes. The
// gate still puts its own question, and a no to that one stops the tool.
func TestAskToolIsNoApproval(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.bot.Command("risky", "Roll, with approval", func(cx *airut.Ctx, a rollArgs) error {
		e.mu.Lock()
		e.ran = append(e.ran, a.Dice)
		e.mu.Unlock()
		return nil
	}).AsTool().RequireApproval()
	tools := append(e.bot.Tools("risky"), airut.AskTool(airut.AskToolOptions{}))
	e.ask(airut.Turn{Prompt: "roll", Tools: tools})
	e.model.play(
		call("c1", "ask", `{"questions":[{"key":"ok","text":"May I roll the risky dice?","choices":["yes","no"]}]}`),
		call("c2", "risky", `{"dice":"1d20"}`),
		say("done"),
	)
	a := airuttest.Start(t, e.bot)
	a.OnAskAll(func(b airut.Bundle) []airuttest.Answer {
		return []airuttest.Answer{{By: airut.User{Platform: airuttest.Platform, ID: "user-1"}, Key: "ok", Text: "yes"}}
	})
	var asked []string
	a.OnAsk(func(q airut.Ask) (string, error) {
		asked = append(asked, q.Question)
		return "no", nil
	})
	a.Text("!ask")
	if len(asked) != 1 || !strings.Contains(asked[0], "risky") {
		t.Fatalf("the gate asked %q; a yes to the model's own question must not stand in for it", asked)
	}
	if got := e.rolls(); len(got) != 0 {
		t.Fatalf("the tool ran %v after the gate's question was answered no", got)
	}
	reqs := e.model.requests()
	if got := toolResults(reqs[len(reqs)-1]); !strings.Contains(got["c1"], `ok: yes, chosen by "user-1"`) || !strings.Contains(got["c2"], "declined") {
		t.Fatalf("the model read %q", got)
	}
}
