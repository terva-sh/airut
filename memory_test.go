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

// keeper is a scripted agent that keeps memories in a map, with none of
// a real store's deduplication or caps.
type keeper struct {
	airuttest.Agent
	mu   sync.Mutex
	next int64
	ms   map[int64]airut.Memory
	// read, if set, runs after Memory reads one, as another writer could.
	read func(*keeper, int64)
}

func (k *keeper) SaveMemory(_ context.Context, m airut.Memory) (airut.Memory, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ms == nil {
		k.ms = map[int64]airut.Memory{}
	}
	k.next++
	m.ID, m.Created = k.next, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	k.ms[m.ID] = m
	return m, nil
}

func (k *keeper) Memories(_ context.Context, q airut.MemoryQuery) ([]airut.Memory, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []airut.Memory
	for id := int64(1); id <= k.next; id++ {
		m, ok := k.ms[id]
		in := m.Scope == airut.UserMemory && m.ScopeKey == q.User && q.User != "" ||
			m.Scope == airut.ChannelMemory && m.ScopeKey == q.Channel && q.Channel != "" ||
			m.Scope == airut.BotMemory && q.Bot
		if ok && in && (len(q.Statuses) == 0 || slices.Contains(q.Statuses, m.Status)) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (k *keeper) Memory(_ context.Context, id int64) (airut.Memory, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, ok := k.ms[id]
	if !ok {
		return airut.Memory{}, airut.ErrNoMemory
	}
	if k.read != nil {
		k.mu.Unlock()
		k.read(k, id)
		k.mu.Lock()
	}
	return m, nil
}

func (k *keeper) SetMemoryStatus(_ context.Context, id int64, s airut.MemoryStatus) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, ok := k.ms[id]
	if !ok {
		return airut.ErrNoMemory
	}
	m.Status = s
	k.ms[id] = m
	return nil
}

func (k *keeper) DeleteMemory(_ context.Context, m airut.Memory) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ms[m.ID] != m {
		return false, nil
	}
	delete(k.ms, m.ID)
	return true, nil
}

func (k *keeper) get(id int64) (airut.Memory, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	m, ok := k.ms[id]
	return m, ok
}

var (
	tester     = airut.User{Platform: "test", ID: "user-1", Name: "tester"}
	approver   = airut.User{Platform: "test", ID: "mod", Name: "Mod"}
	memoryGate = airut.Gate{Access: map[airut.Platform]airut.Access{"test": {Users: []string{"user-1", "bob", "mod"}}}}
)

// memoryBot runs a bot whose agent keeps memories in k, with "approver" as
// its only approver, and an "ask" command that starts a turn with the
// remember tool.
func memoryBot(t *testing.T, k *keeper, cfg airut.Config) (*airuttest.Adapter, *lockedBuffer) {
	t.Helper()
	cfg.Agent, cfg.Gate = k, memoryGate
	cfg.MemoryApprover = func(c *airut.Ctx) bool { return c.Caller().ID == "mod" }
	b, logs := newBot(cfg)
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "hi", Tools: b.Tools("remember")}).StreamTo(c)
	})
	return airuttest.Start(t, b), logs
}

func lastText(evs []airuttest.Event) string {
	if len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].Text
}

