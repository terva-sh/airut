package agent

import (
	"strings"
	"testing"
	"time"

	"terva.sh/terva/packages/core"
	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func msg(role provider.Role, s string, at time.Time) provider.Message {
	return provider.Message{Role: role, Content: []provider.Content{provider.TextBlock{Text: s}}, Time: at}
}

func roles(ms []provider.Message) string {
	var b strings.Builder
	for _, m := range ms {
		b.WriteString(string(m.Role)[:1])
	}
	return b.String()
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-2*time.Hour), now.Add(-time.Minute)
	// A person, an answer with a tool call, its result, and an answer,
	// twice: the second exchange is recent.
	ms := []provider.Message{
		msg(provider.RoleUser, "q1", old), msg(provider.RoleAssistant, "call", old),
		msg(provider.RoleTool, "result", old), msg(provider.RoleAssistant, "a1", old),
		msg(provider.RoleUser, "q2", recent), msg(provider.RoleAssistant, "call", recent),
		msg(provider.RoleTool, "result", recent), msg(provider.RoleAssistant, "a2", recent),
	}
	tr := core.Transcript{Messages: ms, ResumeContext: provider.Usage{InputTokens: 900}}
	for _, c := range []struct {
		name  string
		h     airut.History
		roles string
		due   bool
	}{
		{"no bounds", airut.History{}, "uatauata", false},
		{"too old", airut.History{MaxAge: time.Hour}, "uata", false},
		{"too old, whatever the compaction", airut.History{MaxAge: time.Hour, Compaction: airut.CompactSummarize}, "uata", false},
		// Dropping three would open on a tool result; the cut moves on
		// to the next person's message.
		{"dropped to fit", airut.History{MaxMessages: 5, Compaction: airut.CompactDrop}, "uata", false},
		{"summarized to fit", airut.History{MaxMessages: 5, Compaction: airut.CompactSummarize}, "uatauata", true},
		{"dropped to fit the tokens", airut.History{MaxTokens: 4 * estimate(ms[0]), Compaction: airut.CompactDrop}, "", false},
		{"within bounds", airut.History{MaxMessages: 8, MaxAge: 3 * time.Hour, Compaction: airut.CompactDrop}, "uatauata", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, due := window(tr, c.h, now)
			if roles(got.Messages) != c.roles || due != c.due {
				t.Fatalf("window = %s, due %v; want %s, due %v", roles(got.Messages), due, c.roles, c.due)
			}
			if trimmed := len(got.Messages) < len(ms); trimmed != (got.ResumeContext == provider.Usage{}) {
				t.Errorf("trimmed %v but the resume gauge is %+v", trimmed, got.ResumeContext)
			}
		})
	}

	// A message terva recorded no time for, such as a summary, is kept.
	undated := []provider.Message{msg(provider.RoleUser, "summary", time.Time{}), msg(provider.RoleAssistant, "ok", time.Time{})}
	if got, _ := window(core.Transcript{Messages: undated}, airut.History{MaxAge: time.Second}, now); len(got.Messages) != 2 {
		t.Errorf("kept %d undated messages, want both", len(got.Messages))
	}
}

func TestHistoryDefaults(t *testing.T) {
	def := airut.History{MaxMessages: 10, MaxTokens: 1000, MaxAge: time.Hour}
	if got := history(airut.History{MaxMessages: 4, Compaction: airut.CompactDrop}, def); got != (airut.History{MaxMessages: 4, MaxTokens: 1000, MaxAge: time.Hour, Compaction: airut.CompactDrop}) {
		t.Errorf("history = %+v", got)
	}
	if got := history(airut.History{}, airut.History{}); got != (airut.History{Compaction: airut.CompactSummarize}) {
		t.Errorf("with no bounds anywhere, history = %+v", got)
	}
	for _, h := range []airut.History{{MaxMessages: -1}, {MaxAge: -time.Second}, {Compaction: "trim"}} {
		if checkHistory(h) == nil {
			t.Errorf("%+v was accepted", h)
		}
	}
}

// sent is the roles and texts of the messages a request carried.
func sent(req provider.Request) []string {
	var out []string
	for _, m := range req.Messages {
		out = append(out, string(m.Role)+":"+text(m))
	}
	return out
}

