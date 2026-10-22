package airut_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// hourglass is the reaction a waiting turn shows.
const hourglass = "⏳"

// sleeper is a waiting turn's sleep under the test's control: each sleep
// reports how long it would wait, and returns when the test wakes it or
// when the invocation ends.
type sleeper struct {
	asked chan time.Duration
	wake  chan struct{}
}

func newSleeper() *sleeper {
	return &sleeper{asked: make(chan time.Duration), wake: make(chan struct{})}
}

func (s *sleeper) sleep(ctx context.Context, d time.Duration) error {
	select {
	case s.asked <- d:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// next returns the wait of the next turn to go to sleep.
func (s *sleeper) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-s.asked:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no turn went to sleep")
	}
	return 0
}

// idle fails the test if a turn goes to sleep.
func (s *sleeper) idle(t *testing.T) {
	t.Helper()
	select {
	case d := <-s.asked:
		t.Fatalf("a turn went to sleep for %s", d)
	default:
	}
}

// waitGate lists user-1 to user-4, caps them at one turn a minute each
// and channel at two, and lets a refused turn wait.
var waitGate = airut.Gate{
	Access:     map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1", "user-2", "user-3", "user-4"}}},
	PerUser:    1,
	PerChannel: 2,
	Window:     time.Minute,
	Wait:       true,
}

// waitBot is gateBot on a fake clock and a sleeper.
func waitBot(t *testing.T, g airut.Gate) (*airut.Bot, *airuttest.Agent, *lockedBuffer, *fakeClock, *sleeper) {
	t.Helper()
	b, agent, logs := gateBot(t, g, airut.Budget{})
	clock := &fakeClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	airut.SetGateClock(b, clock.now)
	s := newSleeper()
	airut.SetGateSleep(b, s.sleep)
	return b, agent, logs, clock, s
}

// askLater delivers "!ask" on a goroutine, for a turn that will wait.
func askLater(a *airuttest.Adapter, opts ...airuttest.Option) <-chan []airuttest.Event {
	done := make(chan []airuttest.Event, 1)
	go func() { done <- a.Text("!ask", opts...) }()
	return done
}

func finished(t *testing.T, done <-chan []airuttest.Event) []airuttest.Event {
	t.Helper()
	select {
	case evs := <-done:
		return evs
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting invocation did not finish")
	}
	return nil
}

// about returns what the bot did about message ref, through the Conn
// or in reply, leaving out typing.
func about(a *airuttest.Adapter, ref string) []string {
	var out []string
	for _, e := range a.Events() {
		if e.Ref == ref && (e.Kind == "react" || e.Kind == "unreact") {
			out = append(out, e.Kind+":"+e.Text)
		}
	}
	return out
}

// TestRefusalCarriesWait: a rate refusal carries the wait its message
// names, whichever cap refused, and an access refusal none.
func TestRefusalCarriesWait(t *testing.T) {
	g := waitGate
	g.Wait = false
	b, _ := newBot(airut.Config{Agent: &airuttest.Agent{Steps: []airuttest.Step{{Text: "answer"}}}, Gate: g})
	errs := make(chan error, 1)
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		_, err := c.Agent(airut.Turn{Prompt: "p"}).Text()
		errs <- err
		return err
	})
	clock := &fakeClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	airut.SetGateClock(b, clock.now)
	a := airuttest.Start(t, b)
	ask := func(opts ...airuttest.Option) (*airut.Refusal, string) {
		t.Helper()
		got := lastText(a.Text("!ask", opts...))
		var rf *airut.Refusal
		if err := <-errs; err != nil && !errors.As(err, &rf) {
			t.Fatalf("not a refusal: %v", err)
		}
		return rf, got
	}

	if rf, _ := ask(elsewhere); rf != nil {
		t.Fatalf("the first turn was refused: %v", rf)
	}
	clock.advance(20 * time.Second)
	rf, msg := ask()
	if rf == nil || rf.Reason != airut.RefusedRate || rf.Wait != 40*time.Second || !strings.HasSuffix(msg, "Try again in 40 seconds.") {
		t.Errorf("the caller's cap: %+v %q, want a rate refusal waiting 40s", rf, msg)
	}
	ask(as("user-2"))
	clock.advance(10 * time.Second)
	ask(as("user-3"))
	rf, msg = ask(as("user-4"))
	if rf == nil || rf.Reason != airut.RefusedRate || rf.Wait != 50*time.Second || !strings.Contains(msg, "this channel") {
		t.Errorf("the channel's cap: %+v %q, want a rate refusal waiting 50s", rf, msg)
	}
	rf, _ = ask(as("stranger"))
	if rf == nil || rf.Reason != airut.RefusedAccess || rf.Wait != 0 {
		t.Errorf("the access list: %+v, want an access refusal with no wait", rf)
	}
}

