package airut_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// linker is an agent that answers "ok" and links in memory. It records
// the session key each turn ran in, as the agent package reads it.
type linker struct {
	mu      sync.Mutex
	links   map[string]string // channel id + "/" + message id -> key
	follows map[string]follow // person -> what they last spoke in
	keys    []string
	within  []time.Duration
	err     error
}

type follow struct {
	key string
	at  time.Time
}

func newLinker() *linker {
	return &linker{links: map[string]string{}, follows: map[string]follow{}}
}

func (l *linker) Run(c *airut.Ctx, t airut.Turn, out airut.TurnOutput) error {
	key := c.SessionKey(t.Session)
	l.mu.Lock()
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	out.Text("ok")
	return nil
}

func (l *linker) LinkSession(_ context.Context, ref airut.MessageRef, ch airut.Channel, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.links[ch.ID+"/"+ref.ID] = key
	return nil
}

func (l *linker) LinkedSession(_ context.Context, ref airut.MessageRef, ch airut.Channel) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key, ok := l.links[ch.ID+"/"+ref.ID]
	return key, ok, l.err
}

func (l *linker) FollowSession(_ context.Context, person, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.follows[person] = follow{key, time.Now()}
	return nil
}

func (l *linker) FollowedSession(_ context.Context, person string, within time.Duration) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.within = append(l.within, within)
	if within == 0 {
		within = airut.DefaultFollowWithin
	}
	f, ok := l.follows[person]
	if !ok || time.Since(f.at) > within {
		return "", false, l.err
	}
	return f.key, true, l.err
}

// age makes everything person last spoke in older than FollowWithin.
func (l *linker) age(person string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.follows[person]
	f.at = time.Now().Add(-airut.DefaultFollowWithin - time.Minute)
	l.follows[person] = f
}

func (l *linker) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.keys[len(l.keys)-1]
}

func (l *linker) link(ch, ref, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.links[ch+"/"+ref] = key
}

func linkerBot(t *testing.T, l *linker, scope airut.SessionScope) *airuttest.Adapter {
	t.Helper()
	b, _ := newBot(airut.Config{Gate: openGate, Agent: l, EditInterval: time.Nanosecond})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "?", Session: scope}).StreamTo(c)
	})
	return airuttest.Start(t, b)
}

const (
	me    = "test/user/user-1"
	here  = "channel-1"
	other = "test/user/dana"
)

func TestAReplyResumesTheSessionThatAnswered(t *testing.T) {
	ref := airut.MessageRef{ID: "bot-msg"}
	for _, c := range []struct {
		name   string
		scope  airut.SessionScope
		linked string
		opts   []airuttest.Option
		want   string
	}{
		{"the channel's conversation, from a PerUser turn", airut.PerUser, "test/channel/" + here, nil, "test/channel/" + here},
		{"a thread of this channel", airut.PerUser, "test/thread/" + here + "/t1", nil, "test/thread/" + here + "/t1"},
		{"the caller's own", airut.PerChannel, me, nil, me},
		{"the caller's own in this channel", airut.PerChannel, me + "/channel/" + here + "/17", nil, me + "/channel/" + here + "/17"},
		{"not another member's", airut.PerUser, other, nil, me},
		{"not another member's in this channel", airut.PerUserChannel, other + "/channel/" + here, nil, me + "/channel/" + here},
		{"not another channel's", airut.PerChannel, "test/channel/elsewhere", nil, "test/channel/" + here},
		{"not a thread of another channel", airut.PerChannel, "test/thread/elsewhere/t1", nil, "test/channel/" + here},
		{"not the caller's own in another channel", airut.PerUser, me + "/channel/dm-1", nil, me},
		{"not another platform's", airut.PerUser, "discord/channel/" + here, nil, me},
		// In a direct channel only that channel's conversation resumes, so
		// nothing said there reaches a group room's.
		{"not the caller's PerUser one in a direct channel", airut.PerUser, me, []airuttest.Option{airuttest.InDirect()}, me + "/channel/" + here},
		{"the direct channel's own", airut.PerUser, me + "/channel/" + here + "/5", []airuttest.Option{airuttest.InDirect()}, me + "/channel/" + here + "/5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLinker()
			a := linkerBot(t, l, c.scope)
			l.link(here, ref.ID, c.linked)
			a.Text("!ask", append([]airuttest.Option{airuttest.ReplyingTo(ref)}, c.opts...)...)
			if got := l.last(); got != c.want {
				t.Errorf("the turn ran in %q, want %q", got, c.want)
			}
		})
	}
	// A turn that remembers nothing looks up no link.
	l := newLinker()
	a := linkerBot(t, l, airut.NoSession)
	l.link(here, ref.ID, me)
	l.err = errors.New("no lookup should happen")
	if evs := a.Text("!ask", airuttest.ReplyingTo(ref)); l.last() != "" || final(evs) != "ok" {
		t.Errorf("a NoSession turn ran in %q and answered %q", l.last(), final(evs))
	}
}

