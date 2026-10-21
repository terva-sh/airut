package airuttest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// fatalTB records what Start reports. Fatalf stops the caller with a
// sentinel panic instead of the test's goroutine.
type fatalTB struct {
	testing.TB
	cleanups []func()
	fatal    string
	errs     []string
}

type stopped struct{}

func (f *fatalTB) Helper()                   {}
func (f *fatalTB) Cleanup(fn func())         { f.cleanups = append(f.cleanups, fn) }
func (f *fatalTB) Errorf(s string, a ...any) { f.errs = append(f.errs, fmt.Sprintf(s, a...)) }
func (f *fatalTB) Fatalf(s string, a ...any) {
	f.fatal = fmt.Sprintf(s, a...)
	panic(stopped{})
}

func TestStartReportsAnEarlyFailureWithoutHanging(t *testing.T) {
	b := airut.New(airut.Config{})
	// A bot already running cannot Run again, so Start's Run fails at once.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := airuttest.New("first")
	go b.Run(ctx, first)
	<-first.Ready()

	tb := &fatalTB{TB: t}
	func() {
		defer func() {
			if r := recover(); r != (stopped{}) {
				panic(r)
			}
		}()
		airuttest.Start(tb, b)
	}()
	if !strings.Contains(tb.fatal, "Run called twice") {
		t.Errorf("Fatalf = %q", tb.fatal)
	}

	done := make(chan struct{})
	go func() {
		for i := len(tb.cleanups) - 1; i >= 0; i-- {
			tb.cleanups[i]()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start's cleanup hung after an early failure")
	}
	if len(tb.errs) != 0 {
		t.Errorf("cleanup reported the failure again: %q", tb.errs)
	}
}

func TestKeyedMessagesAreSentOnce(t *testing.T) {
	b := airut.New(airut.Config{Prefix: "!"})
	var refs []airut.MessageRef
	b.Command("twice", "Reply twice with one key", func(c *airut.Ctx) error {
		for range 2 {
			ref, err := c.Send(airut.Message{Text: "once", Key: "reply-key"})
			if err != nil {
				return err
			}
			refs = append(refs, ref)
		}
		return nil
	})
	a := airuttest.Start(t, b)
	got := a.Text("!twice")
	if len(got) != 1 || len(refs) != 2 || refs[0] != refs[1] || refs[0].ID != got[0].Ref {
		t.Errorf("a keyed reply sent twice: events %+v, refs %v", got, refs)
	}
	ch := airut.Channel{Platform: airuttest.Platform, ID: "room"}
	first, _ := a.Send(context.Background(), ch, airut.Message{Text: "x", Key: "send-key"})
	again, _ := a.Send(context.Background(), ch, airut.Message{Text: "x", Key: "send-key"})
	other, _ := a.Send(context.Background(), ch, airut.Message{Text: "x", Key: "other-key"})
	plain1, _ := a.Send(context.Background(), ch, airut.Message{Text: "x"})
	plain2, _ := a.Send(context.Background(), ch, airut.Message{Text: "x"})
	if first != again || other == first || plain1 == plain2 {
		t.Errorf("sends: %v %v %v %v %v", first, again, other, plain1, plain2)
	}
	if n := len(a.Events()); n != 5 {
		t.Errorf("%d events recorded, want 5: the reply, then send-key, other-key, and two unkeyed sends", n)
	}
}

// TestConcurrentKeyedSendsRecordOne races replies and Conn sends that
// share a key, round after round with a fresh key each: every round
// records exactly one message, and all its sends get that message's ref.
func TestConcurrentKeyedSendsRecordOne(t *testing.T) {
	const rounds, racers = 200, 8
	b := airut.New(airut.Config{Prefix: "!"})
	var a *airuttest.Adapter
	bad := 0
	b.Command("race", "Send one key many ways at once", func(c *airut.Ctx) error {
		for round := range rounds {
			key := fmt.Sprintf("race-%d", round)
			refs := make([]airut.MessageRef, racers)
			var wg sync.WaitGroup
			// go releases the racers together, so their sends overlap.
			start := make(chan struct{})
			for i := range racers {
				wg.Go(func() {
					<-start
					if i%2 == 0 {
						refs[i], _ = c.Responder().Reply(c.Context(), airut.Message{Text: "once", Key: key})
					} else {
						refs[i], _ = a.Send(c.Context(), c.Channel(), airut.Message{Text: "once", Key: key})
					}
				})
			}
			close(start)
			wg.Wait()
			for _, r := range refs {
				if r != refs[0] || r.ID == "" {
					bad++
					break
				}
			}
		}
		return nil
	})
	a = airuttest.Start(t, b)
	a.Text("!race")
	if n := len(a.Events()); n != rounds || bad != 0 {
		t.Errorf("%d messages recorded for %d keys; %d rounds handed out differing refs", n, rounds, bad)
	}
}

func TestMentionCallsACommandOrReachesTheHandlers(t *testing.T) {
	b := airut.New(airut.Config{Prefix: "!"})
	b.Command("ping", "Answer pong", func(c *airut.Ctx) error {
		_, err := c.Reply("pong")
		return err
	})
	var seen []airut.MessageEvent
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		seen = append(seen, m)
		return nil
	})
	a := airuttest.Start(t, b)
	for _, line := range []string{"ping", "!ping"} {
		if got := a.Mention(line); len(got) != 1 || got[0].Text != "pong" {
			t.Errorf("Mention(%q) did %+v; want the ping command's answer", line, got)
		}
	}
	a.Mention(" find a fox ")
	a.Text("lunch?")
	if len(seen) != 2 {
		t.Fatalf("the handlers saw %d messages, want 2", len(seen))
	}
	if m := seen[0]; !m.Mentioned || m.Request != "find a fox " || m.Text != airuttest.BotName+":  find a fox " {
		t.Errorf("a mention arrived as Mentioned %v, Request %q, Text %q", m.Mentioned, m.Request, m.Text)
	}
	if m := seen[1]; m.Mentioned || m.Request != "" {
		t.Errorf("plain chat arrived as Mentioned %v, Request %q", m.Mentioned, m.Request)
	}
}