// TestGateWaitOff: without Gate.Wait a refused turn fails at once, and
// shows no reaction.
func TestGateWaitOff(t *testing.T) {
	g := waitGate
	g.Wait = false
	b, _, _, clock, s := waitBot(t, g)
	a := airuttest.Start(t, b)
	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(20 * time.Second)
	wantEvents(t, a.Text("!ask"), "reply:You have started too many agent turns. Try again in 40 seconds.")
	s.idle(t)
	if n := count(t, b.Metrics(), "airut_agent_turn_waits_total", "outcome", "refused"); n != 0 {
		t.Errorf("%v waits counted with Wait off", n)
	}
}

// TestGateWaitStartsWhenFree: a refused turn reacts, waits for its slot,
// takes the reaction down, and runs; a second from the same caller is
// refused meanwhile, while another caller may wait at the same time; and
// no typing shows while a turn waits.
func TestGateWaitStartsWhenFree(t *testing.T) {
	b, agent, logs, clock, s := waitBot(t, waitGate)
	a := airuttest.Start(t, b)
	m := b.Metrics()

	wantEvents(t, a.Text("!ask"), "reply:answer")                          // inv-1
	wantEvents(t, a.Text("!ask", as("user-2"), elsewhere), "reply:answer") // inv-2
	clock.advance(20 * time.Second)
	mine := askLater(a) // inv-3
	if d := s.next(t); d != 40*time.Second {
		t.Errorf("the turn slept %s, want the 40s its refusal names", d)
	}
	if got := about(a, "msg-inv-3"); !slices.Equal(got, []string{"react:" + hourglass}) {
		t.Errorf("while it waits: %q, want the hourglass", got)
	}
	for _, e := range a.Events() {
		if e.Kind == "typing" || (e.Kind == "reply" && strings.Contains(e.Text, "get to this")) {
			t.Errorf("while the turn waits the bot showed %s %q", e.Kind, e.Text)
		}
	}

	// The same caller again: refused as without Wait, with its wait.
	wantEvents(t, a.Text("!ask"), "reply:You have started too many agent turns. Try again in 40 seconds.") // inv-4
	if n := count(t, m, "airut_agent_turn_waits_total", "outcome", "refused"); n != 1 {
		t.Errorf("waits refused = %v, want 1", n)
	}
	// Another caller waits beside the first.
	theirs := askLater(a, as("user-2"), elsewhere) // inv-5
	s.next(t)

	clock.advance(40 * time.Second)
	s.wake <- struct{}{}
	s.wake <- struct{}{}
	finished(t, mine)
	finished(t, theirs)
	for _, ref := range []string{"msg-inv-3", "msg-inv-5"} {
		if got := about(a, ref); !slices.Equal(got, []string{"react:" + hourglass, "unreact:" + hourglass}) {
			t.Errorf("%s: %q, want the hourglass shown and taken down", ref, got)
		}
	}
	evs := a.Events()
	unreact := slices.IndexFunc(evs, func(e airuttest.Event) bool { return e.Kind == "unreact" })
	if typing := slices.IndexFunc(evs, func(e airuttest.Event) bool { return e.Kind == "typing" }); typing >= 0 && typing < unreact {
		t.Errorf("typing showed before the turn started: %q", texts(evs))
	}
	if answers := slices.DeleteFunc(texts(evs), func(s string) bool { return s != "reply:answer" }); len(answers) != 4 {
		t.Errorf("%d answers, want 4: %q", len(answers), texts(evs))
	}
	if n := len(agent.Turns()); n != 4 {
		t.Errorf("%d turns reached the agent, want 4", n)
	}
	for _, c := range []struct {
		name, outcome string
		want          float64
	}{
		{"airut_agent_turn_waits_total", "started", 2},
		{"airut_agent_turn_waits_total", "refused", 1},
		{"airut_agent_turns_total", "ok", 4},
		{"airut_agent_turns_total", "refused_rate", 1},
	} {
		labels := []string{"outcome", c.outcome}
		if c.name == "airut_agent_turns_total" {
			labels = append([]string{"command", "ask"}, labels...)
		}
		if n := count(t, m, c.name, labels...); n != c.want {
			t.Errorf("%s{%s} = %v, want %v", c.name, c.outcome, n, c.want)
		}
	}
	l := logs.String()
	if !strings.Contains(l, "agent turn waiting") || !strings.Contains(l, "wait=40s") || !strings.Contains(l, "agent turn wait over") {
		t.Errorf("the wait was not logged with its duration:\n%s", l)
	}
	if strings.Contains(l, "secret prompt") || strings.Contains(l, "get to this") {
		t.Errorf("the log holds chat text:\n%s", l)
	}
}