func TestRememberByHand(t *testing.T) {
	k := &keeper{}
	var pending []airut.Memory
	a, logs := memoryBot(t, k, airut.Config{OnPendingMemory: func(_ *airut.Ctx, m airut.Memory) { pending = append(pending, m) }})
	for _, c := range []struct {
		line  string
		as    airut.User
		reply string
		want  airut.Memory
	}{
		{"!remember I prefer metric secret-one", tester, "Remembered (memory 1).", airut.Memory{Scope: airut.UserMemory, ScopeKey: "test/user/user-1",
			Text: "I prefer metric secret-one", Status: airut.MemoryActive}},
		{"!remember --channel key=wifi the guest wifi is secret-two", tester, "Remembered (memory 2).", airut.Memory{Scope: airut.ChannelMemory,
			ScopeKey: "test/channel/channel-1", Key: "wifi", Text: "the guest wifi is secret-two", Status: airut.MemoryActive}},
		{"!remember --bot tags are lowercase", tester, "Saved for approval (memory 3).", airut.Memory{Scope: airut.BotMemory,
			Text: "tags are lowercase", Status: airut.MemoryPending}},
		{"!remember --bot tags are lowercase", approver, "Remembered (memory 4).", airut.Memory{Scope: airut.BotMemory,
			Text: "tags are lowercase", Status: airut.MemoryActive}},
		{"!remember about=@bob Bob likes tea", tester, "Saved until they agree to it (memory 5).", airut.Memory{Scope: airut.UserMemory,
			ScopeKey: "test/user/bob", Text: "Bob likes tea", Status: airut.MemoryPending}},
		{"!remember about=@user-1 I like tea", tester, "Remembered (memory 6).", airut.Memory{Scope: airut.UserMemory,
			ScopeKey: "test/user/user-1", Text: "I like tea", Status: airut.MemoryActive}},
	} {
		if got := lastText(a.Text(c.line, airuttest.As(c.as))); got != c.reply {
			t.Errorf("%s: replied %q, want %q", c.line, got, c.reply)
		}
		m, _ := k.get(k.next)
		c.want.ID, c.want.Source, c.want.Author, c.want.Created = k.next, airut.FromUser, c.as, m.Created
		if m != c.want {
			t.Errorf("%s: saved %+v, want %+v", c.line, m, c.want)
		}
	}
	if len(pending) != 1 || pending[0].ID != 3 {
		t.Errorf("OnPendingMemory saw %+v", pending)
	}
	for _, line := range []string{
		"!remember", "!remember --channel --bot both", "!remember --bot about=@bob both",
		"!remember " + strings.Repeat("x", airut.MaxMemoryText+1), "!remember key=" + strings.Repeat("k", airut.MaxMemoryKey+1) + " text",
	} {
		if evs := a.Text(line); len(evs) != 1 || strings.HasPrefix(evs[0].Text, "Remembered") || strings.HasPrefix(evs[0].Text, "Saved") {
			t.Errorf("%.40s: %+v", line, evs)
		}
	}
	if k.next != 6 {
		t.Errorf("%d memories were saved, want 6", k.next)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Errorf("memory text reached the log:\n%s", logs)
	}
}

func TestRememberByTheModel(t *testing.T) {
	for _, c := range []struct {
		name   string
		writes map[airut.MemoryScope]airut.MemoryWrite
		args   string
		answer string // to the question, if one is asked
		asked  bool
		saved  airut.MemoryStatus // "" for nothing saved
	}{
		{"about the caller, asked and agreed", nil, `{"text": "likes metric"}`, "Remember", true, airut.MemoryActive},
		{"about the caller, asked and refused", nil, `{"text": "likes metric"}`, "Don't", true, ""},
		{"about the caller, without asking", map[airut.MemoryScope]airut.MemoryWrite{airut.UserMemory: airut.WriteAuto}, `{"text": "likes metric"}`, "", false, airut.MemoryActive},
		{"about the caller, off", map[airut.MemoryScope]airut.MemoryWrite{airut.UserMemory: airut.WriteOff}, `{"text": "likes metric"}`, "", false, ""},
		{"about someone else, waiting for them", nil, `{"text": "likes tea", "about": "@bob"}`, "", false, airut.MemoryPending},
		{"about the channel, by default", nil, `{"text": "standup is at nine", "channel": true}`, "", false, airut.MemoryActive},
		{"about the bot, off by default", nil, `{"text": "be terse", "bot": true}`, "", false, ""},
		{"about the bot, from someone who is not an approver", map[airut.MemoryScope]airut.MemoryWrite{airut.BotMemory: airut.WriteAuto},
			`{"text": "be terse", "bot": true}`, "", false, airut.MemoryPending},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := &keeper{Agent: airuttest.Agent{Steps: []airuttest.Step{{Call: "remember", Args: c.args}}}}
			a, _ := memoryBot(t, k, airut.Config{MemoryWrites: c.writes})
			var asked []airut.Ask
			a.OnAsk(func(q airut.Ask) (string, error) { asked = append(asked, q); return c.answer, nil })
			a.Text("!ask")
			if (len(asked) > 0) != c.asked || c.asked && asked[0].Who != tester {
				t.Errorf("asked %+v", asked)
			}
			m, ok := k.get(1)
			if ok != (c.saved != "") || ok && (m.Status != c.saved || m.Source != airut.FromModel) {
				t.Errorf("saved %+v, %v", m, ok)
			}
		})
	}
}

