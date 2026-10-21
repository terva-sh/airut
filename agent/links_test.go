package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/agent/sessions"
	"terva.sh/airut/airuttest"
)

// clock is a time a test moves on, shared by the agent and its store.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestConversationsAndLinks runs Conversation turns on the real store:
// a person goes on with their conversation, starts a new one after a
// gap, and goes back to an old one by replying to its answer, while
// someone else's reply to that answer gets their own conversation.
func TestConversationsAndLinks(t *testing.T) {
	c := &clock{t: time.Now()}
	db, err := sessions.Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"), sessions.Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	both := airut.Gate{Access: map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1", "user-2"}}}}
	e := newEnv(t, db, nil, airut.Config{Gate: both})
	e.agent.now = c.now
	e.ask(airut.Turn{Prompt: "go on", Session: airut.Conversation})
	a := airuttest.Start(t, e.bot)
	seen := func() int {
		reqs := e.model.requests()
		return len(reqs[len(reqs)-1].Messages)
	}
	answer := func(evs []airuttest.Event) airut.MessageRef {
		for _, ev := range evs {
			if ev.Kind == "reply" {
				return airut.MessageRef{ID: ev.Ref}
			}
		}
		t.Fatal("no answer")
		return airut.MessageRef{}
	}

	e.model.play(say("one"), say("two"), say("three"), say("four"), say("five"), say("six"))
	first := answer(a.Text("!ask"))
	if n := seen(); n != 1 {
		t.Fatalf("the first turn saw %d messages", n)
	}
	c.advance(10 * time.Minute)
	a.Text("!ask")
	if n := seen(); n != 3 {
		t.Errorf("a turn within FollowWithin saw %d messages, want the conversation so far and its prompt", n)
	}
	c.advance(airut.DefaultFollowWithin + time.Minute)
	// A new conversation is named by the real time it began.
	time.Sleep(2 * time.Millisecond)
	a.Text("!ask")
	if n := seen(); n != 1 {
		t.Errorf("a turn after a gap saw %d messages, want a new conversation", n)
	}
	a.Text("!ask", airuttest.ReplyingTo(first))
	if n := seen(); n != 5 {
		t.Errorf("a reply to the first answer saw %d messages, want the first conversation", n)
	}
	a.Text("!ask", airuttest.ReplyingTo(first), airuttest.As(airut.User{Platform: airuttest.Platform, ID: "user-2"}))
	if n := seen(); n != 1 {
		t.Errorf("someone else's reply saw %d messages, want their own new conversation", n)
	}

	// Forgetting forgets the links too.
	a.Text("!forget")
	a.Text("!ask", airuttest.ReplyingTo(first))
	if n := seen(); n != 1 {
		t.Errorf("after forget a reply to the first answer saw %d messages", n)
	}
	if _, ok, err := e.agent.LinkedSession(context.Background(), first, airut.Channel{Platform: airuttest.Platform, ID: "channel-1"}); ok || err != nil {
		t.Errorf("the forgotten answer is still linked: %v, %v", ok, err)
	}
}

func TestFollowSessionTakesTheDefault(t *testing.T) {
	c := &clock{t: time.Now()}
	db, err := sessions.Open(context.Background(), filepath.Join(t.TempDir(), "sessions.db"), sessions.Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := newEnv(t, db, nil, airut.Config{})
	e.agent.now = c.now
	ctx := context.Background()
	if _, err := db.Session(ctx, "p/1"); err != nil {
		t.Fatal(err)
	}
	if err := e.agent.FollowSession(ctx, "p", "p/1"); err != nil {
		t.Fatal(err)
	}
	c.advance(5 * time.Minute)
	for _, x := range []struct {
		def, within time.Duration
		want        bool
	}{
		{0, 0, true}, // DefaultFollowWithin
		{time.Minute, 0, false},
		{time.Minute, time.Hour, true},
		{time.Hour, time.Minute, false},
	} {
		e.agent.history.FollowWithin = x.def
		if _, ok, err := e.agent.FollowedSession(ctx, "p", x.within); ok != x.want || err != nil {
			t.Errorf("default %v, within %v: followed %v, %v", x.def, x.within, ok, err)
		}
	}
}
