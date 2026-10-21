package airut_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

type rollArgs struct {
	Dice    string `desc:"Dice to roll, like 2d6"`
	Times   *int   `desc:"How many times" min:"1" max:"10"`
	Private bool   `airut:",optional" desc:"Only you see the result"`
}

func roll(c *airut.Ctx, a rollArgs) error {
	times := 1
	if a.Times != nil {
		times = *a.Times
	}
	text := fmt.Sprintf("rolled %s x%d", a.Dice, times)
	if c.FromAgent() {
		text = "tool: " + text
	}
	if a.Private {
		_, err := c.ReplyPrivate(text)
		return err
	}
	_, err := c.Reply(text)
	return err
}

// openGate lets the fake adapter's callers start agent turns.
var openGate = airut.Gate{Access: map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"user-1", "dana"}}}}

// lockedBuffer is a log sink a test can read while the bot writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newBot(cfg airut.Config) (*airut.Bot, *lockedBuffer) {
	logs := &lockedBuffer{}
	cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	b := airut.New(cfg)
	b.Command("roll", "Roll dice", roll).AsTool()
	return b, logs
}

func texts(evs []airuttest.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind+":"+e.Text)
	}
	return out
}

func wantEvents(t *testing.T, got []airuttest.Event, want ...string) {
	t.Helper()
	if g := texts(got); !reflect.DeepEqual(g, want) {
		t.Errorf("events:\n got %q\nwant %q", g, want)
	}
}

func TestStructuredAndTextInvocations(t *testing.T) {
	b, _ := newBot(airut.Config{})
	a := airuttest.Start(t, b)

	wantEvents(t, a.Slash("roll", map[string]any{"dice": "2d6"}), "reply:rolled 2d6 x1")
	// A platform that types numbers sends float64; whole ones bind to int.
	wantEvents(t, a.Slash("roll", map[string]any{"dice": "2d6", "times": float64(3)}), "reply:rolled 2d6 x3")
	wantEvents(t, a.Text("!roll 2d6 times=3"), "reply:rolled 2d6 x3")
	wantEvents(t, a.Text("!roll 1d20 4"), "reply:rolled 1d20 x4")
	wantEvents(t, a.Text("just chatting about !roll"))

	evs := a.Text("!roll 2d6 --private")
	if len(evs) != 1 || !evs[0].Private {
		t.Errorf("private reply: %+v", evs)
	}
	evs = a.Slash("roll", map[string]any{"dice": "2d6", "private": true})
	if len(evs) != 1 || !evs[0].Private {
		t.Errorf("private structured reply: %+v", evs)
	}
}