func TestPendingMemoriesAskThePersonTheyAreAbout(t *testing.T) {
	k := &keeper{}
	a, _ := memoryBot(t, k, airut.Config{})
	bob := airut.User{Platform: "test", ID: "bob", Name: "bob"}
	a.Text("!remember about=@bob Bob likes tea")
	a.Text("!remember about=@bob Bob hates mornings")
	a.Text("!remember about=@bob Bob plays chess")
	var asked []airut.Ask
	answers := []string{"Remember", "Don't"}
	a.OnAsk(func(q airut.Ask) (string, error) {
		asked = append(asked, q)
		ans := answers[0]
		answers = answers[1:]
		return ans, nil
	})
	// Someone else's turn asks nobody about Bob's.
	a.Text("!ask")
	if len(asked) != 0 {
		t.Fatalf("asked %+v", asked)
	}
	// Bob's turn asks him about two, and the third waits.
	a.Text("!ask", airuttest.As(bob))
	if len(asked) != 2 || asked[0].Who != bob || !strings.Contains(asked[0].Question, "tester asked me to remember") {
		t.Fatalf("asked %+v", asked)
	}
	if m, _ := k.get(1); m.Status != airut.MemoryActive {
		t.Errorf("the first is %s", m.Status)
	}
	if _, ok := k.get(2); ok {
		t.Error("the second was kept")
	}
	// memories asks about the rest, then lists.
	answers = []string{"Remember"}
	evs := a.Text("!memories", airuttest.As(bob))
	if len(asked) != 3 {
		t.Errorf("memories asked %d in all", len(asked))
	}
	got := lastText(evs)
	if !evs[len(evs)-1].Private || !strings.Contains(got, "1. Bob likes tea (noted by tester on 2026-10-21)") ||
		!strings.Contains(got, "3. Bob plays chess") || strings.Contains(got, "mornings") {
		t.Errorf("memories listed %q", got)
	}
}

func TestMemoriesLists(t *testing.T) {
	k := &keeper{}
	a, _ := memoryBot(t, k, airut.Config{})
	if got := lastText(a.Text("!memories")); !strings.Contains(got, "I remember nothing") {
		t.Errorf("an empty list: %q", got)
	}
	a.Text("!remember mine")
	a.Text("!remember --channel ours")
	a.Text("!remember --bot proposed")
	a.Text("!remember --channel elsewhere", airuttest.In(airut.Channel{Platform: "test", ID: "channel-2"}))
	a.Text("!remember theirs", airuttest.As(airut.User{Platform: "test", ID: "bob"}))
	a.Text("!remember old news")
	_ = k.SetMemoryStatus(context.Background(), 6, airut.MemoryArchived)
	want := "About you:\n1. mine (noted by tester on 2026-10-21)\n\nAbout this channel:\n2. ours (noted by tester on 2026-10-21)\n\n" +
		"For everyone:\n3. proposed (noted by tester on 2026-10-21, waiting for approval)\nRemove one with: unremember ID"
	if got := lastText(a.Text("!memories")); got != want {
		t.Errorf("memories listed\n%s\nwant\n%s", got, want)
	}
}

