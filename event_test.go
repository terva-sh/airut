package airut_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// seen collects what handlers saw, for a test to read after delivery.
type seen struct {
	mu     sync.Mutex
	events []string
}

func (s *seen) add(format string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, format)
}

func (s *seen) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func TestOnMessageSeesChatButNotCommands(t *testing.T) {
	b, _ := newBot(airut.Config{})
	var got []airut.MessageEvent
	var mu sync.Mutex
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		if c.Caller() != m.Sender || c.Message() != m.Message || c.ReplyTo() != m.ReplyTo || c.Channel() != m.Channel {
			t.Errorf("the handler's Ctx does not match its event: caller %v, message %v, reply to %v", c.Caller(), c.Message(), c.ReplyTo())
		}
		if strings.HasPrefix(m.Text, "echo") {
			_, err := c.Reply("heard " + m.Text)
			return err
		}
		return nil
	})
	a := airuttest.Start(t, b)

	img := airut.Attachment{Name: "cat.png", ContentType: "image/png", Size: 3, URL: "mxc://example.org/cat"}
	if evs := a.Text("look at this", airuttest.WithFiles(img), airuttest.ReplyingTo(airut.MessageRef{ID: "earlier"})); len(evs) != 0 {
		t.Errorf("a quiet handler made the bot do %q", texts(evs))
	}
	wantEvents(t, a.Text("echo hi"), "reply:heard echo hi")
	wantEvents(t, a.Text("!roll 2d6"), "reply:rolled 2d6 x1")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the handler saw %d messages, want the two that were not commands", len(got))
	}
	m := got[0]
	if m.Text != "look at this" || !reflect.DeepEqual(m.Files, []airut.Attachment{img}) || m.ReplyTo.ID != "earlier" ||
		m.Sender.ID != "user-1" || m.Channel.ID != "channel-1" || m.Message.ID == "" || m.ID == "" {
		t.Errorf("the handler saw %+v", m)
	}
}

func TestHandlersRunInOrderAndFailQuietly(t *testing.T) {
	b, logs := newBot(airut.Config{})
	var s seen
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		s.add("first")
		return errors.New("the database at secret-host said no to " + m.Text)
	})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		s.add("second")
		panic("boom")
	})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		s.add("third")
		return airut.Errorf("not for the chat")
	})
	a := airuttest.Start(t, b)

	if evs := a.Text("private words"); len(evs) != 0 {
		t.Errorf("failing handlers answered in the chat: %q", texts(evs))
	}
	if got := s.list(); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Errorf("handlers ran %v, want all three in order", got)
	}
	l := logs.String()
	for _, want := range []string{"event handler failed", "error_kind=handler", "event handler panicked", "error_kind=user"} {
		if !strings.Contains(l, want) {
			t.Errorf("the log lacks %q:\n%s", want, l)
		}
	}
	for _, leak := range []string{"private words", "secret-host", "not for the chat"} {
		if strings.Contains(l, leak) {
			t.Errorf("the log holds %q:\n%s", leak, l)
		}
	}
}

func TestOnReaction(t *testing.T) {
	b, _ := newBot(airut.Config{})
	var got []airut.ReactionEvent
	var mu sync.Mutex
	b.OnReaction(func(c *airut.Ctx, r airut.ReactionEvent) error {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		if c.Message() != r.Target || c.Caller() != r.Sender {
			t.Errorf("the handler's Ctx does not match its reaction")
		}
		if r.Emoji == "✅" {
			_, err := c.Send(airut.Message{Text: "approved", ReplyTo: r.Target})
			return err
		}
		return nil
	})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		t.Error("a reaction reached a message handler")
		return nil
	})
	a := airuttest.Start(t, b)

	// A message the bot posted before a restart: only its platform id is
	// known now.
	review := airut.MessageRef{ID: "$review-posted-yesterday"}
	evs := a.React(review, "✅", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "mod-1"}))
	wantEvents(t, evs, "reply:approved")
	if len(evs) == 1 && evs[0].ReplyTo != review.ID {
		t.Errorf("the reply answered %q, want %q", evs[0].ReplyTo, review.ID)
	}
	a.React(review, "❌")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Target != review || got[0].Emoji != "✅" || got[0].Sender.ID != "mod-1" || got[1].Emoji != "❌" {
		t.Errorf("the handler saw %+v", got)
	}
}