func TestErrorsAreAnsweredInChat(t *testing.T) {
	b, logs := newBot(airut.Config{})
	b.Command("fail", "Fails", func(c *airut.Ctx) error { return errors.New("secret internal detail") })
	b.Command("user", "Refuses", func(c *airut.Ctx) error { return airut.Errorf("You cannot do that here.") })
	b.Command("boom", "Panics", func(c *airut.Ctx) error { panic("secret panic detail") })
	a := airuttest.Start(t, b)

	usage := "\nUsage: !roll <dice> [times] [private]"
	wantEvents(t, a.Text("!nope"), "reply:There is no command !nope.")
	wantEvents(t, a.Slash("secret-name", nil), "reply:There is no command !secret-name.")
	wantEvents(t, a.Text("!roll"), "reply:dice: is required"+usage)
	wantEvents(t, a.Text("!roll 2d6 times=0"), "reply:times: must be at least 1"+usage)
	wantEvents(t, a.Text("!roll 2d6 times=two"), "reply:times: must be a number"+usage)
	wantEvents(t, a.Text("!roll 2d6 1.5"), "reply:times: must be a whole number"+usage)
	wantEvents(t, a.Text("!roll 2d6 1 maybe"), "reply:private: must be true or false"+usage)
	wantEvents(t, a.Text("!roll 2d6 1 yes extra"), "reply:too many arguments"+usage)
	wantEvents(t, a.Text(`!roll "2d6`), "reply:a quote is not closed"+usage)
	wantEvents(t, a.Slash("roll", map[string]any{"dice": "2d6", "colour": "red"}), "reply:colour: is not a parameter of this command"+usage)
	wantEvents(t, a.Slash("roll", map[string]any{"dice": 7}), "reply:dice: must be text"+usage)
	wantEvents(t, a.Text("!fail now"), "reply:this command takes no arguments\nUsage: !fail")
	wantEvents(t, a.Text("!fail"), "reply:Something went wrong running fail.")
	wantEvents(t, a.Text("!user"), "reply:You cannot do that here.")
	wantEvents(t, a.Text("!boom"), "reply:Something went wrong running boom.")

	log := logs.String()
	for _, secret := range []string{"secret internal detail", "secret panic detail", "2d6", "colour", "nope", "secret-name"} {
		if strings.Contains(log, secret) {
			t.Errorf("the log holds chat or error text %q:\n%s", secret, log)
		}
	}
	for _, kind := range []string{"kind=handler", "kind=user", "kind=arguments", "command panicked", "unknown command"} {
		if !strings.Contains(log, kind) {
			t.Errorf("the log lacks %q:\n%s", kind, log)
		}
	}
}

func TestCtxSurface(t *testing.T) {
	b := airut.New(airut.Config{AskTimeout: 20 * time.Millisecond})
	type whoArgs struct {
		Who  airut.User        `desc:"Who"`
		Room *airut.Channel    `desc:"Where"`
		Role *airut.Role       `desc:"Role"`
		File *airut.Attachment `desc:"File"`
	}
	b.Command("who", "Who", func(c *airut.Ctx, a whoArgs) error {
		room, role, file := "-", "-", "-"
		if a.Room != nil {
			room = a.Room.ID
		}
		if a.Role != nil {
			role = a.Role.ID
		}
		if a.File != nil {
			file = a.File.Name
		}
		_, err := c.Replyf("%s %s %s %s | %s %s %s %v %s", a.Who.ID, room, role, file,
			c.Caller().ID, c.Channel().ID, c.Platform(), c.Native(), c.Command())
		return err
	})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		got, err := c.Ask("Sure?", "yes", "no")
		if err != nil {
			return err
		}
		_, err = c.Reply("answer " + got)
		return err
	})
	b.Command("thread", "Thread", func(c *airut.Ctx) error {
		th, err := c.Thread("talk")
		if err != nil {
			return err
		}
		ref, err := th.Reply("in thread")
		if err != nil {
			return err
		}
		if err := th.Edit(ref, "edited"); err != nil {
			return err
		}
		if err := th.React(c.Message(), "👍"); err != nil {
			return err
		}
		return th.Typing()
	})
	b.Command("files", "Files", func(c *airut.Ctx) error {
		_, err := c.Attach("a.txt", "text/plain", []byte("hi"))
		return err
	})
	a := airuttest.Start(t, b)

	evs := a.Text("!who @alice #dev &mods", airuttest.WithFiles(airut.Attachment{Name: "pic.png"}))
	wantEvents(t, evs, "reply:alice dev mods pic.png | user-1 channel-1 test native-inv-1 who")
	evs = a.Slash("who", map[string]any{"who": airut.User{ID: "bob"}}, airuttest.As(airut.User{Platform: "test", ID: "carol"}))
	wantEvents(t, evs, "reply:bob - - - | carol channel-1 test native-inv-2 who")
	wantEvents(t, a.Text("!who alice"), "reply:who: is not a user here\nUsage: !who <who> [room] [role] [file]")

	wantEvents(t, a.Text("!ask"), "ask:Sure?", "reply:No answer came in time, so nothing was done.")
	a.OnAsk(func(q airut.Ask) (string, error) {
		if q.Who.ID != "user-1" || q.Timeout != 20*time.Millisecond {
			return "", fmt.Errorf("ask %+v", q)
		}
		return q.Choices[0], nil
	})
	wantEvents(t, a.Text("!ask"), "ask:Sure?", "reply:answer yes")

	evs = a.Text("!thread")
	wantEvents(t, evs, "thread:talk", "reply:in thread", "edit:edited", "react:👍", "typing:")
	if evs[1].Thread != "talk" || evs[2].Ref != evs[1].Ref || evs[3].Ref != "msg-inv-6" {
		t.Errorf("thread events: %+v", evs)
	}

	evs = a.Text("!files")
	if len(evs) != 1 || !reflect.DeepEqual(evs[0].Files, []string{"a.txt"}) {
		t.Errorf("attach: %+v", evs)
	}
}