func TestHistoryThroughTheEngine(t *testing.T) {
	t.Run("drop", func(t *testing.T) {
		e := newEnv(t, nil, nil, airut.Config{})
		e.ask(airut.Turn{Prompt: "next", Session: airut.PerChannel, History: airut.History{MaxMessages: 3, Compaction: airut.CompactDrop}})
		e.model.play(say("one"), say("two"), say("three"))
		a := airuttest.Start(t, e.bot)
		for range 3 {
			a.Text("!ask")
		}
		reqs := e.model.requests()
		// The third turn resumes four messages; the oldest goes, and with
		// it the answer it would open on.
		if got := sent(reqs[2]); strings.Join(got, "|") != "user:next|assistant:two|user:next" {
			t.Fatalf("the third turn sent %q", got)
		}
	})

	t.Run("summarize", func(t *testing.T) {
		e := newEnv(t, nil, nil, airut.Config{})
		e.ask(airut.Turn{Prompt: "next", Session: airut.PerChannel, History: airut.History{MaxMessages: 3}})
		e.model.play(say("one"), say("two"), say("SUMMARY of one and two"), say("three"))
		a := airuttest.Start(t, e.bot)
		for range 3 {
			a.Text("!ask")
		}
		reqs := e.model.requests()
		if len(reqs) != 4 {
			t.Fatalf("%d requests, want a summarizer's between the second and third turns", len(reqs))
		}
		got := strings.Join(sent(reqs[3]), "|")
		if !strings.Contains(got, "SUMMARY of one and two") || strings.Count(got, "user:next") > 2 {
			t.Fatalf("the third turn sent %q, want the summary and the recent tail", got)
		}
		if final(a.Text("!ask")) == "" {
			t.Fatal("a turn after the compaction failed")
		}
	})

	t.Run("max age", func(t *testing.T) {
		e := newEnv(t, nil, nil, airut.Config{})
		e.agent.history = airut.History{MaxAge: time.Hour}
		e.ask(airut.Turn{Prompt: "next", Session: airut.PerChannel})
		a := airuttest.Start(t, e.bot)
		a.Text("!ask")
		a.Text("!ask")
		if n := len(e.model.requests()[1].Messages); n != 3 {
			t.Fatalf("a recent exchange resumed %d messages, want 3", n)
		}
		e.agent.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		a.Text("!ask")
		if n := len(e.model.requests()[2].Messages); n != 1 {
			t.Fatalf("a stale exchange resumed %d messages, want only the prompt", n)
		}
	})
}

func TestForgetInADirectChannelClearsTheGroupConversation(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "remember me", Session: airut.PerUser})
	a := airuttest.Start(t, e.bot)
	a.Text("!ask")
	a.Text("!forget", airuttest.InDirect(), airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "dm-1"}))
	a.Text("!ask")
	if n := len(e.model.requests()[1].Messages); n != 1 {
		t.Fatalf("after forget from a direct channel, the group turn resumed %d messages, want only its prompt", n)
	}
}

func TestForgetClearsEveryChannelOfTheCaller(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	e.ask(airut.Turn{Prompt: "remember me", Session: airut.PerUserChannel})
	a := airuttest.Start(t, e.bot)
	other := airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "elsewhere"})
	a.Text("!ask")
	a.Text("!ask", other)
	// Another person's conversation in the same channel, whose id
	// user-1's is a prefix of.
	s, err := e.agent.db.Session(t.Context(), "test/user/user-10/channel/channel-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(msg(provider.RoleUser, "kept", time.Now())); err != nil {
		t.Fatal(err)
	}

	a.Text("!forget")
	a.Text("!ask")
	a.Text("!ask", other)
	reqs := e.model.requests()
	if len(reqs[2].Messages) != 1 || len(reqs[3].Messages) != 1 {
		t.Fatalf("after forget the turns resumed %d and %d messages, want none", len(reqs[2].Messages), len(reqs[3].Messages))
	}
	if tr, err := s.Reload(); err != nil || len(tr.Messages) != 1 {
		t.Fatalf("user-10's conversation, whose id user-1's extends, was forgotten too (%v)", err)
	}
}