func TestDeliveredMessagesAreDated(t *testing.T) {
	b := airut.New(airut.Config{})
	var seen []airut.MessageEvent
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		seen = append(seen, m)
		return nil
	})
	a := airuttest.Start(t, b)
	before := time.Now()
	a.Text("now")
	long := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a.Text("then", airuttest.SentAt(long))
	a.Mention("then too", airuttest.SentAt(long))
	if len(seen) != 3 || seen[0].Sent.Before(before) || !seen[1].Sent.Equal(long) || !seen[2].Sent.Equal(long) {
		t.Errorf("messages were sent at %v, %v, %v", seen[0].Sent, seen[1].Sent, seen[2].Sent)
	}
}

// TestAMessageInAThreadIsAnsweredThere: as on Matrix, a reply to a
// command or a message delivered in a thread goes into that thread.
func TestAMessageInAThreadIsAnsweredThere(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("ping", "Answer pong", func(c *airut.Ctx) error {
		_, err := c.Reply("pong")
		return err
	})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		_, err := c.Reply("heard")
		return err
	})
	a := airuttest.Start(t, b)
	for _, evs := range [][]airuttest.Event{a.Text("!ping", airuttest.InThread("t1")), a.Text("hello", airuttest.InThread("t1"))} {
		if len(evs) != 1 || evs[0].Thread != "t1" {
			t.Errorf("a message in thread t1 was answered with %+v", evs)
		}
	}
	if evs := a.Text("!ping"); len(evs) != 1 || evs[0].Thread != "" {
		t.Errorf("a message outside any thread was answered with %+v", evs)
	}
}

