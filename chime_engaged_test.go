package airut_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func TestChimeEngagesARoomTheBotSpokeIn(t *testing.T) {
	picked := make(chan airut.MessageEvent, 4)
	var hello airut.MessageRef
	a, j, ran, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{
		Prompt: "Interrupt?", FollowUpPersona: "follow", FollowUpPrompt: "Follow up?",
		EngagedFor: time.Hour, Cooldown: time.Hour, MaxPerHour: 1,
		OnPicked: func(c *airut.Ctx, ev airut.MessageEvent) error { picked <- ev; return nil },
		Handler: func(c *airut.Ctx, e airut.Chimed) error {
			ref, err := c.Reply("hello there")
			if hello.ID == "" {
				hello = ref
			}
			return err
		},
	})
	a.Text("hi all", airuttest.As(alice))
	got := j.next(t)
	if got.Persona != "chime" || got.Prompt != "Interrupt?" {
		t.Errorf("a cold room was judged as %q, %q", got.Persona, got.Prompt)
	}
	j.answers <- yes(got.Batch[0])
	if r := waitRan(t, ran); r.e.FollowUp {
		t.Error("an interruption was called a follow-up")
	}
	if ev := <-picked; ev.Text != "hi all" {
		t.Errorf("OnPicked saw %q", ev.Text)
	}
	if st, _ := b.ChimeState(general); time.Until(st.EngagedUntil) < 50*time.Minute || st.InterruptionsLeft != 0 {
		t.Errorf("the state after speaking: %+v", st)
	}

	// The room is engaged: neither the gap nor the cap holds it, the
	// follow-up question is asked, and the bot's answer is in context.
	a.Text("can you see my last message?", airuttest.As(alice), airuttest.ReplyingTo(hello))
	got = j.next(t)
	if got.Persona != "follow" || got.Prompt != "Follow up?" {
		t.Errorf("an engaged room was judged as %q, %q", got.Persona, got.Prompt)
	}
	if !slices.Equal(quoted(got.Context), []string{"hi all", "hello there"}) || !got.Context[1].FromBot || got.Context[0].FromBot {
		t.Errorf("context %+v", got.Context)
	}
	j.answers <- yes(got.Batch[0])
	if r := waitRan(t, ran); !r.e.FollowUp {
		t.Error("a follow-up was called an interruption")
	}
	if outcome(b, "deferred") != 0 {
		t.Error("an engaged room was deferred")
	}
	s, _ := b.Metrics().Get("airut_chime_total", "persona", "chime", "outcome", "spoke", "engaged", "true")
	if s.Value != 1 {
		t.Errorf("engaged answers counted %v times", s.Value)
	}
}

func TestChimeIsEngagedByWhatTheBotSays(t *testing.T) {
	// A command's reply engages the room, as any send through a Ctx does.
	a, j, _, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{EngagedFor: time.Hour, FollowUpPrompt: "Follow up?"})
	if st, _ := b.ChimeState(general); !st.EngagedUntil.IsZero() {
		t.Errorf("a fresh room is engaged until %v", st.EngagedUntil)
	}
	a.Text("!roll 1d6", airuttest.As(alice))
	a.Text("nice", airuttest.As(bob))
	// With no FollowUpPersona the persona is Persona's.
	if got := j.next(t); got.Prompt != "Follow up?" || got.Persona != "chime" {
		t.Errorf("after a command reply the room was judged with %q, %q", got.Persona, got.Prompt)
	}
	j.answers <- no()

	// Without EngagedFor the bot's messages are read, and never engage.
	a, j, _, b, _ = startChime(t, airut.Config{}, airut.ChimeOptions{Prompt: "Interrupt?", FollowUpPrompt: "Follow up?"})
	a.Text("!roll 1d6", airuttest.As(alice))
	a.Text("nice", airuttest.As(bob))
	if got := j.next(t); got.Prompt != "Interrupt?" || !slices.Equal(quoted(got.Context), []string{"rolled 1d6 x1"}) {
		t.Errorf("judged with %q after %q", got.Prompt, quoted(got.Context))
	}
	j.answers <- no()
	if st, _ := b.ChimeState(general); !st.EngagedUntil.IsZero() {
		t.Errorf("a room engaged with no EngagedFor: %+v", st)
	}
}