func TestNoAgent(t *testing.T) {
	b := airut.New(airut.Config{})
	var direct error
	b.Command("ask", "Ask the assistant", func(c *airut.Ctx) error {
		_, direct = c.Agent(airut.Turn{Prompt: "hi"}).Text()
		return c.Agent(airut.Turn{Prompt: "hi"}).StreamTo(c)
	})
	a := airuttest.Start(t, b)
	wantEvents(t, a.Text("!ask"), "reply:This bot has no agent configured, so it cannot run that command.")
	if !errors.Is(direct, airut.ErrNoAgent) || !strings.Contains(direct.Error(), "Config.Agent") {
		t.Errorf("Text() err = %v, want ErrNoAgent naming Config.Agent", direct)
	}
}

func TestAgentTurn(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{
		{Text: "Let me "},
		{Text: "roll. "},
		{Call: "roll", Args: `{"dice":"3d6","times":2}`},
		{Call: "secret"},
		{Call: "roll", Args: `{"dice":"3d6","times":99}`},
		{Text: "You got 11."},
	}}
	// With the keepalive off, typing shows at each tool call, as before
	// there was one.
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent, EditInterval: time.Nanosecond, TypingEvery: -1})
	b.Command("secret", "Not for the agent", func(c *airut.Ctx) error {
		t.Error("the agent reached a command its turn did not name")
		return nil
	}).AsTool()
	b.Command("ask", "Ask the assistant", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{
			Prompt:  "roll for me",
			Persona: "helper",
			Tools:   b.Tools("roll"),
			Session: airut.PerChannel,
			Budget:  airut.Budget{Turns: 4, Tokens: 1000},
		}).StreamTo(c)
	})
	a := airuttest.Start(t, b)

	evs := a.Text("!ask")
	wantEvents(t, evs,
		"reply:Let me ",
		"edit:Let me roll. ",
		"typing:",
		"typing:",
		"edit:Let me roll. You got 11.",
	)
	results := agent.Results()
	want := []string{
		"tool: rolled 3d6 x2",
		"refused: the turn does not name tool secret",
		"error: times: must be at most 10",
	}
	if !reflect.DeepEqual(results, want) {
		t.Errorf("tool results:\n got %q\nwant %q", results, want)
	}
	turns := agent.Turns()
	if len(turns) != 1 || turns[0].Persona != "helper" || turns[0].Budget.Turns != 4 || len(turns[0].Tools) != 1 {
		t.Errorf("turns: %+v", turns)
	}
}