func TestEventHandlersCanStartAgentTurns(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Text: "a cat"}}}
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		return c.Agent(airut.Turn{Prompt: "describe", Session: airut.PerUser}).StreamTo(c)
	})
	a := airuttest.Start(t, b)
	wantEvents(t, a.Text("what is this"), "reply:a cat")
	// The gate still applies: this sender is on no access list.
	if evs := a.Text("and this", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "stranger"})); len(evs) != 0 {
		t.Errorf("a refused turn from a handler answered in the chat: %q", texts(evs))
	}
	if n := len(agent.Turns()); n != 1 {
		t.Errorf("%d turns reached the agent, want 1", n)
	}
}

func TestACommandKnowsWhatItRepliesTo(t *testing.T) {
	b, _ := newBot(airut.Config{})
	var got airut.MessageRef
	b.Command("tags", "Edit tags", func(c *airut.Ctx) error {
		got = c.ReplyTo()
		return nil
	})
	a := airuttest.Start(t, b)
	a.Text("!tags", airuttest.ReplyingTo(airut.MessageRef{ID: "$review"}))
	if got.ID != "$review" {
		t.Errorf("ReplyTo = %q, want the message the command replied to", got.ID)
	}
}

func TestHandlersCannotBeAddedWhileRunning(t *testing.T) {
	b, _ := newBot(airut.Config{})
	airuttest.Start(t, b)
	for name, add := range map[string]func(){
		"OnMessage":  func() { b.OnMessage(func(*airut.Ctx, airut.MessageEvent) error { return nil }) },
		"OnReaction": func() { b.OnReaction(func(*airut.Ctx, airut.ReactionEvent) error { return nil }) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s after Run did not panic", name)
				}
			}()
			add()
		}()
	}
}

func TestReactionsKeepTheirThread(t *testing.T) {
	b, _ := newBot(airut.Config{})
	var keys []string
	var mu sync.Mutex
	record := func(c *airut.Ctx) {
		mu.Lock()
		keys = append(keys, c.SessionKey(airut.PerThread))
		mu.Unlock()
	}
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error { record(c); return nil })
	b.OnReaction(func(c *airut.Ctx, r airut.ReactionEvent) error { record(c); return nil })
	a := airuttest.Start(t, b)
	a.Text("in the thread", airuttest.InThread("t7"))
	a.React(airut.MessageRef{ID: "$m"}, "👍", airuttest.InThread("t7"))
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != keys[1] || !strings.Contains(keys[1], "t7") {
		t.Errorf("session keys = %q; a reaction in a thread should share its message's thread session", keys)
	}
}

// pointerAdapter delivers events as pointers, which Event allows.
type pointerAdapter struct{ events []airut.Event }

func (pointerAdapter) Platform() airut.Platform { return airuttest.Platform }

func (p pointerAdapter) Run(ctx context.Context, h airut.Host) error {
	for _, ev := range p.events {
		h.Deliver(ctx, ev, nil)
	}
	return nil
}

func TestPointerEventsAreDelivered(t *testing.T) {
	b, logs := newBot(airut.Config{})
	var s seen
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error { s.add("message " + m.Text); return nil })
	b.OnReaction(func(c *airut.Ctx, r airut.ReactionEvent) error { s.add("reaction " + r.Emoji); return nil })
	var nilMsg *airut.MessageEvent
	err := b.Run(context.Background(), pointerAdapter{events: []airut.Event{
		&airut.MessageEvent{ID: "e1", Text: "hi"},
		&airut.ReactionEvent{ID: "e2", Emoji: "✅"},
		nilMsg,
		nil,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.list(); !reflect.DeepEqual(got, []string{"message hi", "reaction ✅"}) {
		t.Errorf("handlers saw %q", got)
	}
	if n := strings.Count(logs.String(), "an adapter delivered an event the core does not know"); n != 2 {
		t.Errorf("logged %d unknown events, want the nil pointer and the nil interface:\n%s", n, logs.String())
	}
}
