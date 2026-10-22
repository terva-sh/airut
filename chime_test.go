package airut_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// scripted is a Judge that hands each judgement to the test and answers
// with what the test sends back.
type scripted struct {
	asked   chan airut.Judgement
	answers chan airut.Verdict
	mu      sync.Mutex
	running int
	most    int
}

func newScripted() *scripted {
	return &scripted{asked: make(chan airut.Judgement, 16), answers: make(chan airut.Verdict, 16)}
}

func (s *scripted) Judge(c *airut.Ctx, j airut.Judgement) (airut.Verdict, error) {
	s.mu.Lock()
	s.running++
	s.most = max(s.most, s.running)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}()
	s.asked <- j
	select {
	case v := <-s.answers:
		return v, nil
	case <-c.Context().Done():
		return airut.Verdict{}, c.Context().Err()
	}
}

// next waits for the next judgement.
func (s *scripted) next(t *testing.T) airut.Judgement {
	t.Helper()
	select {
	case j := <-s.asked:
		return j
	case <-time.After(5 * time.Second):
		t.Fatal("no judgement was asked for")
	}
	return airut.Judgement{}
}

// none checks that no judgement is asked for within d.
func (s *scripted) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case j := <-s.asked:
		t.Fatalf("an unexpected judgement of %d messages", len(j.Batch))
	case <-time.After(d):
	}
}

func no() airut.Verdict { return airut.Verdict{Decision: airut.No} }

func yes(q airut.Quoted) airut.Verdict {
	return airut.Verdict{Decision: airut.Yes, Target: q.Ref, Reason: "secret reason"}
}

func quoted(qs []airut.Quoted) []string {
	var out []string
	for _, q := range qs {
		out = append(out, q.Text)
	}
	return out
}

var (
	general = airut.Channel{Platform: "test", ID: "channel-1"}
	alice   = airut.User{Platform: "test", ID: "alice", Name: "Alice"}
	bob     = airut.User{Platform: "test", ID: "bob", Name: "Bob"}
)

// chimed is what a test's Chime handler saw.
type chimed struct {
	c   *airut.Ctx
	e   airut.Chimed
	err error
}

// startChime runs a bot with one Chime listener on general, and returns
// its adapter and what reaches the handler. o's Handler, if set, runs
// before the run is reported.
func startChime(t *testing.T, cfg airut.Config, o airut.ChimeOptions) (*airuttest.Adapter, *scripted, chan chimed, *airut.Bot, *lockedBuffer) {
	t.Helper()
	return startChimeWith(t, cfg, o, func(j *scripted) airut.Judge { return j })
}

// startChimeWith is startChime with the scripted judge wrapped.
func startChimeWith(t *testing.T, cfg airut.Config, o airut.ChimeOptions, wrap func(*scripted) airut.Judge) (*airuttest.Adapter, *scripted, chan chimed, *airut.Bot, *lockedBuffer) {
	t.Helper()
	j := newScripted()
	cfg.Judge = wrap(j)
	b, logs := newBot(cfg)
	ran := make(chan chimed, 16)
	h := o.Handler
	o.Handler = func(c *airut.Ctx, e airut.Chimed) error {
		var err error
		if h != nil {
			err = h(c, e)
		}
		ran <- chimed{c, e, err}
		return err
	}
	if o.Channels == nil {
		o.Channels = []airut.Channel{general}
	}
	if o.MaxPerHour == 0 {
		o.MaxPerHour = 10
	}
	if o.Settle == 0 {
		o.Settle = 50 * time.Millisecond
	}
	if o.Persona == "" {
		o.Persona = "chime"
	}
	b.Chime(o)
	return airuttest.Start(t, b), j, ran, b, logs
}

func waitRan(t *testing.T, ran chan chimed) chimed {
	t.Helper()
	select {
	case r := <-ran:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not run")
	}
	return chimed{}
}

func noRun(t *testing.T, ran chan chimed) {
	t.Helper()
	select {
	case <-ran:
		t.Fatal("the handler ran")
	case <-time.After(100 * time.Millisecond):
	}
}

// outcome is how often the round outcome o has been counted, engaged
// or not.
func outcome(b *airut.Bot, o string) float64 {
	n := 0.0
	for _, e := range []string{"false", "true"} {
		s, _ := b.Metrics().Get("airut_chime_total", "persona", "chime", "outcome", o, "engaged", e)
		n += s.Value
	}
	return n
}