func TestAgentToolSeesCallerAndCanAsk(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Call: "confirm"}, {Call: "nested"}}}
	b := airut.New(airut.Config{Gate: openGate, Agent: agent})
	b.Command("confirm", "Confirm", func(c *airut.Ctx) error {
		got, err := c.Ask("Go ahead?", "yes", "no")
		if err != nil {
			return err
		}
		ref, _ := c.Reply("first")
		_ = c.Edit(ref, "caller "+c.Caller().ID+" said "+got)
		_ = c.Typing()
		_ = c.React(ref, "x")
		if _, err := c.Thread("nope"); err == nil {
			t.Error("a tool opened a thread")
		}
		_, err = c.Attach("f.txt", "text/plain", nil)
		return err
	}).AsTool()
	b.Command("nested", "Nested", func(c *airut.Ctx) error {
		_, err := c.Agent(airut.Turn{}).Text()
		return err
	}).AsTool()
	b.Command("go", "Go", func(c *airut.Ctx) error {
		_, err := c.Agent(airut.Turn{Tools: b.Tools("confirm", "nested")}).Text()
		return err
	})
	a := airuttest.Start(t, b)
	a.OnAsk(func(q airut.Ask) (string, error) { return "yes", nil })

	evs := a.Text("!go", airuttest.As(airut.User{Platform: "test", ID: "dana"}))
	wantEvents(t, evs, "ask:Go ahead?")
	want := []string{
		"caller dana said yes\n[file f.txt was not shown to the model]",
		"error: airut: an agent turn cannot start another agent turn",
	}
	if got := agent.Results(); !reflect.DeepEqual(got, want) {
		t.Errorf("results:\n got %q\nwant %q", got, want)
	}
}

func TestSessionKeys(t *testing.T) {
	b := airut.New(airut.Config{})
	var keys []string
	b.Command("keys", "Keys", func(c *airut.Ctx) error {
		for _, s := range []airut.SessionScope{airut.NoSession, airut.PerChannel, airut.PerThread, airut.PerUser, airut.PerUserChannel} {
			keys = append(keys, c.SessionKey(s))
		}
		return nil
	})
	var ids []string
	b.Command("open", "Open a thread", func(c *airut.Ctx) error {
		th, err := c.Thread("t9")
		if err != nil {
			return err
		}
		ids = append(ids, c.ThreadID(), th.ThreadID())
		keys = append(keys, th.SessionKey(airut.PerThread))
		return nil
	})
	var later []string
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		later = append(later, c.SessionKey(airut.PerThread), c.ThreadID())
		return nil
	})
	a := airuttest.Start(t, b)
	a.Text("!keys")
	a.Text("!keys", airuttest.InThread("t9"))
	// In a direct channel every scope but NoSession is the one
	// conversation there, so nothing said in it resumes elsewhere.
	a.Text("!keys", airuttest.InDirect(), airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "dm-1"}))
	a.Text("!open")
	dm := "test/user/user-1/channel/dm-1"
	want := []string{
		"", "test/channel/channel-1", "test/channel/channel-1", "test/user/user-1", "test/user/user-1/channel/channel-1",
		"", "test/channel/channel-1", "test/thread/channel-1/t9", "test/user/user-1", "test/user/user-1/channel/channel-1",
		"", dm, dm, dm, dm,
		"test/thread/channel-1/t9",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys:\n got %q\nwant %q", keys, want)
	}
	// The thread a command opened is the one a later message in it is in,
	// with the same session.
	a.Text("more", airuttest.InThread("t9"))
	if !reflect.DeepEqual(ids, []string{"", "t9"}) || !reflect.DeepEqual(later, []string{"test/thread/channel-1/t9", "t9"}) {
		t.Errorf("thread ids %q, and a later message's key and id %q", ids, later)
	}
}

func TestSpecsForAdapters(t *testing.T) {
	b, _ := newBot(airut.Config{})
	b.Command("alpha", "First", func(*airut.Ctx) error { return nil })
	a := airuttest.Start(t, b)
	var names []string
	for _, s := range a.Specs() {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "roll"}) {
		t.Errorf("specs = %v", names)
	}
}

type failingAdapter struct{ platform airut.Platform }

func (f failingAdapter) Platform() airut.Platform { return f.platform }
func (f failingAdapter) Run(ctx context.Context, h airut.Host) error {
	return errors.New("cannot connect")
}

