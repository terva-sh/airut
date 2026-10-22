package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
	"terva.sh/airut/config"
)

// narrate is a reply that says text and then calls roll.
func narrate(text, id string) reply {
	return reply{content: []provider.Content{
		provider.TextBlock{Text: text},
		provider.ToolCallBlock{ID: id, Name: "roll", Arguments: json.RawMessage(`{"dice":"1d6"}`)},
	}, stop: provider.StopToolUse, tokens: 100}
}

// cutOff is a reply the output limit stopped, saying text.
func cutOff(text string) reply {
	r := reply{stop: provider.StopLength, tokens: 100}
	if text != "" {
		r.content = []provider.Content{provider.TextBlock{Text: text}}
	}
	return r
}

// cutOffCalling is a reply the output limit stopped inside a call to
// roll, after saying text.
func cutOffCalling(text, id string) reply {
	r := narrate(text, id)
	r.stop, r.toolStarts = provider.StopLength, true
	return r
}

// logs points e's agent at a buffer and returns a function that reads it.
func (e *env) logs() func() string {
	var mu sync.Mutex
	var b strings.Builder
	e.agent.log = slogTo(func(p []byte) {
		mu.Lock()
		defer mu.Unlock()
		b.Write(p)
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return b.String()
	}
}

// finishedLine is the "agent turn finished" line in logs.
func finishedLine(t *testing.T, logs string) string {
	t.Helper()
	for line := range strings.Lines(logs) {
		if strings.Contains(line, `msg="agent turn finished"`) {
			return line
		}
	}
	t.Fatalf("no agent turn finished line in %q", logs)
	return ""
}

func TestALengthStopAfterToolStepsTellsTheRoom(t *testing.T) {
	for _, c := range []struct {
		name    string
		last    reply
		notices Notices
		want    string
	}{
		{"with text", cutOff("The tags are"), Notices{},
			"Let me check. Checking again. The tags are\n\n" + DefaultCutOffNotice},
		{"without text", cutOff(""), Notices{},
			"Let me check. Checking again. \n\n" + DefaultCutOffNotice},
		{"the app's notice", cutOff("The tags are"), Notices{CutOff: "(Too long; ask for less.)"},
			"Let me check. Checking again. The tags are\n\n(Too long; ask for less.)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil, nil, airut.Config{})
			e.agent.notices = c.notices.withDefaults()
			logs := e.logs()
			e.ask(airut.Turn{Prompt: "tag it", Tools: e.bot.Tools("roll")})
			e.model.play(narrate("Let me check. ", "c1"), narrate("Checking again. ", "c2"), c.last)
			evs := airuttest.Start(t, e.bot).Text("!ask")

			if got := final(evs); got != c.want {
				t.Errorf("answer = %q, want %q", got, c.want)
			}
			if n := len(e.model.requests()); n != 3 {
				t.Errorf("%d model requests, want 3", n)
			}
			if line := finishedLine(t, logs()); !strings.Contains(line, " stop=length ") || !strings.Contains(line, " requests=3 ") {
				t.Errorf("finished line %q, want stop=length after 3 requests", line)
			}
			m := e.bot.Metrics()
			if s, _ := m.Get("airut_agent_stops_total", "command", "ask", "stop", "length"); s.Value != 1 {
				t.Errorf("length stops = %v, want 1", s.Value)
			}
			if s, _ := m.Get("airut_agent_stops_total", "command", "ask", "stop", "done"); s.Value != 0 {
				t.Errorf("done stops = %v, want 0", s.Value)
			}
		})
	}
}

func TestALengthStopAloneIsTheWholeReply(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "tag it", Tools: e.bot.Tools("roll")})
	e.model.play(call("c1", "roll", `{"dice":"1d6"}`), cutOff(""))
	evs := airuttest.Start(t, e.bot).Text("!ask")
	if got := final(evs); got != DefaultCutOffNotice {
		t.Errorf("answer = %q, want only the notice", got)
	}
}

func TestATurnThatEndsIsDone(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	logs := e.logs()
	e.ask(airut.Turn{Prompt: "roll", Tools: e.bot.Tools("roll")})
	e.model.play(call("c1", "roll", `{"dice":"1d6"}`), say("You rolled."))
	evs := airuttest.Start(t, e.bot).Text("!ask")
	if got := final(evs); got != "You rolled." {
		t.Errorf("answer = %q", got)
	}
	if line := finishedLine(t, logs()); !strings.Contains(line, " stop=done ") {
		t.Errorf("finished line %q, want stop=done", line)
	}
}