// waitOutcome waits until the round outcome o has been counted n times.
func waitOutcome(t *testing.T, b *airut.Bot, o string, n float64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); outcome(b, o) < n; {
		if time.Now().After(deadline) {
			t.Fatalf("outcome %s counted %v times, want %v", o, outcome(b, o), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestChimeJudgesABurstOnce(t *testing.T) {
	// Each message extends the batch by Settle, so it outlasts the first
	// Settle after the first message.
	a, j, _, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{Settle: 250 * time.Millisecond, Prompt: "Should you speak?"})
	for _, line := range []string{"one", "two", "three", "four", "five"} {
		a.Text(line, airuttest.As(alice))
		time.Sleep(100 * time.Millisecond)
	}
	got := j.next(t)
	if !slices.Equal(quoted(got.Batch), []string{"one", "two", "three", "four", "five"}) || len(got.Context) != 0 {
		t.Errorf("batch %q after context %q", quoted(got.Batch), quoted(got.Context))
	}
	if got.Persona != "chime" || got.Prompt != "Should you speak?" {
		t.Errorf("persona %q, prompt %q", got.Persona, got.Prompt)
	}
	j.answers <- no()
	j.none(t, 400*time.Millisecond)

	// A long message is cut, as a quote in a reply chain is.
	a.Text(strings.Repeat("é", airut.MaxQuote), airuttest.As(alice))
	got = j.next(t)
	if q := got.Batch[0].Text; len(q) > airut.MaxQuote || !strings.HasSuffix(q, "…") || !utf8.ValidString(q) {
		t.Errorf("a long message was quoted as %d bytes", len(q))
	}
	j.answers <- no()
}

func TestChimeWaitsNoLongerThanMaxWait(t *testing.T) {
	a, j, _, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{Settle: time.Hour, MaxWait: 100 * time.Millisecond})
	start := time.Now()
	a.Text("hello", airuttest.As(alice))
	j.next(t)
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("judged after %v, before MaxWait", d)
	}
	j.answers <- no()
}

func TestChimeJudgesOneBatchAtATimePerChannel(t *testing.T) {
	a, j, _, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{})
	a.Text("first", airuttest.As(alice))
	j.next(t)
	// While the first judgement waits, the next messages queue.
	a.Text("second", airuttest.As(bob))
	a.Text("third", airuttest.As(alice))
	j.none(t, 200*time.Millisecond)
	j.answers <- no()
	got := j.next(t)
	if !slices.Equal(quoted(got.Batch), []string{"second", "third"}) || !slices.Equal(quoted(got.Context), []string{"first"}) {
		t.Errorf("batch %q after context %q", quoted(got.Batch), quoted(got.Context))
	}
	j.answers <- no()
	// Another channel is judged on its own, beside a busy one.
	other := airut.Channel{Platform: "test", ID: "channel-2"}
	a2, j2, _, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{Channels: []airut.Channel{general, other}})
	a2.Text("here", airuttest.As(alice))
	j2.next(t)
	a2.Text("there", airuttest.As(alice), airuttest.In(other))
	if got := j2.next(t); !slices.Equal(quoted(got.Batch), []string{"there"}) {
		t.Errorf("the other channel's batch is %q", quoted(got.Batch))
	}
	j2.answers <- no()
	j2.answers <- no()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.most != 1 {
		t.Errorf("%d judgements ran at once in one channel", j.most)
	}
}

func TestChimeJudgesOnlyWhatItShould(t *testing.T) {
	// The bot has an owner, so the direct message, which the fake adapter
	// posts in the watched channel, is not answered with the claim line.
	a, j, ran, b, _ := startChime(t, airut.Config{Owners: map[airut.Platform][]string{"test": {"owner"}}}, airut.ChimeOptions{})
	a.Text("!roll 1d6", airuttest.As(alice))
	a.Text("!nosuch", airuttest.As(alice))
	a.Text("missed", airuttest.As(alice), airuttest.SentAt(time.Now().Add(-time.Hour)))
	a.DirectMessage("just us", airuttest.As(alice))
	a.Text("elsewhere", airuttest.As(alice), airuttest.In(airut.Channel{Platform: "test", ID: "channel-9"}))
	a.Mention("what is this?", airuttest.As(alice))
	j.none(t, 300*time.Millisecond)

	// A message that addresses the bot is read, but never chosen.
	a.Mention("and that?", airuttest.As(alice))
	a.Text("and this", airuttest.As(bob))
	got := j.next(t)
	// The command's reply is the bot's, and read as such.
	if !slices.Equal(quoted(got.Context), []string{"rolled 1d6 x1", airuttest.BotName + ": what is this?"}) || !got.Context[0].FromBot ||
		!slices.Equal(quoted(got.Batch), []string{airuttest.BotName + ": and that?", "and this"}) {
		t.Fatalf("batch %q after %q", quoted(got.Batch), quoted(got.Context))
	}
	j.answers <- yes(got.Batch[0])
	waitOutcome(t, b, "not_judged", 1)
	noRun(t, ran)
}