func TestRun(t *testing.T) {
	if err := airut.New(airut.Config{}).Run(context.Background()); err == nil {
		t.Error("Run with no adapters succeeded")
	}
	err := airut.New(airut.Config{}).Run(context.Background(), airuttest.New("x"), airuttest.New("x"))
	if err == nil || !strings.Contains(err.Error(), "two adapters for platform x") {
		t.Errorf("duplicate platforms: %v", err)
	}

	// One adapter failing stops the others and Run reports it.
	b := airut.New(airut.Config{Logger: slog.New(slog.DiscardHandler)})
	ok := airuttest.New("ok")
	err = b.Run(context.Background(), ok, failingAdapter{"bad"})
	if err == nil || !strings.Contains(err.Error(), "bad adapter: cannot connect") {
		t.Errorf("failing adapter: %v", err)
	}
	if err := b.Run(context.Background(), ok); err == nil || !strings.Contains(err.Error(), "Run called twice") {
		t.Errorf("second Run: %v", err)
	}

	// Commands cannot change once the bot runs.
	b = airut.New(airut.Config{})
	airuttest.Start(t, b)
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "registered after Run") {
			t.Errorf("late Command: %v", r)
		}
	}()
	b.Command("late", "Late", func(*airut.Ctx) error { return nil })
}

func TestAgentTextAndThrottle(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Text: "  "}, {Text: "one "}, {Text: "two "}, {Text: "three"}}}
	// An hour between edits: the first visible text posts at once, and
	// the throttle holds every later piece until the final flush.
	b := airut.New(airut.Config{Gate: openGate, Agent: agent, EditInterval: time.Hour})
	var collected string
	b.Command("text", "Text", func(c *airut.Ctx) error {
		var err error
		collected, err = c.Agent(airut.Turn{}).Text()
		return err
	})
	b.Command("stream", "Stream", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{}).StreamTo(c)
	})
	a := airuttest.Start(t, b)

	wantEvents(t, a.Text("!text"))
	if collected != "  one two three" {
		t.Errorf("Text() = %q", collected)
	}
	wantEvents(t, a.Text("!stream"), "reply:  one ", "edit:  one two three")

	agent.Err = errors.New("provider down")
	wantEvents(t, a.Text("!stream"), "reply:  one ", "edit:  one two three", "reply:Something went wrong running stream.")
}

func TestAccessors(t *testing.T) {
	b := airut.New(airut.Config{})
	c := b.Command("who", "Who are you", func(c *airut.Ctx) error {
		var roles []string
		for _, r := range c.Roles() {
			roles = append(roles, r.ID)
		}
		_, err := c.Replyf("%s in %s with %v; responder %v", c.Caller().ID, c.Channel().ID, roles, c.Responder() != nil)
		return err
	})
	if c.Name() != "who" || c.Description() != "Who are you" {
		t.Errorf("Name, Description = %q, %q", c.Name(), c.Description())
	}
	a := airuttest.Start(t, b)
	evs := a.Slash("who", nil, airuttest.In(airut.Channel{Platform: "test", ID: "lab"}), airuttest.WithRoles(airut.Role{ID: "mods"}))
	wantEvents(t, evs, "reply:user-1 in lab with [mods]; responder true")
	if got := a.Events(); len(got) != 1 {
		t.Errorf("Events() = %+v", got)
	}
	if airut.KindAttachment.String() != "attachment" || airut.Kind(99).String() != "Kind(99)" {
		t.Error("Kind.String")
	}
}