func TestNewWarnsOfAModelWithNoOutputLimit(t *testing.T) {
	reg := provider.NewRegistry()
	reg.RegisterExtraModel(provider.Model{Provider: "fake", ID: "open"})
	reg.RegisterExtraModel(provider.Model{Provider: "fake", ID: "capped", MaxOutput: 4096})
	db := openDB(t, filepath.Join(t.TempDir(), "s.db"))
	var logs strings.Builder
	limited := config.ChainEntry{Provider: "fake", Model: "small", Served: "open"}
	limited.MaxOutput = 2048
	_, err := New(Options{
		Clients: map[string]provider.Client{"fake": &fake{}}, Catalog: reg, Sessions: db,
		Models: config.Chain{
			{Provider: "fake", Model: "big", Served: "open"},
			limited,
			{Provider: "fake", Model: "capped"},
			{Provider: "fake", Model: "other", Served: "open"},
		},
		Logger: slogTo(func(p []byte) { logs.Write(p) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	var warned []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "no output limit") {
			warned = append(warned, strings.TrimSpace(line[strings.Index(line, "provider="):]))
		}
	}
	if want := []string{"provider=fake model=big", "provider=fake model=other"}; strings.Join(warned, "|") != strings.Join(want, "|") {
		t.Errorf("warned of %q, want %q", warned, want)
	}
}

func TestFinalOnlyPostsOnlyTheLastRequestsText(t *testing.T) {
	for _, c := range []struct {
		name      string
		finalOnly bool
		last      reply
		notices   Notices
		want      []string // every reply and edit, in order
	}{
		{"by default, everything", false, say("The tags are x."), Notices{},
			[]string{"reply:Let me check. ", "edit:Let me check. Checking again. ", "edit:Let me check. Checking again. The tags are x."}},
		{"by default, an empty answer leaves the narration", false, reply{stop: provider.StopEnd, tokens: 100}, Notices{},
			[]string{"reply:Let me check. ", "edit:Let me check. Checking again. "}},
		{"final only", true, say("The tags are x."), Notices{},
			[]string{"reply:The tags are x."}},
		{"final only, an empty answer", true, reply{stop: provider.StopEnd, tokens: 100}, Notices{},
			[]string{"reply:" + DefaultNoAnswerNotice}},
		{"final only, a blank answer, in the app's words", true, say(" \n"), Notices{NoAnswer: "Nothing to add."},
			[]string{"reply:Nothing to add."}},
		{"final only, cut off", true, cutOff("The tags are"), Notices{},
			[]string{"reply:The tags are", "edit:The tags are\n\n" + DefaultCutOffNotice}},
		{"final only, cut off with nothing said", true, cutOff(""), Notices{},
			[]string{"reply:" + DefaultCutOffNotice}},
		{"final only, cut off inside a tool call", true, cutOffCalling("One more look. ", "c3"), Notices{},
			[]string{"reply:" + DefaultCutOffNotice}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil, nil, airut.Config{})
			e.agent.notices = c.notices.withDefaults()
			e.ask(airut.Turn{Prompt: "tag it", Tools: e.bot.Tools("roll"), FinalOnly: c.finalOnly})
			e.model.play(narrate("Let me check. ", "c1"), narrate("Checking again. ", "c2"), c.last)
			evs := airuttest.Start(t, e.bot).Text("!ask")
			var got []string
			for _, ev := range evs {
				if ev.Kind == "reply" || ev.Kind == "edit" {
					got = append(got, ev.Kind+":"+ev.Text)
				}
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("posted %q, want %q", got, c.want)
			}
			if rolls := e.rolls(); len(rolls) != 2 {
				t.Errorf("rolled %q, want both tool steps run", rolls)
			}
		})
	}
}

func TestFinalOnlyAtTheBudget(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "keep rolling", Tools: e.bot.Tools("roll"), FinalOnly: true, Budget: airut.Budget{Turns: 2}})
	e.model.play(narrate("Rolling. ", "c1"), narrate("Again. ", "c2"), say("never asked"))
	evs := airuttest.Start(t, e.bot).Text("!ask")
	if got, want := final(evs), "(Stopped: this turn used all 2 of its model requests.)"; got != want {
		t.Errorf("answer = %q, want %q", got, want)
	}
}

func TestFinalOnlyText(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	var got string
	e.bot.Command("ask", "Ask", func(c *airut.Ctx) error {
		var err error
		got, err = c.Agent(airut.Turn{Prompt: "tag it", Tools: e.bot.Tools("roll")}).FinalOnly().Text()
		return err
	})
	e.model.play(narrate("Let me check. ", "c1"), say("The tags are x."))
	if evs := airuttest.Start(t, e.bot).Text("!ask"); len(evs) != 0 {
		t.Errorf("Text posted %d events", len(evs))
	}
	if got != "The tags are x." {
		t.Errorf("Text() = %q, want the answer", got)
	}
}