func TestChimeRunsItsHandlerOnAYes(t *testing.T) {
	a, j, ran, b, logs := startChime(t, airut.Config{}, airut.ChimeOptions{Handler: func(c *airut.Ctx, e airut.Chimed) error {
		_, err := c.Reply("I know this one")
		return err
	}})
	a.Text("secret question one", airuttest.As(alice))
	a.Text("secret question two", airuttest.As(bob))
	got := j.next(t)
	j.answers <- yes(got.Batch[0])
	r := waitRan(t, ran)
	if r.e.Target.Text != "secret question one" || r.e.Reason != "secret reason" || r.c.Caller() != alice || r.c.Message() != got.Batch[0].Ref {
		t.Errorf("the handler saw %+v as %v, on %v", r.e, r.c.Caller(), r.c.Message())
	}
	waitOutcome(t, b, "spoke", 1)
	if evs := a.Events(); len(evs) != 1 || evs[0].Kind != "reply" || evs[0].Text != "I know this one" {
		t.Errorf("events %+v", evs)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Errorf("chat or a reason reached the log:\n%s", logs)
	}
}

func TestChimeDropsAYesTheRoomOvertook(t *testing.T) {
	for _, c := range []struct {
		name  string
		after func(a *airuttest.Adapter, target airut.Quoted)
		stale bool
	}{
		{"someone else spoke", func(a *airuttest.Adapter, _ airut.Quoted) { a.Text("I can help", airuttest.As(bob)) }, true},
		{"someone replied to the target", func(a *airuttest.Adapter, q airut.Quoted) {
			a.Text("ah, so", airuttest.As(alice), airuttest.ReplyingTo(q.Ref))
		}, true},
		{"the sender went on", func(a *airuttest.Adapter, _ airut.Quoted) { a.Text("anyone?", airuttest.As(alice)) }, false},
		{"someone else answered another message", func(a *airuttest.Adapter, _ airut.Quoted) {
			a.Text("yes", airuttest.As(bob), airuttest.ReplyingTo(airut.MessageRef{ID: "older"}))
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, j, ran, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{})
			a.Text("how do I do this?", airuttest.As(alice))
			got := j.next(t)
			c.after(a, got.Batch[0])
			j.answers <- yes(got.Batch[0])
			if c.stale {
				waitOutcome(t, b, "stale", 1)
				noRun(t, ran)
			} else {
				waitRan(t, ran)
			}
			j.next(t)
			j.answers <- no()
		})
	}
}

func TestChimeStaysOut(t *testing.T) {
	a, j, ran, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{StayOut: true})
	a.Text("what do you think?", airuttest.As(alice))
	got := j.next(t)
	j.answers <- no()
	// A reply to another person is read but never judged on its own.
	a.Text("I think so", airuttest.As(bob), airuttest.ReplyingTo(got.Batch[0].Ref))
	j.none(t, 300*time.Millisecond)

	// A reply to a message the listener never saw is read to see whose it
	// was, and one it cannot read counts as a person's.
	a.AddHistory("channel-1", airut.MessageEvent{Message: airut.MessageRef{ID: "bots"}, FromBot: true})
	a.Text("thanks", airuttest.As(bob), airuttest.ReplyingTo(airut.MessageRef{ID: "gone"}))
	got = j.next(t)
	j.answers <- yes(got.Batch[len(got.Batch)-1])
	waitOutcome(t, b, "stayed_out", 1)
	noRun(t, ran)
	a.Text("thanks to you", airuttest.As(bob), airuttest.ReplyingTo(airut.MessageRef{ID: "bots"}))
	got = j.next(t)
	j.answers <- yes(got.Batch[len(got.Batch)-1])
	waitRan(t, ran)

	// Someone who replies to themselves is in no exchange.
	a, j, ran, _, _ = startChime(t, airut.Config{}, airut.ChimeOptions{StayOut: true})
	a.Text("does anyone know?", airuttest.As(alice))
	got = j.next(t)
	j.answers <- no()
	a.Text("I mean the build", airuttest.As(alice), airuttest.ReplyingTo(got.Batch[0].Ref))
	got = j.next(t)
	j.answers <- yes(got.Batch[0])
	waitRan(t, ran)
}

