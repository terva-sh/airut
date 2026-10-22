package airut_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// agentFunc is an Agent whose turn is a function.
type agentFunc func(c *airut.Ctx, out airut.TurnOutput) error

func (f agentFunc) Run(c *airut.Ctx, _ airut.Turn, out airut.TurnOutput) error { return f(c, out) }

func TestFinalOnly(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{
		{Text: "Let me "}, {Text: "roll. "},
		{Call: "roll", Args: `{"dice":"1d6"}`},
		{Text: "Checking again."},
		{Call: "roll", Args: `{"dice":"1d6"}`},
		{Text: "You got "}, {Text: "4."},
	}}
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent, EditInterval: time.Hour})
	var text string
	b.Command("text", "Text", func(c *airut.Ctx) error {
		var err error
		text, err = c.Agent(airut.Turn{Prompt: "roll", Tools: b.Tools("roll")}).FinalOnly().Text()
		return err
	})
	b.Command("stream", "Stream", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "roll", Tools: b.Tools("roll")}).FinalOnly().StreamTo(c)
	})
	b.Command("all", "Stream all", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "roll", Tools: b.Tools("roll")}).StreamTo(c)
	})
	a := airuttest.Start(t, b)

	a.Text("!text")
	if text != "You got 4." {
		t.Errorf("Text() = %q, want the last segment", text)
	}
	// The agent leaves the narration out, so the answer is posted once,
	// and nothing is posted to be replaced.
	wantEvents(t, a.Text("!stream"), "reply:You got 4.")
	// Without FinalOnly the segments add up, and the throttle is still
	// flushed at each call.
	wantEvents(t, a.Text("!all"), "reply:Let me ", "edit:Let me roll. ", "edit:Let me roll. Checking again.",
		"edit:Let me roll. Checking again.You got 4.")
	if turns := agent.Turns(); !turns[0].FinalOnly || !turns[1].FinalOnly || turns[2].FinalOnly {
		t.Errorf("Turn.FinalOnly = %v, %v, %v; want it set by FinalOnly only", turns[0].FinalOnly, turns[1].FinalOnly, turns[2].FinalOnly)
	}
}

// TestFinalOnlyWithAnAgentThatIgnoresIt has the core keep only the last
// segment itself, for an agent that passes the narration on anyway.
func TestFinalOnlyWithAnAgentThatIgnoresIt(t *testing.T) {
	agent := agentFunc(func(c *airut.Ctx, out airut.TurnOutput) error {
		out.Text("Let me ")
		out.Text("roll. ")
		out.ToolCall("roll")
		out.Text("Checking again.")
		out.ToolCall("roll")
		out.Text("You got ")
		out.Text("4.")
		return nil
	})
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent, EditInterval: time.Hour})
	var text string
	b.Command("text", "Text", func(c *airut.Ctx) error {
		var err error
		text, err = c.Agent(airut.Turn{Prompt: "roll"}).FinalOnly().Text()
		return err
	})
	b.Command("stream", "Stream", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "roll"}).FinalOnly().StreamTo(c)
	})
	a := airuttest.Start(t, b)

	a.Text("!text")
	if text != "You got 4." {
		t.Errorf("Text() = %q, want the last segment", text)
	}
	// Each tool call sends what the hour-long throttle held back, and
	// each segment replaces the last.
	wantEvents(t, a.Text("!stream"), "reply:Let me ", "edit:Let me roll. ", "edit:Checking again.", "edit:You got 4.")
}

func TestRunTo(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Text: "a"}, {Call: "roll", Args: `{"dice":"1d6"}`}, {Text: "b"}}}
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent})
	rec := &recorder{}
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "go", Tools: b.Tools("roll")}).FinalOnly().RunTo(rec)
	})
	if evs := airuttest.Start(t, b).Text("!ask"); len(evs) != 0 {
		t.Errorf("RunTo posted %q; the app presents the answer", texts(evs))
	}
	if got := strings.Join(rec.got, "|"); got != "text:a|call:roll|text:b" {
		t.Errorf("RunTo passed on %q, want everything as it came", got)
	}
}

type recorder struct{ got []string }

func (r *recorder) Text(d string)        { r.got = append(r.got, "text:"+d) }
func (r *recorder) ToolCall(name string) { r.got = append(r.got, "call:"+name) }

// TestAskPostsBelowACompleteReply has the agent ask with no tool call
// first, as an approval check does, while the throttle holds text back.
func TestAskPostsBelowACompleteReply(t *testing.T) {
	agent := agentFunc(func(c *airut.Ctx, out airut.TurnOutput) error {
		out.Text("Let me ")
		out.Text("confirm the change.")
		if _, err := c.Ask("Allow it?", "yes", "no"); err != nil {
			return err
		}
		out.Text(" Done.")
		return nil
	})
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent, EditInterval: time.Hour})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "go"}).StreamTo(c)
	})
	a := airuttest.Start(t, b)
	a.OnAsk(func(airut.Ask) (string, error) { return "yes", nil })
	wantEvents(t, a.Text("!ask"), "reply:Let me ", "edit:Let me confirm the change.", "ask:Allow it?",
		"edit:Let me confirm the change. Done.")
}

func TestTypingKeepalive(t *testing.T) {
	agent := agentFunc(func(c *airut.Ctx, out airut.TurnOutput) error {
		time.Sleep(60 * time.Millisecond)
		if _, err := c.Ask("Which?", "a", "b"); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
		out.Text("Done.")
		time.Sleep(30 * time.Millisecond)
		return nil
	})
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent, TypingDelay: 10 * time.Millisecond, TypingEvery: 10 * time.Millisecond})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "go"}).StreamTo(c)
	})
	a := airuttest.Start(t, b)
	var during []string
	a.OnAsk(func(airut.Ask) (string, error) {
		n := len(a.Events())
		time.Sleep(60 * time.Millisecond)
		during = texts(a.Events()[n:])
		return "a", nil
	})
	evs := texts(a.Text("!ask"))

	ask := slices.Index(evs, "ask:Which?")
	if ask < 0 {
		t.Fatalf("no question in %q", evs)
	}
	before, after := evs[:ask], evs[ask+1:]
	// Renewed while the turn worked, then taken down for the question.
	if n := slices.Index(before, "typing:"); n < 0 || len(before)-n < 3 || before[len(before)-1] != "typing-stop:" {
		t.Errorf("before the question: %q, want renewals and then the notice taken down", before)
	}
	// Nothing renews it while the question waits.
	if len(during) != 0 {
		t.Errorf("while the question waited: %q", during)
	}
	// Renewed once it was answered, and taken down when the turn ended.
	if slices.Index(after, "typing:") < 0 || after[len(after)-1] != "typing-stop:" || !slices.Contains(after, "reply:Done.") {
		t.Errorf("after the question: %q, want renewals, the answer, and the notice taken down", after)
	}
}

func TestTypingWaitsItsDelay(t *testing.T) {
	agent := agentFunc(func(c *airut.Ctx, out airut.TurnOutput) error {
		out.Text("At once.")
		return nil
	})
	for _, every := range []time.Duration{0, -1} {
		b, _ := newBot(airut.Config{Gate: openGate, Agent: agent, TypingEvery: every})
		b.Command("ask", "Ask", func(c *airut.Ctx) error {
			return c.Agent(airut.Turn{Prompt: "go"}).StreamTo(c)
		})
		// A turn that answers inside the default second shows no notice,
		// and one with the keepalive off shows none without a tool call.
		wantEvents(t, airuttest.Start(t, b).Text("!ask"), "reply:At once.")
	}
}