func TestUnremember(t *testing.T) {
	k := &keeper{}
	a, _ := memoryBot(t, k, airut.Config{})
	bob := airut.User{Platform: "test", ID: "bob", Name: "bob"}
	a.Text("!remember mine")                                       // 1
	a.Text("!remember theirs", airuttest.As(bob))                  // 2
	a.Text("!remember --channel ours")                             // 3
	a.Text("!remember --channel bobs", airuttest.As(bob))          // 4
	a.Text("!remember --bot proposed")                             // 5, pending
	a.Text("!remember --bot approved", airuttest.As(approver))     // 6
	a.Text("!remember about=@bob Bob likes tea")                   // 7, pending
	a.Text("!remember --bot also proposed", airuttest.As(bob))     // 8, pending
	a.Text("!remember about=@bob Bob reads", airuttest.As(tester)) // 9, then agreed to
	_ = k.SetMemoryStatus(context.Background(), 9, airut.MemoryActive)
	a.Text("!remember --bot then approved") // 10, then approved
	_ = k.SetMemoryStatus(context.Background(), 10, airut.MemoryActive)
	for _, c := range []struct {
		id      string
		as      airut.User
		removed bool
	}{
		{"2", tester, false},  // someone else's own
		{"4", tester, false},  // a channel memory someone else wrote
		{"6", tester, false},  // an approved bot memory
		{"8", tester, false},  // someone else's proposal
		{"99", tester, false}, // none at all
		{"9", tester, false},  // a note about someone that they agreed to
		{"10", tester, false}, // one's own proposal, once approved
		{"1", tester, true},
		{"3", tester, true},
		{"5", tester, true}, // one's own proposal
		{"7", tester, true}, // one's own pending note about someone
		{"4", approver, true},
		{"6", approver, true},
		{"2", bob, true},
	} {
		evs := a.Text("!unremember "+c.id, airuttest.As(c.as))
		id := c.id
		var n int64
		for _, r := range id {
			n = n*10 + int64(r-'0')
		}
		_, kept := k.get(n)
		if kept == c.removed && n != 99 || c.removed != strings.HasPrefix(lastText(evs), "Forgot memory") {
			t.Errorf("%s removing %s: %q, kept %v", c.as.ID, c.id, lastText(evs), kept)
		}
	}
}

func TestMemoryWritesAreChecked(t *testing.T) {
	for _, w := range []map[airut.MemoryScope]airut.MemoryWrite{
		{"room": airut.WriteAuto}, {airut.UserMemory: "always"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New accepted MemoryWrites %v", w)
				}
			}()
			airut.New(airut.Config{MemoryWrites: w})
		}()
	}
}