func TestEntityKindsMustMatch(t *testing.T) {
	b := airut.New(airut.Config{})
	type args struct {
		Who  airut.User     `desc:"Who"`
		Room *airut.Channel `desc:"Room"`
	}
	b.Command("who", "Who", func(c *airut.Ctx, a args) error {
		_, err := c.Reply(a.Who.ID)
		return err
	})
	a := airuttest.Start(t, b)
	usage := "\nUsage: !who <who> [room]"
	wantEvents(t, a.Slash("who", map[string]any{"who": airut.Channel{ID: "c"}}), "reply:who: must be a user"+usage)
	wantEvents(t, a.Slash("who", map[string]any{"who": airut.Role{ID: "r"}}), "reply:who: must be a user"+usage)
	wantEvents(t, a.Slash("who", map[string]any{"who": airut.User{ID: "u"}, "room": airut.User{ID: "u"}}), "reply:room: must be a channel"+usage)
	wantEvents(t, a.Text("!who #general"), "reply:who: is not a user here"+usage)
	wantEvents(t, a.Slash("who", map[string]any{"who": airut.User{ID: "u"}}), "reply:u")
}

func TestIntegerBounds(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{
		{Call: "big", Args: `{"n": 9007199254740992}`},
		{Call: "big", Args: `{"n": 9007199254740993}`},
		{Call: "big", Args: `{"n": -9007199254740993}`},
		{Call: "big", Args: `{"n": 1.5}`},
		{Call: "big", Args: `{"n": 2e3}`},
	}}
	b := airut.New(airut.Config{Gate: openGate, Agent: agent})
	type args struct {
		N int64 `desc:"N"`
	}
	b.Command("big", "Big", func(c *airut.Ctx, a args) error {
		_, err := c.Replyf("%d", a.N)
		return err
	}).AsTool()
	b.Command("go", "Go", func(c *airut.Ctx) error {
		_, err := c.Agent(airut.Turn{Tools: b.Tools("big")}).Text()
		return err
	})
	a := airuttest.Start(t, b)

	usage := "\nUsage: !big <n>"
	limit := "n: must be between -9007199254740992 and 9007199254740992"
	wantEvents(t, a.Text("!big 9007199254740992"), "reply:9007199254740992")
	wantEvents(t, a.Text("!big 9007199254740993"), "reply:"+limit+usage)
	wantEvents(t, a.Text("!big 99999999999999999999"), "reply:n: must be a whole number"+usage)
	wantEvents(t, a.Text("!big 2.5"), "reply:n: must be a whole number"+usage)
	wantEvents(t, a.Text("!big many"), "reply:n: must be a number"+usage)
	wantEvents(t, a.Slash("big", map[string]any{"n": int64(1 << 53)}), "reply:9007199254740992")
	wantEvents(t, a.Slash("big", map[string]any{"n": int64(1<<53 + 1)}), "reply:"+limit+usage)
	wantEvents(t, a.Slash("big", map[string]any{"n": float64(1 << 54)}), "reply:n: must be a whole number"+usage)

	a.Text("!go")
	want := []string{"9007199254740992", "error: " + limit, "error: " + limit, "error: n: must be a whole number", "error: n: must be a whole number"}
	if got := agent.Results(); !reflect.DeepEqual(got, want) {
		t.Errorf("tool results:\n got %q\nwant %q", got, want)
	}
}

func TestAttachmentsBindFromFiles(t *testing.T) {
	b := airut.New(airut.Config{})
	type args struct {
		File    airut.Attachment  `desc:"The file"`
		Caption string            `desc:"A caption"`
		Extra   *airut.Attachment `desc:"Another file"`
		Note    *string           `airut:",rest" desc:"A note"`
	}
	b.Command("upload", "Upload", func(c *airut.Ctx, a args) error {
		extra, note := "-", "-"
		if a.Extra != nil {
			extra = a.Extra.Name
		}
		if a.Note != nil {
			note = *a.Note
		}
		_, err := c.Replyf("%s %q %s %q", a.File.Name, a.Caption, extra, note)
		return err
	})
	a := airuttest.Start(t, b)
	pic, doc := airut.Attachment{Name: "pic.png"}, airut.Attachment{Name: "doc.pdf"}

	wantEvents(t, a.Text("!upload hello", airuttest.WithFiles(pic)), `reply:pic.png "hello" - "-"`)
	wantEvents(t, a.Text(`!upload "two words" and a "note`, airuttest.WithFiles(pic, doc)), `reply:pic.png "two words" doc.pdf "and a \"note"`)
	// An attachment's name is an ordinary word in text: files bind it.
	wantEvents(t, a.Text("!upload file=x", airuttest.WithFiles(pic)), `reply:pic.png "file=x" - "-"`)
	wantEvents(t, a.Text("!upload hello"), "reply:file: is required\nUsage: !upload <file> <caption> [extra] [note...]")
}