func TestChimeDefersWhatItCannotJudgeYet(t *testing.T) {
	// A message that arrives while the handler runs is judged after it,
	// with the earlier batch as context.
	entered, release := make(chan struct{}), make(chan struct{})
	a, j, ran, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{Handler: func(*airut.Ctx, airut.Chimed) error {
		entered <- struct{}{}
		<-release
		return nil
	}})
	a.Text("one", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	<-entered
	a.Text("two", airuttest.As(bob))
	j.none(t, 150*time.Millisecond)
	close(release)
	waitRan(t, ran)
	got := j.next(t)
	if !slices.Equal(quoted(got.Batch), []string{"two"}) || !slices.Equal(quoted(got.Context), []string{"one"}) {
		t.Errorf("batch %q after %q", quoted(got.Batch), quoted(got.Context))
	}
	j.answers <- no()

	// Messages in the gap after an interruption wait, and are judged
	// together when it ends.
	a, j, ran, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{Cooldown: 400 * time.Millisecond})
	a.Text("one", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	start := time.Now()
	a.Text("two", airuttest.As(alice))
	a.Text("three", airuttest.As(bob))
	waitOutcome(t, b, "deferred", 1)
	got = j.next(t)
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("judged %v into the gap", d)
	}
	if !slices.Equal(quoted(got.Batch), []string{"two", "three"}) {
		t.Errorf("the deferred batch is %q", quoted(got.Batch))
	}
	j.answers <- no()

	// At MaxPerHour messages wait for the hour, and age out by MaxAge
	// meanwhile, without a judgement.
	a, j, ran, b, _ = startChime(t, airut.Config{}, airut.ChimeOptions{MaxPerHour: 1, MaxAge: 300 * time.Millisecond})
	a.Text("one", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	a.Text("two", airuttest.As(alice))
	waitOutcome(t, b, "deferred", 1)
	if st, ok := b.ChimeState(general); !ok || st.InterruptionsLeft != 0 || st.Pending != 1 || st.QuietUntil.IsZero() {
		t.Errorf("state while deferred: %+v, %v", st, ok)
	}
	waitOutcome(t, b, "aged_out", 1)
	j.none(t, 100*time.Millisecond)
	if st, _ := b.ChimeState(general); st.Pending != 0 {
		t.Errorf("%d messages still pending after aging out", st.Pending)
	}
	if _, ok := b.ChimeState(airut.Channel{Platform: "test", ID: "channel-9"}); ok {
		t.Error("a channel without a listener has a state")
	}
}

func TestChimeTurnsAreTheChannels(t *testing.T) {
	turn := func(c *airut.Ctx, _ airut.Chimed) error {
		_, err := c.Agent(airut.Turn{Prompt: "chime in"}).Text()
		return err
	}
	gate := airut.Gate{
		Access:  map[airut.Platform]airut.Access{"test": {Channels: []string{"channel-1"}}},
		PerUser: 1, PerChannel: 2, Window: time.Hour,
	}
	chimeTurn := func(a *airuttest.Adapter, j *scripted, ran chan chimed) error {
		t.Helper()
		a.Text("go on", airuttest.As(alice))
		got := j.next(t)
		j.answers <- yes(got.Batch[len(got.Batch)-1])
		return waitRan(t, ran).err
	}

	// Alice is listed nowhere, and PerUser would stop her second turn,
	// but neither applies: the turns are the channel's, and it is listed.
	// PerChannel still counts them.
	a, j, ran, _, _ := startChime(t, airut.Config{Agent: &airuttest.Agent{}, Gate: gate}, airut.ChimeOptions{Handler: turn})
	for i := range 2 {
		if err := chimeTurn(a, j, ran); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	var rf *airut.Refusal
	if err := chimeTurn(a, j, ran); !errors.As(err, &rf) || rf.Reason != airut.RefusedRate {
		t.Errorf("a third turn in the channel: %v", err)
	}

	// PerChannel is read as a setting at each turn, so lowering it from
	// chat holds for Chime turns too.
	st := &settingStore{}
	a, j, ran, _, _ = startChime(t, airut.Config{Agent: st, Gate: gate, Settings: airut.Settings{
		Owner: func(c *airut.Ctx) (bool, error) { return c.Caller() == alice, nil },
	}}, airut.ChimeOptions{Handler: turn})
	if got := lastText(a.Text("!set agent.per_channel 1", airuttest.As(alice))); got != "agent.per_channel: 2 → 1." {
		t.Fatalf("set: %q", got)
	}
	if err := chimeTurn(a, j, ran); err != nil {
		t.Fatalf("the first turn under agent.per_channel 1: %v", err)
	}
	if err := chimeTurn(a, j, ran); !errors.As(err, &rf) || rf.Reason != airut.RefusedRate {
		t.Errorf("a second turn under agent.per_channel 1: %v", err)
	}

	// A channel that is not listed may not start a turn, even when the
	// sender may.
	gate.Access = map[airut.Platform]airut.Access{"test": {Users: []string{"alice"}}}
	a, j, ran, _, _ = startChime(t, airut.Config{Agent: &airuttest.Agent{}, Gate: gate}, airut.ChimeOptions{Handler: turn})
	if err := chimeTurn(a, j, ran); !errors.As(err, &rf) || rf.Reason != airut.RefusedAccess {
		t.Errorf("a turn in a channel not listed: %v", err)
	}
}

func TestChimeAnchorsItsContext(t *testing.T) {
	a, j, _, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{ContextMessages: 2})
	var contexts [][]string
	for _, line := range []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7"} {
		a.Text(line, airuttest.As(alice))
		got := j.next(t)
		contexts = append(contexts, quoted(got.Context))
		j.answers <- no()
		// The next message must not join this batch.
		time.Sleep(20 * time.Millisecond)
	}
	// The window keeps its start while it grows to four, so each
	// judgement opens as the last did, then drops to the last two.
	want := [][]string{nil, {"m1"}, {"m1", "m2"}, {"m1", "m2", "m3"}, {"m1", "m2", "m3", "m4"}, {"m4", "m5"}, {"m4", "m5", "m6"}}
	for i := range want {
		if !slices.Equal(contexts[i], want[i]) {
			t.Errorf("judgement %d read %q, want %q", i+1, contexts[i], want[i])
		}
	}
}