// TestGateWaitReplies: a turn that cannot react says once how long it
// will wait, and runs when its slot frees.
func TestGateWaitReplies(t *testing.T) {
	b, _, logs, clock, s := waitBot(t, waitGate)
	a := airuttest.Start(t, b)
	a.FailReact("channel-1", errors.New("reactions at secret-host are off"))
	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(time.Second)
	done := askLater(a)
	s.next(t)
	clock.advance(59 * time.Second)
	s.wake <- struct{}{}
	wantEvents(t, finished(t, done), "reply:I'll get to this in 59 seconds.", "reply:answer")
	if l := logs.String(); !strings.Contains(l, "waiting reaction failed") || strings.Contains(l, "secret-host") {
		t.Errorf("the failed reaction was not logged by kind alone:\n%s", l)
	}
}

// TestGateWaitAgain: a turn whose slot is taken while it sleeps waits
// again, inside its window, and then runs.
func TestGateWaitAgain(t *testing.T) {
	b, agent, _, clock, s := waitBot(t, waitGate)
	a := airuttest.Start(t, b)
	wantEvents(t, a.Text("!ask", as("user-2")), "reply:answer") // 0s
	clock.advance(10 * time.Second)
	wantEvents(t, a.Text("!ask", as("user-3")), "reply:answer") // 10s: the channel is full
	clock.advance(10 * time.Second)
	done := askLater(a)
	if d := s.next(t); d != 40*time.Second {
		t.Fatalf("the first wait: %s, want 40s", d)
	}
	clock.advance(40 * time.Second)
	// user-4 takes the slot that freed at 60s before the waiting turn wakes.
	wantEvents(t, a.Text("!ask", as("user-4")), "reply:answer")
	s.wake <- struct{}{}
	if d := s.next(t); d != 10*time.Second {
		t.Fatalf("the second wait: %s, want the 10s until the turn at 10s leaves", d)
	}
	clock.advance(10 * time.Second)
	s.wake <- struct{}{}
	if got := lastText(finished(t, done)); got != "answer" {
		t.Errorf("the waiting turn ended with %q, want its answer", got)
	}
	if got := about(a, "msg-inv-3"); !slices.Equal(got, []string{"react:" + hourglass, "unreact:" + hourglass}) {
		t.Errorf("%q, want the hourglass shown once and taken down", got)
	}
	if n := len(agent.Turns()); n != 4 {
		t.Errorf("%d turns reached the agent, want 4", n)
	}
	if n := count(t, b.Metrics(), "airut_agent_turn_waits_total", "outcome", "started"); n != 1 {
		t.Errorf("waits started = %v, want 1", n)
	}
}

// TestGateWaitExpires: a turn that would have to wait past one window
// from its first refusal fails with the refusal it would have had.
func TestGateWaitExpires(t *testing.T) {
	b, agent, _, clock, s := waitBot(t, waitGate)
	a := airuttest.Start(t, b)
	wantEvents(t, a.Text("!ask", elsewhere), "reply:answer") // 0s
	clock.advance(20 * time.Second)
	done := askLater(a)
	s.next(t)
	clock.advance(10 * time.Second)
	wantEvents(t, a.Text("!ask", as("user-2")), "reply:answer") // 30s
	clock.advance(20 * time.Second)
	wantEvents(t, a.Text("!ask", as("user-3")), "reply:answer") // 50s: the channel is full until 90s
	clock.advance(10 * time.Second)
	s.wake <- struct{}{}
	// At 60s the turn would wait until 90s, past 80s, a window after 20s.
	const refusal = "Too many agent turns have started in this channel. Try again in 30 seconds."
	if got := lastText(finished(t, done)); got != refusal {
		t.Errorf("the waiting turn ended with %q, want %q", got, refusal)
	}
	if got := about(a, "msg-inv-2"); !slices.Equal(got, []string{"react:" + hourglass, "unreact:" + hourglass}) {
		t.Errorf("%q, want the hourglass shown and taken down", got)
	}
	s.idle(t)
	if n := len(agent.Turns()); n != 3 {
		t.Errorf("%d turns reached the agent, want 3", n)
	}
	m := b.Metrics()
	if n := count(t, m, "airut_agent_turn_waits_total", "outcome", "expired"); n != 1 {
		t.Errorf("waits expired = %v, want 1", n)
	}
	if n := count(t, m, "airut_agent_turns_total", "command", "ask", "outcome", "refused_rate"); n != 1 {
		t.Errorf("turns refused = %v, want 1", n)
	}
}