func TestToolArgumentsMustBeOneObject(t *testing.T) {
	ran := 0
	b := airut.New(airut.Config{})
	type args struct {
		N *int `desc:"N"`
	}
	b.Command("count", "Count", func(c *airut.Ctx, a args) error {
		ran++
		return nil
	}).AsTool()
	var results []string
	b.Command("go", "Go", func(c *airut.Ctx) error {
		tool := b.Tools("count")[0]
		for _, payload := range []string{
			`{"n":1} garbage`, `{"n":1}{"n":2}`, `{"n":1} {`, `null`, `[]`, `"x"`, `{"n":`,
		} {
			_, err := tool.Call(c, []byte(payload))
			results = append(results, fmt.Sprint(err))
		}
		for _, payload := range []string{``, `  `, `{}`, ` {"n":1} `} {
			if _, err := tool.Call(c, []byte(payload)); err != nil {
				t.Errorf("Call(%q) = %v", payload, err)
			}
		}
		return nil
	})
	a := airuttest.Start(t, b)
	a.Text("!go")
	for i, r := range results {
		if r != "the arguments are not a JSON object" {
			t.Errorf("malformed payload %d: err = %s", i, r)
		}
	}
	if ran != 4 {
		t.Errorf("the handler ran %d times, want 4: only the well-formed payloads", ran)
	}
}

// forgetter is a scripted agent that can forget sessions.
type forgetter struct {
	airuttest.Agent
	mu   sync.Mutex
	keys []string
	err  error
}

func (f *forgetter) ForgetSession(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	return f.err
}

func (f *forgetter) forgotten() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func TestForgetCommand(t *testing.T) {
	ag := &forgetter{}
	b, _ := newBot(airut.Config{Gate: openGate, Agent: ag})
	a := airuttest.Start(t, b)
	var names []string
	for _, s := range a.Specs() {
		names = append(names, s.Name)
	}
	// The Gate's limits are settings on a bot with an Agent, so it has
	// the settings commands too.
	if !reflect.DeepEqual(names, []string{"forget", "roll", "set", "settings", "unset"}) {
		t.Fatalf("specs = %v; an agent that can forget brings a forget command", names)
	}

	evs := a.Text("!forget", airuttest.InThread("t1"))
	wantEvents(t, evs, "reply:The agent has forgotten your conversations with it.")
	if len(evs) == 1 && !evs[0].Private {
		t.Error("the answer to forget was not private")
	}
	// Only the caller's own session, whatever channel or thread they ask in.
	if got := ag.forgotten(); !reflect.DeepEqual(got, []string{"test/user/user-1"}) {
		t.Fatalf("forgot %q, want only the caller's own session", got)
	}

	ag.err = errors.New("disk on fire")
	if evs := a.Text("!forget"); len(evs) != 1 || strings.Contains(evs[0].Text, "disk on fire") {
		t.Fatalf("a failed forget answered %q; its error text must not reach the chat", texts(evs))
	}
}

// prefixForgetter can also forget every session under a prefix.
type prefixForgetter struct {
	forgetter
	prefixes []string
}

func (f *prefixForgetter) ForgetSessionsWithPrefix(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefixes = append(f.prefixes, prefix)
	return f.err
}