func TestStreamToLinksItsAnswer(t *testing.T) {
	l := newLinker()
	a := linkerBot(t, l, airut.PerChannel)
	evs := a.Text("!ask")
	var ref string
	for _, e := range evs {
		if e.Kind == "reply" {
			ref = e.Ref
		}
	}
	l.mu.Lock()
	got := l.links[here+"/"+ref]
	l.mu.Unlock()
	if ref == "" || got != "test/channel/"+here {
		t.Fatalf("the answer %q is linked to %q", ref, got)
	}
	// A reply to it from a PerUser turn goes on in that conversation.
	b, _ := newBot(airut.Config{Gate: openGate, Agent: l})
	var run *airut.TurnRun
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		run = c.Agent(airut.Turn{Prompt: "?", Session: airut.PerUser})
		text, err := run.Text()
		if err != nil {
			return err
		}
		posted, err := c.Reply(text)
		if err != nil {
			return err
		}
		return run.Link(c, posted)
	})
	a2 := airuttest.Start(t, b)
	evs = a2.Text("!ask", airuttest.ReplyingTo(airut.MessageRef{ID: ref}))
	if l.last() != "test/channel/"+here {
		t.Errorf("the reply ran in %q", l.last())
	}
	// Link links what the app posted to the session the turn ran in.
	for _, e := range evs {
		if e.Kind == "reply" {
			ref = e.Ref
		}
	}
	l.mu.Lock()
	got = l.links[here+"/"+ref]
	l.mu.Unlock()
	if got != "test/channel/"+here {
		t.Errorf("the app's own post is linked to %q", got)
	}
}

func TestConversationFollowsAPerson(t *testing.T) {
	l := newLinker()
	a := linkerBot(t, l, airut.Conversation)
	gen := me + "/channel/" + here + "/"

	// A first message starts a conversation, and the next goes on in it.
	a.Text("!ask")
	first := l.last()
	if !strings.HasPrefix(first, gen) || len(first) == len(gen) {
		t.Fatalf("the first turn ran in %q, want a generation of %q", first, gen)
	}
	// Later than a new generation would be named the same.
	time.Sleep(2 * time.Millisecond)
	a.Text("!ask")
	if l.last() != first {
		t.Errorf("the next turn ran in %q, want %q", l.last(), first)
	}
	// After a longer gap than FollowWithin, a new one starts.
	l.age(me + "/channel/" + here)
	time.Sleep(2 * time.Millisecond) // a new generation's name differs
	a.Text("!ask")
	second := l.last()
	if second == first || !strings.HasPrefix(second, gen) {
		t.Errorf("after a gap the turn ran in %q, want a new generation", second)
	}
	// A reply to an answer of the first goes back to it.
	l.link(here, "old-answer", first)
	a.Text("!ask", airuttest.ReplyingTo(airut.MessageRef{ID: "old-answer"}))
	if l.last() != first {
		t.Errorf("a reply to the first conversation ran in %q", l.last())
	}
	// The next message goes on in the conversation the reply resumed.
	a.Text("!ask")
	if l.last() != first {
		t.Errorf("the message after a reply ran in %q, want %q", l.last(), first)
	}
	// A thread is one conversation for everyone in it, and a person who
	// comes back from it to the channel goes on in it.
	thread := "test/thread/" + here + "/t9"
	a.Text("!ask", airuttest.InThread("t9"))
	if l.last() != thread {
		t.Errorf("a thread turn ran in %q", l.last())
	}
	a.Text("!ask")
	if l.last() != thread {
		t.Errorf("back from the thread the turn ran in %q, want the thread's", l.last())
	}
	// So does one who replied into the channel's shared conversation.
	shared := "test/channel/" + here
	l.link(here, "shared-answer", shared)
	a.Text("!ask", airuttest.ReplyingTo(airut.MessageRef{ID: "shared-answer"}))
	a.Text("!ask")
	if l.last() != shared {
		t.Errorf("after replying into the shared conversation the turn ran in %q", l.last())
	}
	// Another person has their own.
	a.Text("!ask", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "dana"}))
	if !strings.HasPrefix(l.last(), other+"/channel/"+here+"/") {
		t.Errorf("another person's turn ran in %q", l.last())
	}
	// A direct channel is one conversation, in a thread or after a gap,
	// and its turns change nothing a group room follows.
	dm := airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "dm-1"})
	for _, opts := range [][]airuttest.Option{{dm, airuttest.InDirect()}, {dm, airuttest.InDirect(), airuttest.InThread("t9")}} {
		a.Text("!ask", opts...)
		if l.last() != me+"/channel/dm-1" {
			t.Errorf("a direct channel's turn ran in %q", l.last())
		}
	}
	l.mu.Lock()
	_, followed := l.follows[me+"/channel/dm-1"]
	l.mu.Unlock()
	if followed {
		t.Error("a direct channel's turn was recorded to follow")
	}
	// The turn's FollowWithin reaches the linker; zero is the default.
	l.mu.Lock()
	within := l.within
	l.mu.Unlock()
	for _, w := range within {
		if w != 0 {
			t.Errorf("FollowSession got %v, want 0 for the agent's default", w)
		}
	}
}

func TestConversationWithoutALinker(t *testing.T) {
	g := &airuttest.Agent{}
	b, _ := newBot(airut.Config{Gate: openGate, Agent: g})
	var key string
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		key = c.SessionKey(airut.Conversation)
		return c.Agent(airut.Turn{Prompt: "?", Session: airut.Conversation}).StreamTo(c)
	})
	airuttest.Start(t, b).Text("!ask")
	if key != me+"/channel/"+here {
		t.Errorf("Conversation without a linker is %q, want the PerUserChannel key", key)
	}
}

func TestALinkLookupThatFailsFailsTheTurn(t *testing.T) {
	l := newLinker()
	l.err = errors.New("database is locked")
	a := linkerBot(t, l, airut.PerUser)
	evs := a.Text("!ask", airuttest.ReplyingTo(airut.MessageRef{ID: "x"}))
	l.mu.Lock()
	ran := len(l.keys)
	l.mu.Unlock()
	if ran != 0 || strings.Contains(final(evs), "locked") || final(evs) == "" {
		t.Errorf("a failed lookup ran %d turns and answered %q", ran, final(evs))
	}
}

// final is the text of the bot's last reply or edit.
func final(evs []airuttest.Event) string {
	var last string
	for _, e := range evs {
		if e.Kind == "reply" || e.Kind == "edit" {
			last = e.Text
		}
	}
	return last
}