// TestConnReactionsSwap swaps a status reaction through the bot's Conn,
// as a pipeline outside any invocation does, and checks what the Conn
// refuses.
func TestConnReactionsSwap(t *testing.T) {
	b := airut.New(airut.Config{Prefix: "!"})
	var conn airut.Conn
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		var err error
		conn, err = c.Conn()
		if err != nil {
			return err
		}
		seen, err := conn.React(c.Context(), c.Channel(), m.Message, "👀")
		if err != nil {
			return err
		}
		if err := conn.Unreact(c.Context(), c.Channel(), seen); err != nil {
			return err
		}
		_, err = conn.React(c.Context(), c.Channel(), m.Message, "✅")
		return err
	})
	a := airuttest.Start(t, b)
	evs := a.Text("a picture")
	want := []string{"react 👀", "unreact 👀", "react ✅"}
	if len(evs) != len(want) {
		t.Fatalf("events = %+v", evs)
	}
	for i, e := range evs {
		if got := e.Kind + " " + e.Text; got != want[i] || e.Channel != "channel-1" || e.Ref == "" || e.Reaction == "" {
			t.Errorf("event %d = %+v, want %s", i, e, want[i])
		}
	}
	if evs[0].Reaction != evs[1].Reaction || evs[1].Reaction == evs[2].Reaction {
		t.Errorf("the retraction names %q, the reactions %q and %q", evs[1].Reaction, evs[0].Reaction, evs[2].Reaction)
	}

	ctx, ch, msg := context.Background(), airut.Channel{Platform: airuttest.Platform, ID: "channel-1"}, airut.MessageRef{ID: evs[0].Ref}
	again, err := conn.React(ctx, ch, msg, "✅")
	if err != nil || again.ID != evs[2].Reaction {
		t.Errorf("reacting again = %v, %v, want the standing reaction %s", again, err, evs[2].Reaction)
	}
	for _, c := range []struct {
		name string
		ch   airut.Channel
		ref  airut.MessageRef
	}{
		{"a message", ch, msg},
		{"no ref", ch, airut.MessageRef{}},
		{"another channel's reaction", airut.Channel{ID: "elsewhere"}, again},
	} {
		if err := conn.Unreact(ctx, c.ch, c.ref); !errors.Is(err, airut.ErrNotOwnReaction) {
			t.Errorf("unreacting %s = %v, want ErrNotOwnReaction", c.name, err)
		}
	}
	if err := conn.Unreact(ctx, ch, airut.MessageRef{ID: evs[0].Reaction}); err != nil {
		t.Errorf("retracting a retracted reaction = %v", err)
	}
	if n := len(a.Events()); n != 3 {
		t.Errorf("%d events after the repeats and refusals, want the 3 swaps alone", n)
	}
}

// TestDirectMessages delivers lines in a direct channel: a bare command
// name runs the command, other chat reaches OnMessage as addressed, and
// the Ctx reports the channel as direct.
func TestDirectMessages(t *testing.T) {
	b := airut.New(airut.Config{Prefix: "!"})
	var direct []bool
	b.Command("ping", "Answer pong", func(c *airut.Ctx) error {
		direct = append(direct, c.Direct())
		_, err := c.Reply("pong")
		return err
	})
	var got []airut.MessageEvent
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		direct = append(direct, c.Direct())
		got = append(got, m)
		return nil
	})
	a := airuttest.Start(t, b)
	for _, line := range []string{"ping", "!ping"} {
		if evs := a.DirectMessage(line); len(evs) != 1 || evs[0].Text != "pong" {
			t.Errorf("%q in a direct channel = %+v", line, evs)
		}
	}
	a.DirectMessage("hello there")
	a.Text("hello all")
	if len(got) != 2 || !got[0].Direct || !got[0].Mentioned || got[0].Request != "hello there" || got[1].Direct || got[1].Mentioned {
		t.Errorf("messages = %+v", got)
	}
	if fmt.Sprint(direct) != "[true true true false]" {
		t.Errorf("Ctx.Direct reported %v", direct)
	}

	conn := a.Conn()
	ch := airut.Channel{Platform: airuttest.Platform, ID: "dm-1"}
	alice := airut.User{Platform: airuttest.Platform, ID: "alice"}
	a.SetDirect("dm-1", alice)
	if who, ok, err := conn.Direct(context.Background(), ch); !ok || err != nil || who != alice {
		t.Errorf("Direct after SetDirect = %v, %v, %v", who, ok, err)
	}
	a.SetDirect("dm-1", airut.User{})
	if _, ok, _ := conn.Direct(context.Background(), ch); ok {
		t.Error("Direct still reports a channel SetDirect cleared")
	}
}