func TestForgetCommandForms(t *testing.T) {
	ag := &prefixForgetter{}
	var admit bool
	b, _ := newBot(airut.Config{Gate: openGate, Agent: ag, ForgetShared: func(*airut.Ctx) bool { return admit }})
	a := airuttest.Start(t, b)

	// Plain forget clears the caller's conversations in every channel
	// too, and only theirs: the slash keeps user-10 out of user-1's.
	a.Text("!forget")
	if got := ag.forgotten(); !reflect.DeepEqual(got, []string{"test/user/user-1"}) || !reflect.DeepEqual(ag.prefixes, []string{"test/user/user-1/"}) {
		t.Fatalf("forgot %q and prefixes %q", got, ag.prefixes)
	}

	// A shared conversation needs ForgetShared's say-so.
	ag.keys = nil
	for _, line := range []string{"!forget channel", "!forget thread"} {
		evs := a.Text(line, airuttest.InThread("t1"))
		if len(evs) != 1 || !strings.Contains(evs[0].Text, "may not") {
			t.Fatalf("%s by someone not admitted answered %q", line, texts(evs))
		}
	}
	if len(ag.forgotten()) != 0 {
		t.Fatalf("forgot %q without ForgetShared's say-so", ag.forgotten())
	}
	admit = true
	wantEvents(t, a.Text("!forget channel", airuttest.InThread("t1")), "reply:The agent has forgotten this channel's conversation.")
	wantEvents(t, a.Text("!forget thread", airuttest.InThread("t1")), "reply:The agent has forgotten this thread's conversation.")
	if got := ag.forgotten(); !reflect.DeepEqual(got, []string{"test/channel/channel-1", "test/thread/channel-1/t1"}) {
		t.Fatalf("forgot %q", got)
	}
	if evs := a.Text("!forget thread"); len(evs) != 1 || !strings.Contains(evs[0].Text, "not a thread") {
		t.Fatalf("forget thread outside a thread answered %q", texts(evs))
	}

	// From a direct channel too, plain forget clears the conversation
	// the caller has everywhere, not only the direct channel's.
	ag.keys, ag.prefixes = nil, nil
	a.Text("!forget", airuttest.InDirect(), airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "dm-1"}))
	if got := ag.forgotten(); !reflect.DeepEqual(got, []string{"test/user/user-1"}) || !reflect.DeepEqual(ag.prefixes, []string{"test/user/user-1/"}) {
		t.Fatalf("forget in a direct channel forgot %q and prefixes %q", got, ag.prefixes)
	}

	// Without ForgetShared nobody may.
	b, _ = newBot(airut.Config{Gate: openGate, Agent: &forgetter{}})
	if evs := airuttest.Start(t, b).Text("!forget channel"); len(evs) != 1 || !strings.Contains(evs[0].Text, "may not") {
		t.Fatalf("forget channel with no ForgetShared answered %q", texts(evs))
	}
}

func TestForgetCommandIsOptional(t *testing.T) {
	t.Run("an agent that cannot forget brings none", func(t *testing.T) {
		b, _ := newBot(airut.Config{Gate: openGate, Agent: &airuttest.Agent{}})
		for _, s := range airuttest.Start(t, b).Specs() {
			if s.Name == "forget" {
				t.Fatal("a forget command was added for an agent that cannot forget")
			}
		}
	})
	t.Run("the bot's own forget wins", func(t *testing.T) {
		ag := &forgetter{}
		b, _ := newBot(airut.Config{Gate: openGate, Agent: ag})
		b.Command("forget", "Forget a reminder", func(c *airut.Ctx) error {
			_, err := c.Reply("the bot's own")
			return err
		})
		a := airuttest.Start(t, b)
		wantEvents(t, a.Text("!forget"), "reply:the bot's own")
		if len(ag.forgotten()) != 0 {
			t.Fatal("the built-in ran in place of the bot's own command")
		}
	})
}