func TestChimeReadsTheBotsMessagesCut(t *testing.T) {
	long := strings.Repeat("a", 400)
	a, j, ran, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{BotQuote: 100, Handler: func(c *airut.Ctx, _ airut.Chimed) error {
		ref, err := c.Reply("draft")
		if err != nil {
			return err
		}
		// A streamed reply is edited as it grows, and the judge reads
		// the latest.
		if err := c.Edit(ref, long); err != nil {
			return err
		}
		_, err = c.ReplyPrivate("only for you")
		return err
	}})
	a.Text("question", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	a.Text("next", airuttest.As(bob))
	got := j.next(t)
	var bot []airut.Quoted
	for _, q := range append(got.Context, got.Batch...) {
		if q.FromBot {
			bot = append(bot, q)
		}
	}
	if len(bot) != 1 || len(bot[0].Text) > 100 || !strings.HasPrefix(bot[0].Text, "aaa") || !strings.HasSuffix(bot[0].Text, "…") {
		t.Errorf("the bot's messages read as %+v", bot)
	}
	j.answers <- no()
}

func TestChimeBoundsItsContext(t *testing.T) {
	// ContextBytes drops the oldest to half of it once the window is
	// past it, so judgements between drops open alike.
	a, j, _, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{ContextBytes: 7})
	var contexts [][]string
	for _, line := range []string{"aa1", "aa2", "aa3", "aa4", "aa5"} {
		a.Text(line, airuttest.As(alice))
		contexts = append(contexts, quoted(j.next(t).Context))
		j.answers <- no()
		time.Sleep(20 * time.Millisecond)
	}
	want := [][]string{nil, {"aa1"}, {"aa1", "aa2"}, {"aa3"}, {"aa3", "aa4"}}
	for i := range want {
		if !slices.Equal(contexts[i], want[i]) {
			t.Errorf("judgement %d read %q, want %q", i+1, contexts[i], want[i])
		}
	}

	// ContextAge drops what has grown stale: once the oldest is older
	// than it, everything older than half of it.
	a, j, _, _, _ = startChime(t, airut.Config{}, airut.ChimeOptions{ContextAge: 600 * time.Millisecond})
	a.Text("old news", airuttest.As(alice))
	j.next(t)
	j.answers <- no()
	time.Sleep(300 * time.Millisecond)
	a.Text("fresh", airuttest.As(alice))
	if got := j.next(t); !slices.Equal(quoted(got.Context), []string{"old news"}) {
		t.Errorf("a fresh window read %q", quoted(got.Context))
	}
	j.answers <- no()
	time.Sleep(300 * time.Millisecond)
	a.Text("later", airuttest.As(alice))
	if got := j.next(t); len(got.Context) != 0 {
		t.Errorf("a stale window read %q", quoted(got.Context))
	}
	j.answers <- no()
}

func TestChimeFollowUpsAreTheSenders(t *testing.T) {
	carol := airut.User{Platform: "test", ID: "carol", Name: "Carol"}
	limited := make(chan airut.MessageEvent, 4)
	turn := func(c *airut.Ctx, _ airut.Chimed) error {
		_, err := c.Agent(airut.Turn{Prompt: "follow up"}).Text()
		return err
	}
	// The channel is not listed, and PerChannel would stop a second turn,
	// but a follow-up is the sender's: it needs their access and takes
	// their PerUser.
	gate := airut.Gate{
		Access:  map[airut.Platform]airut.Access{"test": {Users: []string{"alice", "bob"}}},
		PerUser: 1, PerChannel: 1, Window: time.Hour,
	}
	a, j, ran, b, _ := startChime(t, airut.Config{Agent: &airuttest.Agent{}, Gate: gate}, airut.ChimeOptions{
		EngagedFor: time.Hour, Handler: turn,
		OnLimited: func(c *airut.Ctx, ev airut.MessageEvent) error { limited <- ev; return nil },
	})
	a.Text("!roll 1d6", airuttest.As(alice))
	follow := func(u airut.User, text string) {
		t.Helper()
		a.Text(text, airuttest.As(u))
		got := j.next(t)
		j.answers <- yes(got.Batch[len(got.Batch)-1])
	}
	follow(alice, "and again?")
	if r := waitRan(t, ran); r.err != nil {
		t.Fatalf("alice's follow-up: %v", r.err)
	}
	follow(bob, "me too")
	if r := waitRan(t, ran); r.err != nil {
		t.Fatalf("bob's follow-up, past PerChannel: %v", r.err)
	}
	// Alice is at PerUser: the handler does not run, and OnLimited does.
	follow(alice, "once more")
	waitOutcome(t, b, "limited", 1)
	if ev := <-limited; ev.Text != "once more" || ev.Sender.ID != "alice" {
		t.Errorf("OnLimited saw %q from %s", ev.Text, ev.Sender.ID)
	}
	noRun(t, ran)
	// Carol may not start a turn at all, and is refused without a hook.
	follow(carol, "what about me?")
	waitOutcome(t, b, "refused", 1)
	noRun(t, ran)
	if len(limited) != 0 {
		t.Error("OnLimited ran for a refused sender")
	}
}