// TestGateWaitCancelled: a turn waiting when the bot stops runs nothing,
// says nothing, and takes its reaction down.
func TestGateWaitCancelled(t *testing.T) {
	b, agent, _, clock, s := waitBot(t, waitGate)
	a := airuttest.New("")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = b.Run(ctx, a)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	<-a.Ready()
	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(20 * time.Second)
	done := askLater(a)
	s.next(t)
	cancel()
	wantEvents(t, finished(t, done), "react:"+hourglass, "unreact:"+hourglass)
	if n := len(agent.Turns()); n != 1 {
		t.Errorf("%d turns reached the agent, want only the first", n)
	}
	m := b.Metrics()
	if n := count(t, m, "airut_agent_turn_waits_total", "outcome", "cancelled"); n != 1 {
		t.Errorf("waits cancelled = %v, want 1", n)
	}
	if n := count(t, m, "airut_agent_turns_total", "command", "ask", "outcome", "canceled"); n != 1 {
		t.Errorf("turns canceled = %v, want 1", n)
	}
}

// TestGateWaitNeverForChime: a turn a Chime handler starts is refused at
// once, as without Wait; it would otherwise sleep for the hour its
// refusal names, and waitRan would give up.
func TestGateWaitNeverForChime(t *testing.T) {
	turn := func(c *airut.Ctx, _ airut.Chimed) error {
		_, err := c.Agent(airut.Turn{Prompt: "chime in"}).Text()
		return err
	}
	gate := airut.Gate{
		Access:     map[airut.Platform]airut.Access{"test": {Channels: []string{"channel-1"}}},
		PerChannel: 1, Window: time.Hour, Wait: true,
	}
	a, j, ran, b, _ := startChime(t, airut.Config{Agent: &airuttest.Agent{}, Gate: gate}, airut.ChimeOptions{Handler: turn})
	chimeTurn := func() error {
		t.Helper()
		a.Text("go on", airuttest.As(alice))
		got := j.next(t)
		j.answers <- yes(got.Batch[len(got.Batch)-1])
		return waitRan(t, ran).err
	}
	if err := chimeTurn(); err != nil {
		t.Fatalf("the first turn: %v", err)
	}
	var rf *airut.Refusal
	if err := chimeTurn(); !errors.As(err, &rf) || rf.Reason != airut.RefusedRate || rf.Wait <= 0 {
		t.Errorf("a second turn in the channel: %v", err)
	}
	for _, e := range a.Events() {
		if e.Text == hourglass {
			t.Errorf("a Chime turn showed the hourglass: %+v", e)
		}
	}
	for _, o := range []string{"started", "expired", "cancelled", "refused"} {
		if n := count(t, b.Metrics(), "airut_agent_turn_waits_total", "outcome", o); n != 0 {
			t.Errorf("a Chime turn counted a wait %s", o)
		}
	}
}

// slowReact is the fake adapter with a Conn whose React takes took on
// the fake clock, as a slow platform request does.
type slowReact struct {
	*airuttest.Adapter
	clock *fakeClock
	took  time.Duration
}

func (a slowReact) Run(ctx context.Context, h airut.Host) error {
	return a.Adapter.Run(ctx, slowHost{h, a})
}

type slowHost struct {
	airut.Host
	a slowReact
}

func (h slowHost) Ready(ctx context.Context, c airut.Conn) {
	h.Host.Ready(ctx, slowConn{c, h.a})
}

type slowConn struct {
	airut.Conn
	a slowReact
}

func (c slowConn) React(ctx context.Context, ch airut.Channel, ref airut.MessageRef, emoji string) (airut.MessageRef, error) {
	c.a.clock.advance(c.a.took)
	return c.Conn.React(ctx, ch, ref, emoji)
}