func kinds(evs []airuttest.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestTheConsentQuestionComesAfterACompleteAnswer(t *testing.T) {
	k := &keeper{Agent: airuttest.Agent{Steps: []airuttest.Step{{Text: "Hello"}, {Text: " there"}}}}
	a, _ := memoryBot(t, k, airut.Config{TypingDelay: -1, TypingEvery: time.Millisecond, EditInterval: time.Hour})
	a.Text("!remember about=@bob likes tea")
	a.OnAsk(func(airut.Ask) (string, error) {
		// Long enough for a keepalive still running to renew typing.
		time.Sleep(50 * time.Millisecond)
		return "Don't", nil
	})
	evs := a.Text("!ask", airuttest.As(airut.User{Platform: "test", ID: "bob"}))
	// The answer is whole before the question, and typing has stopped.
	got := kinds(evs)
	ask := slices.Index(got, "ask")
	if ask < 0 || slices.Index(got, "edit") > ask {
		t.Fatalf("events %v", got)
	}
	if e := evs[slices.Index(got, "edit")]; e.Text != "Hello there" {
		t.Errorf("the answer ended as %q", e.Text)
	}
	if n := slices.Index(got[ask:], "typing"); n >= 0 {
		t.Errorf("typing went on while the question waited: %v", got)
	}
}

func TestATurnThatFailedOrNobodyAskedForAsksNobody(t *testing.T) {
	k := &keeper{Agent: airuttest.Agent{Err: errors.New("model down")}}
	a, _ := memoryBot(t, k, airut.Config{})
	a.Text("!remember about=@user-1 likes tea", airuttest.As(airut.User{Platform: "test", ID: "bob"}))
	asked := 0
	a.OnAsk(func(airut.Ask) (string, error) { asked++; return "Don't", nil })
	a.Text("!ask")
	if asked != 0 {
		t.Errorf("a failed turn asked %d times", asked)
	}

	// A Chime turn is the channel's: the person it answers asked for
	// nothing, so nobody is asked.
	k = &keeper{}
	ran := make(chan struct{}, 1)
	judge := judgeFunc(func(_ *airut.Ctx, j airut.Judgement) (airut.Verdict, error) {
		return airut.Verdict{Decision: airut.Yes, Target: j.Batch[0].Ref}, nil
	})
	cfg := airut.Config{Agent: k, Judge: judge, Gate: airut.Gate{Access: map[airut.Platform]airut.Access{"test": {Channels: []string{"channel-1"}}}}}
	b, _ := newBot(cfg)
	b.Chime(airut.ChimeOptions{Channels: []airut.Channel{general}, MaxPerHour: 5, Settle: 10 * time.Millisecond,
		Handler: func(c *airut.Ctx, _ airut.Chimed) error {
			defer func() { ran <- struct{}{} }()
			_, err := c.Agent(airut.Turn{}).Text()
			return err
		}})
	a = airuttest.Start(t, b)
	a.Text("!remember about=@user-1 likes coffee", airuttest.As(airut.User{Platform: "test", ID: "bob"}))
	asked = 0
	a.OnAsk(func(airut.Ask) (string, error) { asked++; return "Don't", nil })
	a.Text("anyone around?")
	<-ran
	if asked != 0 {
		t.Errorf("a Chime turn asked %d times", asked)
	}
}

// A memory that changes between the check and the removal, as when
// someone replaces it under its key, is left alone.
func TestUnrememberChecksWhatItRemoves(t *testing.T) {
	k := &keeper{}
	a, _ := memoryBot(t, k, airut.Config{})
	a.Text("!remember --channel key=wifi the wifi is open")
	k.read = func(k *keeper, id int64) {
		k.mu.Lock()
		defer k.mu.Unlock()
		m := k.ms[id]
		m.Text, m.Author = "the wifi is closed", airut.User{Platform: "test", ID: "bob", Name: "bob"}
		k.ms[id] = m
	}
	if got := lastText(a.Text("!unremember 1")); got != "There is no memory 1 you can remove." {
		t.Errorf("unremember answered %q", got)
	}
	if m, ok := k.get(1); !ok || m.Text != "the wifi is closed" {
		t.Errorf("the replacement is %+v, %v", m, ok)
	}
}

func TestMemoryAccess(t *testing.T) {
	onlyMod := func(c *airut.Ctx) (bool, error) { return c.Caller().ID == "mod", nil }
	k := &keeper{}
	a, _ := memoryBot(t, k, airut.Config{MemoryAccess: onlyMod})
	for _, line := range []string{"!remember I like tea", "!memories", "!unremember 1"} {
		name := strings.Fields(line)[0]
		if got := lastText(a.Text(line, airuttest.As(tester))); got != "You are not allowed to use "+name+"." {
			t.Errorf("%s by someone MemoryAccess refuses: %q", line, got)
		}
	}
	if got := lastText(a.Text("!remember --channel standup is at nine", airuttest.As(approver))); got != "Remembered (memory 1)." {
		t.Errorf("remember by someone MemoryAccess allows: %q", got)
	}
	if got := lastText(a.Text("!unremember 1", airuttest.As(approver))); got != "Forgot memory 1." {
		t.Errorf("unremember by someone MemoryAccess allows: %q", got)
	}
}

func TestMemoryAccessCoversTheTool(t *testing.T) {
	onlyMod := func(c *airut.Ctx) (bool, error) { return c.Caller().ID == "mod", nil }
	for _, c := range []struct {
		as     airut.User
		result string
		saved  int64
	}{
		// The tool is left out of a refused caller's turn, so the model is
		// never offered it.
		{tester, "refused: the turn does not name tool remember", 0},
		{approver, "Remembered (memory 1).", 1},
	} {
		k := &keeper{Agent: airuttest.Agent{Steps: []airuttest.Step{{Call: "remember", Args: `{"text": "standup is at nine", "channel": true}`}}}}
		a, _ := memoryBot(t, k, airut.Config{MemoryAccess: onlyMod})
		a.Text("!ask", airuttest.As(c.as))
		if res := k.Results(); len(res) != 1 || res[0] != c.result {
			t.Errorf("a turn of %s: tool results %q, want %q", c.as.ID, res, c.result)
		}
		if k.next != c.saved {
			t.Errorf("a turn of %s saved %d memories, want %d", c.as.ID, k.next, c.saved)
		}
	}
}