func TestChimeTellsOfTheJudgeRateWhenEngaged(t *testing.T) {
	limited := make(chan airut.MessageEvent, 4)
	a, j, _, b, _ := startChime(t, airut.Config{JudgeRate: airut.JudgeRate{PerChannel: 1, Window: time.Hour}}, airut.ChimeOptions{
		EngagedFor: time.Hour,
		OnLimited: func(c *airut.Ctx, ev airut.MessageEvent) error {
			limited <- ev
			return errors.New("the hook fails, and is only logged")
		},
	})
	a.Text("one", airuttest.As(alice))
	j.answers <- no()
	j.next(t)
	a.Text("!roll 1d6", airuttest.As(alice))
	a.Text("two", airuttest.As(alice))
	waitOutcome(t, b, "limited", 1)
	select {
	case ev := <-limited:
		if ev.Text != "two" {
			t.Errorf("OnLimited saw %q", ev.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnLimited did not run")
	}

	// A cold room says nothing when the judge's rate stops it.
	a, j, _, b, _ = startChime(t, airut.Config{JudgeRate: airut.JudgeRate{PerChannel: 1, Window: time.Hour}}, airut.ChimeOptions{
		OnLimited: func(c *airut.Ctx, ev airut.MessageEvent) error { limited <- ev; return nil },
	})
	a.Text("one", airuttest.As(alice))
	j.answers <- no()
	j.next(t)
	time.Sleep(20 * time.Millisecond)
	a.Text("two", airuttest.As(alice))
	waitOutcome(t, b, "limited", 1)
	if len(limited) != 0 {
		t.Error("OnLimited ran in a cold room")
	}
}

func TestChimeDeclaresItsSettings(t *testing.T) {
	st := &settingStore{}
	owner := func(c *airut.Ctx) (bool, error) { return c.Caller() == alice, nil }
	a, j, ran, b, _ := startChime(t, airut.Config{Agent: st, Settings: airut.Settings{Owner: owner}},
		airut.ChimeOptions{Settings: "chime", MaxPerHour: 5, Cooldown: time.Minute})
	if got := lastText(a.Text("!set chime.max_per_hour 6", airuttest.As(alice))); !strings.Contains(got, "from 1 to 5") {
		t.Errorf("raising max_per_hour past its ceiling: %q", got)
	}
	if got := lastText(a.Text("!set chime.max_per_hour 2", airuttest.As(alice))); got != "chime.max_per_hour: 5 → 2." {
		t.Errorf("set: %q", got)
	}
	if got := lastText(a.Text("!set chime.cooldown 0s", airuttest.As(alice))); got != "chime.cooldown: 1m → 0s." {
		t.Errorf("set: %q", got)
	}
	if s, _ := b.ChimeState(general); s.InterruptionsLeft != 2 {
		t.Errorf("%d interruptions left under max_per_hour 2", s.InterruptionsLeft)
	}
	// The listener reads them at use time: with no gap, a second
	// interruption is judged at once.
	for _, line := range []string{"one", "two"} {
		a.Text(line, airuttest.As(bob))
		got := j.next(t)
		j.answers <- yes(got.Batch[len(got.Batch)-1])
		waitRan(t, ran)
	}
	if s, _ := b.ChimeState(general); s.InterruptionsLeft != 0 {
		t.Errorf("%d interruptions left after two", s.InterruptionsLeft)
	}
	// A cap lowered below what the hour already holds leaves none, not
	// fewer than none.
	lastText(a.Text("!set chime.max_per_hour 1", airuttest.As(alice)))
	if s, _ := b.ChimeState(general); s.InterruptionsLeft != 0 {
		t.Errorf("%d interruptions left under a lowered cap", s.InterruptionsLeft)
	}
}

// fitting is a scripted judge that is a JudgeLimit: a judgement fits
// while its context holds at most most messages.
type fitting struct {
	*scripted
	most int
}

func (f fitting) InputTokens() int            { return 100 }
func (f fitting) Fits(j airut.Judgement) bool { return len(j.Context) <= f.most }

func TestChimeFitsAJudgeLimit(t *testing.T) {
	wrap := func(j *scripted) airut.Judge { return fitting{j, 2} }
	a, j, _, _, _ := startChimeWith(t, airut.Config{}, airut.ChimeOptions{ContextTokens: 50}, wrap)
	var contexts [][]string
	for _, line := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		a.Text(line, airuttest.As(alice))
		got := j.next(t)
		if got.InputTokens != 50 {
			t.Errorf("the judgement carried InputTokens %d", got.InputTokens)
		}
		contexts = append(contexts, quoted(got.Context))
		j.answers <- no()
		time.Sleep(20 * time.Millisecond)
	}
	// A window that does not fit drops its oldest half, so judgements
	// between drops open alike.
	want := [][]string{nil, {"m1"}, {"m1", "m2"}, {"m3"}, {"m3", "m4"}, {"m5"}}
	for i := range want {
		if !slices.Equal(contexts[i], want[i]) {
			t.Errorf("judgement %d read %q, want %q", i+1, contexts[i], want[i])
		}
	}

	// With Settings, context_tokens is a setting the judge's cap bounds.
	owner := func(c *airut.Ctx) (bool, error) { return c.Caller() == alice, nil }
	a, j, _, _, _ = startChimeWith(t, airut.Config{Agent: &settingStore{}, Settings: airut.Settings{Owner: owner}},
		airut.ChimeOptions{Settings: "chime", ContextTokens: 30}, wrap)
	// ContextTokens is the setting's default.
	a.Text("first", airuttest.As(bob))
	if got := j.next(t); got.InputTokens != 30 {
		t.Errorf("the default carried InputTokens %d", got.InputTokens)
	}
	j.answers <- no()
	if got := lastText(a.Text("!set chime.context_tokens 101", airuttest.As(alice))); !strings.Contains(got, "from 1 to 100") {
		t.Errorf("raising context_tokens past the judge's cap: %q", got)
	}
	if got := lastText(a.Text("!set chime.context_tokens 80", airuttest.As(alice))); got != "chime.context_tokens: 30 → 80." {
		t.Errorf("set: %q", got)
	}
	a.Text("hello", airuttest.As(bob))
	if got := j.next(t); got.InputTokens != 80 {
		t.Errorf("the judgement carried InputTokens %d", got.InputTokens)
	}
	j.answers <- no()
}

func TestChimeKeepsItsStateAcrossFollowUps(t *testing.T) {
	// A follow-up opens no gap: only interruptions do.
	a, j, ran, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{EngagedFor: time.Hour, Cooldown: time.Hour,
		Handler: func(c *airut.Ctx, e airut.Chimed) error {
			if !e.FollowUp {
				_, err := c.Reply("hello")
				return err
			}
			return nil
		}})
	a.Text("hi", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	before, _ := b.ChimeState(general)
	a.Text("and you?", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	if after, _ := b.ChimeState(general); !after.QuietUntil.Equal(before.QuietUntil) {
		t.Errorf("a follow-up moved the gap from %v to %v", before.QuietUntil, after.QuietUntil)
	}

	// Messages held in a gap are judged as soon as the bot speaks and
	// engages the room, without waiting out the gap.
	a, j, ran, _, _ = startChime(t, airut.Config{}, airut.ChimeOptions{EngagedFor: time.Hour, Cooldown: time.Hour, MaxAge: time.Hour})
	a.Text("one", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	a.Text("two", airuttest.As(bob))
	j.none(t, 150*time.Millisecond)
	a.Text("!roll 1d6", airuttest.As(alice))
	// The roll's reply came after, so it is in the batch, as the bot's.
	if got := j.next(t); !slices.Equal(quoted(got.Batch), []string{"two", "rolled 1d6 x1"}) || !got.Batch[1].FromBot {
		t.Errorf("the held batch is %q", quoted(got.Batch))
	}
	j.answers <- no()

	// A send with no text, such as a file, engages the room as well, and
	// releases what it held.
	var rc *airut.Ctx
	a, j, ran, _, _ = startChime(t, airut.Config{}, airut.ChimeOptions{EngagedFor: time.Hour, Cooldown: time.Hour, MaxAge: time.Hour,
		Handler: func(c *airut.Ctx, _ airut.Chimed) error { rc = c; return nil }})
	a.Text("one", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	a.Text("two", airuttest.As(bob))
	j.none(t, 150*time.Millisecond)
	if _, err := rc.Attach("dice.png", "image/png", []byte("png")); err != nil {
		t.Fatal(err)
	}
	if got := j.next(t); !slices.Equal(quoted(got.Batch), []string{"two"}) {
		t.Errorf("the batch a file released is %q", quoted(got.Batch))
	}
	j.answers <- no()

	// What waits is bounded: the oldest fall into the context unjudged.
	a, j, ran, b, _ = startChime(t, airut.Config{}, airut.ChimeOptions{Cooldown: time.Hour, MaxAge: time.Hour})
	a.Text("one", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	for i := range airut.MaxRoomMessages + 5 {
		a.Text("line "+strconv.Itoa(i), airuttest.As(bob))
	}
	if st, _ := b.ChimeState(general); st.Pending != airut.MaxRoomMessages {
		t.Errorf("%d messages pending, want %d", st.Pending, airut.MaxRoomMessages)
	}
}

func TestChimeTreatsARepliedBotAsNoPerson(t *testing.T) {
	var hello airut.MessageRef
	a, j, ran, b, _ := startChime(t, airut.Config{}, airut.ChimeOptions{StayOut: true, Handler: func(c *airut.Ctx, _ airut.Chimed) error {
		var err error
		hello, err = c.Reply("hello")
		return err
	}})
	a.Text("hi", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	// Bob answers the bot, which is no exchange between other people: it
	// is judged, and a yes about it is answered.
	a.Text("hello to you", airuttest.As(bob), airuttest.ReplyingTo(hello))
	got := j.next(t)
	j.answers <- yes(got.Batch[0])
	waitRan(t, ran)
	if outcome(b, "stayed_out") != 0 {
		t.Error("a reply to the bot stayed out")
	}
}

func TestChimeReadsAnEditOfAnOlderReply(t *testing.T) {
	var reply airut.MessageRef
	var rc *airut.Ctx
	a, j, ran, _, _ := startChime(t, airut.Config{}, airut.ChimeOptions{BotQuote: 50, Handler: func(c *airut.Ctx, _ airut.Chimed) error {
		var err error
		rc = c
		reply, err = c.Reply("short")
		return err
	}})
	a.Text("question", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	a.Text("next", airuttest.As(bob))
	j.next(t)
	j.answers <- no()
	// The reply is in the context now. An edit replaces it there, cut.
	if err := rc.Edit(reply, strings.Repeat("b", 200)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	a.Text("after", airuttest.As(bob))
	got := j.next(t)
	for _, q := range got.Context {
		if q.FromBot && (len(q.Text) > 50 || !strings.HasPrefix(q.Text, "bbb")) {
			t.Errorf("the edited reply reads %q", q.Text)
		}
	}
	j.answers <- no()

	// An edit that grows the window past ContextBytes is bounded before
	// the next judgement, not only after it.
	a, j, ran, _, _ = startChime(t, airut.Config{}, airut.ChimeOptions{BotQuote: 100, ContextBytes: 40, Handler: func(c *airut.Ctx, _ airut.Chimed) error {
		var err error
		rc = c
		reply, err = c.Reply("hi")
		return err
	}})
	a.Text("question", airuttest.As(alice))
	j.answers <- yes(j.next(t).Batch[0])
	waitRan(t, ran)
	a.Text("next", airuttest.As(bob))
	j.next(t)
	j.answers <- no()
	if err := rc.Edit(reply, strings.Repeat("c", 100)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	a.Text("after", airuttest.As(bob))
	got = j.next(t)
	size := 0
	for _, q := range got.Context {
		size += len(q.Text)
	}
	if size > 40 {
		t.Errorf("the context after an edit is %d bytes, over ContextBytes: %q", size, quoted(got.Context))
	}
	j.answers <- no()
}