// startSlow runs b on a fake adapter whose reactions take took on clock,
// until the test ends.
func startSlow(t *testing.T, b *airut.Bot, clock *fakeClock, took time.Duration) *airuttest.Adapter {
	t.Helper()
	a := airuttest.New("")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = b.Run(ctx, slowReact{a, clock, took})
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	<-a.Ready()
	return a
}

// TestGateWaitCountsTheNotice: the time the reaction took comes off the
// sleep, so the retry is due when the refusal said.
func TestGateWaitCountsTheNotice(t *testing.T) {
	b, agent, _, clock, s := waitBot(t, waitGate)
	a := startSlow(t, b, clock, 15*time.Second)
	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(20 * time.Second)
	done := askLater(a)
	if d := s.next(t); d != 25*time.Second {
		t.Errorf("after a 15s reaction the turn slept %s, want the 25s left of its 40s", d)
	}
	clock.advance(25 * time.Second)
	s.wake <- struct{}{}
	if got := lastText(finished(t, done)); got != "answer" {
		t.Errorf("the waiting turn ended with %q", got)
	}
	if n := len(agent.Turns()); n != 2 {
		t.Errorf("%d turns reached the agent, want 2", n)
	}
}

// TestGateWaitRepliesWithTimeLeft: the fallback reply names the time
// left as it is sent, after the failed reaction took its time.
func TestGateWaitRepliesWithTimeLeft(t *testing.T) {
	b, _, _, clock, s := waitBot(t, waitGate)
	a := startSlow(t, b, clock, 15*time.Second)
	a.FailReact("channel-1", errors.New("off"))
	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(20 * time.Second)
	done := askLater(a)
	s.next(t)
	clock.advance(25 * time.Second)
	s.wake <- struct{}{}
	wantEvents(t, finished(t, done), "reply:I'll get to this in 25 seconds.", "reply:answer")
}

// TestGateWaitNoticeOutlastsTheWait: a reaction that took longer than
// the wait leaves nothing to sleep, and the turn retries at once.
func TestGateWaitNoticeOutlastsTheWait(t *testing.T) {
	b, agent, _, clock, s := waitBot(t, waitGate)
	a := startSlow(t, b, clock, 45*time.Second)
	wantEvents(t, a.Text("!ask"), "reply:answer")
	clock.advance(20 * time.Second)
	wantEvents(t, finished(t, askLater(a)), "react:"+hourglass, "unreact:"+hourglass, "reply:answer")
	s.idle(t)
	if n := len(agent.Turns()); n != 2 {
		t.Errorf("%d turns reached the agent, want 2", n)
	}
}

// TestGateWaitWokenPastTheDeadline: a turn whose sleep ends after one
// window from its first refusal is not admitted, though its slot is
// free, and runs nothing; one woken exactly at the deadline starts.
func TestGateWaitWokenPastTheDeadline(t *testing.T) {
	for _, c := range []struct {
		name  string
		late  time.Duration
		start bool
	}{
		{"at the deadline", 0, true},
		{"past it", time.Nanosecond, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, agent, _, clock, s := waitBot(t, waitGate)
			a := airuttest.Start(t, b)
			wantEvents(t, a.Text("!ask"), "reply:answer")
			clock.advance(20 * time.Second)
			done := askLater(a)
			s.next(t)
			// The deadline is 80s, a window after the refusal at 20s.
			clock.advance(60*time.Second + c.late)
			s.wake <- struct{}{}
			got := lastText(finished(t, done))
			s.idle(t)
			turns := len(agent.Turns())
			expired := count(t, b.Metrics(), "airut_agent_turn_waits_total", "outcome", "expired")
			if c.start {
				if got != "answer" || turns != 2 || expired != 0 {
					t.Errorf("ended with %q, %d turns, %v expired; want the turn started", got, turns, expired)
				}
				return
			}
			if want := "You have started too many agent turns. Try again in 1 second."; got != want {
				t.Errorf("ended with %q, want %q", got, want)
			}
			if turns != 1 || expired != 1 {
				t.Errorf("%d turns, %v expired; want the waiting turn never run, and expired", turns, expired)
			}
			if got := about(a, "msg-inv-2"); !slices.Equal(got, []string{"react:" + hourglass, "unreact:" + hourglass}) {
				t.Errorf("%q, want the hourglass shown and taken down", got)
			}
		})
	}
}