func TestChimeCountsMisses(t *testing.T) {
	var reply airut.MessageRef
	a, j, ran, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{Misses: []string{"👎"}, Handler: func(c *airut.Ctx, _ airut.Chimed) error {
		var err error
		reply, err = c.Reply("chiming in")
		return err
	}})
	a.Text("hmm", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	a.React(reply, "👍", airuttest.As(alice))
	if s, _ := b.Metrics().Get("airut_chime_misses_total", "persona", "chime"); s.Value != 0 {
		t.Errorf("a reaction that is not a miss counted")
	}
	a.React(reply, "👎", airuttest.As(alice))
	a.React(reply, "👎", airuttest.As(bob))
	a.React(airut.MessageRef{ID: "msg-inv-1"}, "👎", airuttest.As(bob))
	if s, _ := b.Metrics().Get("airut_chime_misses_total", "persona", "chime"); s.Value != 1 {
		t.Errorf("%v misses, want 1", s.Value)
	}
}

func TestChimeChecksItsOptions(t *testing.T) {
	h := func(*airut.Ctx, airut.Chimed) error { return nil }
	judge := judgeFunc(func(*airut.Ctx, airut.Judgement) (airut.Verdict, error) { return no(), nil })
	ok := airut.ChimeOptions{Channels: []airut.Channel{general}, MaxPerHour: 1, Handler: h}
	for _, c := range []struct {
		name string
		cfg  airut.Config
		edit func(*airut.ChimeOptions)
	}{
		{"no judge", airut.Config{}, func(*airut.ChimeOptions) {}},
		{"no handler", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.Handler = nil }},
		{"no channel", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.Channels = nil }},
		{"no MaxPerHour", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.MaxPerHour = 0 }},
		{"too much context", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.ContextMessages = airut.MaxRoomMessages + 1 }},
		{"a negative duration", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.Cooldown = -time.Second }},
		{"a negative EngagedFor", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.EngagedFor = -time.Second }},
		{"a negative ContextAge", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.ContextAge = -time.Second }},
		{"a negative ContextBytes", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.ContextBytes = -1 }},
		{"too long a BotQuote", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.BotQuote = airut.MaxQuote + 1 }},
		{"a negative BotQuote", airut.Config{Judge: judge}, func(o *airut.ChimeOptions) { o.BotQuote = -1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := ok
			c.edit(&o)
			defer func() {
				if recover() == nil {
					t.Error("Chime did not panic")
				}
			}()
			airut.New(c.cfg).Chime(o)
		})
	}
	b := airut.New(airut.Config{Judge: judge})
	b.Chime(ok)
	// A second listener on the same channel would judge every message
	// twice.
	defer func() {
		if recover() == nil {
			t.Error("a second listener on one channel did not panic")
		}
	}()
	b.Chime(ok)
}
