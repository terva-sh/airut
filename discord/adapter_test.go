package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
	"terva.sh/airut/config"
)

// hostAdapter hands a running Bot's Host to a test, which drives the
// Discord session itself.
type hostAdapter struct{ got chan airut.Host }

func (hostAdapter) Platform() airut.Platform { return airut.Discord }
func (a hostAdapter) Run(ctx context.Context, h airut.Host) error {
	a.got <- h
	<-ctx.Done()
	return nil
}

// harness is a session on a fake Discord, serving b.
type harness struct {
	f   *fakeDiscord
	s   *session
	now time.Time
	mu  sync.Mutex
	err error
	// leaked records follow-ups Discord would have shown in the deferred
	// response's place, keeping its visibility rather than theirs.
	leaked []string
}

func start(t *testing.T, b *airut.Bot) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := hostAdapter{make(chan airut.Host, 1)}
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, a) }()
	h := <-a.got
	x := &harness{f: newFakeDiscord(), now: time.Unix(1_800_000_000, 0)}
	x.s = newSession(ctx, x.f.session(t), h, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func(err error) {
		x.mu.Lock()
		x.err = err
		x.mu.Unlock()
	})
	x.s.now = func() time.Time {
		x.mu.Lock()
		defer x.mu.Unlock()
		return x.now
	}
	// Model Discord's rule that the first follow-up after a deferral,
	// while the deferred response still shows its placeholder, edits
	// that response instead of posting a message of its own.
	deferred, resolved := false, false
	x.f.reply("POST", "/interactions/i1/tok/callback", func(string) fakeReply {
		deferred = true
		return fakeReply{204, ""}
	})
	x.f.reply("PATCH", "/webhooks/42/tok/messages/@original", func(string) fakeReply {
		resolved = true
		return fakeReply{200, `{"id": "orig", "channel_id": "c1"}`}
	})
	x.f.reply("POST", "/webhooks/42/tok", func(body string) fakeReply {
		if deferred && !resolved {
			x.leaked = append(x.leaked, body)
		}
		x.f.next++
		return fakeReply{200, fmt.Sprintf(`{"id": "m%d", "channel_id": "c1"}`, x.f.next)}
	})
	x.f.on("DELETE", "/webhooks/42/tok/messages/*", 204, "")
	x.f.messages("PATCH", "/webhooks/42/tok/messages/*")
	x.f.messages("POST", "/channels/c1/messages")
	t.Cleanup(func() {
		x.s.stop()
		cancel()
		<-done
		if len(x.leaked) > 0 {
			t.Errorf("follow-ups Discord would have shown as the deferred response: %v", x.leaked)
		}
	})
	return x
}

func (x *harness) advance(d time.Duration) {
	x.mu.Lock()
	x.now = x.now.Add(d)
	x.mu.Unlock()
}

// call delivers an application command interaction and waits for every
// goroutine it started.
func (x *harness) call(name string, opts []*discordgo.ApplicationCommandInteractionDataOption, res *discordgo.ApplicationCommandInteractionDataResolved) {
	x.s.onInteraction(x.s.dg, interaction(name, opts, res))
	x.s.stop()
	x.s.mu.Lock()
	x.s.stopped = false
	x.s.mu.Unlock()
}

func interaction(name string, opts []*discordgo.ApplicationCommandInteractionDataOption, res *discordgo.ApplicationCommandInteractionDataResolved) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i1", AppID: "42", Token: "tok", Type: discordgo.InteractionApplicationCommand,
		GuildID: "g1", ChannelID: "c1",
		Member: &discordgo.Member{User: &discordgo.User{ID: "u1", Username: "ada"}, Nick: "Ada", Roles: []string{"r1"}},
		Data:   discordgo.ApplicationCommandInteractionData{ID: "cmd", Name: name, Options: opts, Resolved: res},
	}}
}

// paths lists the requests the fake received, as "METHOD path".
func (x *harness) paths() []string {
	var out []string
	for _, r := range x.f.sent() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func same(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

func TestDefersBeforeTheHandlerRuns(t *testing.T) {
	b := airut.New(airut.Config{})
	release := make(chan struct{})
	b.Command("slow", "Take a while", func(c *airut.Ctx) error {
		<-release
		_, err := c.Reply("done")
		return err
	})
	x := start(t, b)
	go x.s.onInteraction(x.s.dg, interaction("slow", nil, nil))
	r := x.f.waitFor(t, "POST", "/interactions/i1/tok/callback")
	if !strings.Contains(r.Body, `"type":5`) {
		t.Errorf("acknowledged with %s, want a deferred response", r.Body)
	}
	close(release)
	r = x.f.waitFor(t, "PATCH", "/webhooks/42/tok/messages/@original")
	if !strings.Contains(r.Body, `"content":"done"`) {
		t.Errorf("filled the response with %s", r.Body)
	}
}

func TestRepliesFillTheResponseThenFollowUp(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("talk", "Talk", func(c *airut.Ctx) error {
		if _, err := c.Reply("one"); err != nil {
			return err
		}
		if _, err := c.ReplyPrivate("secret"); err != nil {
			return err
		}
		_, err := c.Reply("two @everyone")
		return err
	})
	x := start(t, b)
	x.call("talk", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		"PATCH /webhooks/42/tok/messages/@original",
		"POST /webhooks/42/tok",
		"POST /webhooks/42/tok",
	}
	if got := x.paths(); !same(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	sent := x.f.sent()
	if b := sent[2].Body; !strings.Contains(b, `"flags":64`) || !strings.Contains(b, "secret") {
		t.Errorf("private reply = %s, want an ephemeral follow-up", b)
	}
	if b := sent[3].Body; strings.Contains(b, `"flags"`) || !strings.Contains(b, `"allowed_mentions":{"parse":[],"replied_user":false}`) {
		t.Errorf("second reply = %s, want a public follow-up that pings nobody", b)
	}
}

func TestAnAgentTurnStreamsIntoTheDeferredResponse(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{
		{Text: "Rolling. "},
		{Call: "roll", Args: `{}`},
		{Text: "You got 4."},
	}}
	gate := airut.Gate{Access: map[airut.Platform]airut.Access{airut.Discord: {Users: []string{"u1"}}}}
	b := airut.New(airut.Config{Agent: agent, Gate: gate, EditInterval: time.Nanosecond})
	b.Command("roll", "Roll a die", func(c *airut.Ctx) error {
		_, err := c.Reply("4")
		return err
	}).AsTool()
	b.Command("ask", "Ask the assistant", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "roll for me", Tools: b.Tools("roll")}).StreamTo(c)
	})
	x := start(t, b)
	x.f.on("POST", "/channels/c1/typing", 204, "")
	x.call("ask", nil, nil)
	sent := x.f.sent()
	if len(sent) == 0 || sent[0].Path != "/interactions/i1/tok/callback" || !strings.Contains(sent[0].Body, `"type":5`) {
		t.Fatalf("requests = %v, want a deferral first", x.paths())
	}
	var last string
	for _, r := range sent[1:] {
		switch {
		case r.Method == "PATCH" && strings.HasPrefix(r.Path, "/webhooks/42/tok/messages/"):
			last = r.Body
		case r.Path == "/channels/c1/typing":
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.Path)
		}
	}
	if !strings.Contains(last, `"content":"Rolling. You got 4."`) {
		t.Errorf("the answer ended as %s", last)
	}
	if got := agent.Results(); len(got) != 1 || !strings.Contains(got[0], "4") {
		t.Errorf("tool results = %q", got)
	}
}

func TestPrivateOnlyRemovesThePlaceholder(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("whisper", "Whisper", func(c *airut.Ctx) error {
		_, err := c.ReplyPrivate("psst")
		return err
	})
	x := start(t, b)
	x.call("whisper", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		// The placeholder is replaced first, so the follow-up is a
		// message of its own and stays ephemeral.
		"PATCH /webhooks/42/tok/messages/@original",
		"POST /webhooks/42/tok",
		"DELETE /webhooks/42/tok/messages/@original",
	}
	if got := x.paths(); !same(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	sent := x.f.sent()
	if b := sent[1].Body; strings.Contains(b, "psst") || !strings.Contains(b, privateNoteText) {
		t.Errorf("the deferred response became %s", b)
	}
}

func TestPrivateThenPublic(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("both", "Whisper, then speak", func(c *airut.Ctx) error {
		if _, err := c.ReplyPrivate("psst"); err != nil {
			return err
		}
		_, err := c.Reply("hello all")
		return err
	})
	x := start(t, b)
	x.call("both", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		"PATCH /webhooks/42/tok/messages/@original",
		"POST /webhooks/42/tok",
		// The public reply replaces the note, and stays.
		"PATCH /webhooks/42/tok/messages/@original",
	}
	if got := x.paths(); !same(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func TestAFailedFirstReplyLeavesTheResponse(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("retry", "Fail once", func(c *airut.Ctx) error {
		if _, err := c.Reply("first try"); err == nil {
			return errors.New("the first reply was meant to fail")
		}
		_, err := c.Reply("second try")
		return err
	})
	x := start(t, b)
	tries := 0
	x.f.reply("PATCH", "/webhooks/42/tok/messages/@original", func(string) fakeReply {
		if tries++; tries == 1 {
			return fakeReply{400, `{"message": "Invalid Form Body", "code": 50035}`}
		}
		return fakeReply{200, `{"id": "orig"}`}
	})
	x.call("retry", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		"PATCH /webhooks/42/tok/messages/@original",
		"PATCH /webhooks/42/tok/messages/@original",
	}
	if got := x.paths(); !same(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func TestAGrowingEditThatFailsKeepsWhatItPosted(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("grow", "Grow, fail, shrink", func(c *airut.Ctx) error {
		ref, err := c.Reply("short")
		if err != nil {
			return err
		}
		if err := c.Edit(ref, strings.Repeat("a", 4500)); err == nil {
			return errors.New("the growing edit was meant to fail")
		}
		return c.Edit(ref, "short again")
	})
	x := start(t, b)
	posts := 0
	x.f.reply("POST", "/webhooks/42/tok", func(string) fakeReply {
		if posts++; posts == 2 {
			return fakeReply{500, `{"message": "Internal Server Error", "code": 0}`}
		}
		return fakeReply{200, `{"id": "m1"}`}
	})
	x.call("grow", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		"PATCH /webhooks/42/tok/messages/@original",
		"PATCH /webhooks/42/tok/messages/@original",
		"POST /webhooks/42/tok",
		"POST /webhooks/42/tok",
		"PATCH /webhooks/42/tok/messages/@original",
		// The part posted before the failure is still known, and goes.
		"DELETE /webhooks/42/tok/messages/m1",
	}
	if got := x.paths(); !same(got, want) {
		t.Errorf("requests =\n%v\nwant\n%v", got, want)
	}
}

func TestOptionsArriveTyped(t *testing.T) {
	b := airut.New(airut.Config{})
	got := make(chan everyKind, 1)
	var caller airut.User
	var roles []airut.Role
	b.Command("every", "Take every kind", func(c *airut.Ctx, a everyKind) error {
		caller, roles = c.Caller(), c.Roles()
		got <- a
		return nil
	})
	x := start(t, b)
	opt := func(name string, typ discordgo.ApplicationCommandOptionType, v any) *discordgo.ApplicationCommandInteractionDataOption {
		return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: typ, Value: v}
	}
	x.call("every", []*discordgo.ApplicationCommandInteractionDataOption{
		opt("mode", discordgo.ApplicationCommandOptionString, "fast"),
		opt("count", discordgo.ApplicationCommandOptionInteger, float64(7)),
		opt("scale", discordgo.ApplicationCommandOptionNumber, -1.5),
		opt("loud", discordgo.ApplicationCommandOptionBoolean, true),
		opt("who", discordgo.ApplicationCommandOptionUser, "u2"),
		opt("where", discordgo.ApplicationCommandOptionChannel, "c2"),
		opt("group", discordgo.ApplicationCommandOptionRole, "r2"),
		opt("image", discordgo.ApplicationCommandOptionAttachment, "a1"),
	}, &discordgo.ApplicationCommandInteractionDataResolved{
		Users:       map[string]*discordgo.User{"u2": {ID: "u2", Username: "grace", GlobalName: "Grace"}},
		Members:     map[string]*discordgo.Member{"u2": {Nick: "Admiral"}},
		Channels:    map[string]*discordgo.Channel{"c2": {ID: "c2", Name: "general"}},
		Roles:       map[string]*discordgo.Role{"r2": {ID: "r2", Name: "mods"}},
		Attachments: map[string]*discordgo.MessageAttachment{"a1": {ID: "a1", Filename: "cat.png", ContentType: "image/png", Size: 1234, URL: "https://cdn.example.com/cat.png"}},
	})
	a := <-got
	want := everyKind{
		Mode: "fast", Count: 7, Scale: -1.5, Loud: true,
		Who:   airut.User{Platform: airut.Discord, ID: "u2", Name: "Admiral"},
		Where: airut.Channel{Platform: airut.Discord, ID: "c2", Name: "general"},
		Group: airut.Role{Platform: airut.Discord, ID: "r2", Name: "mods"},
		Image: airut.Attachment{Name: "cat.png", ContentType: "image/png", Size: 1234, URL: "https://cdn.example.com/cat.png"},
	}
	if a != want {
		t.Errorf("arguments = %+v\nwant %+v", a, want)
	}
	if caller != (airut.User{Platform: airut.Discord, ID: "u1", Name: "Ada"}) {
		t.Errorf("caller = %+v", caller)
	}
	if len(roles) != 1 || roles[0].ID != "r1" {
		t.Errorf("roles = %+v", roles)
	}
}

func TestLongTextSpillsAndEditsFollowIt(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("long", "Say a lot", func(c *airut.Ctx) error {
		ref, err := c.Reply(strings.Repeat("a", 4500))
		if err != nil {
			return err
		}
		if err := c.Edit(ref, "short now"); err != nil {
			return err
		}
		return c.Edit(ref, strings.Repeat("b", 2500))
	})
	x := start(t, b)
	x.call("long", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		// 4500 characters are three messages.
		"PATCH /webhooks/42/tok/messages/@original",
		"POST /webhooks/42/tok",
		"POST /webhooks/42/tok",
		// Shrinking removes the two follow-ups.
		"PATCH /webhooks/42/tok/messages/@original",
		"DELETE /webhooks/42/tok/messages/m1",
		"DELETE /webhooks/42/tok/messages/m2",
		// Growing again posts one.
		"PATCH /webhooks/42/tok/messages/@original",
		"POST /webhooks/42/tok",
	}
	if got := x.paths(); !same(got, want) {
		t.Errorf("requests =\n%v\nwant\n%v", got, want)
	}
	for _, r := range x.f.sent()[1:] {
		if r.Method != "DELETE" && !strings.Contains(r.Body, `"parse":[]`) {
			t.Errorf("%s %s lets the text ping: %.80s", r.Method, r.Path, r.Body)
		}
	}
}

func TestAfterTheTokenExpires(t *testing.T) {
	b := airut.New(airut.Config{})
	var x *harness
	b.Command("late", "Answer late", func(c *airut.Ctx) error {
		x.advance(20 * time.Minute)
		if _, err := c.Reply("public"); err != nil {
			return err
		}
		_, err := c.ReplyPrivate("private")
		return err
	})
	x = start(t, b)
	x.f.on("POST", "/users/@me/channels", 200, `{"id": "dm1", "type": 1}`)
	x.f.messages("POST", "/channels/dm1/messages")
	x.call("late", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		"POST /channels/c1/messages",
		"POST /users/@me/channels",
		"POST /channels/dm1/messages",
	}
	if got := x.paths(); !same(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func TestReplyToGoesToTheChannel(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("answer", "Answer a message", func(c *airut.Ctx) error {
		_, err := c.Send(airut.Message{Text: "re", ReplyTo: airut.MessageRef{ID: "m9"}})
		return err
	})
	x := start(t, b)
	x.call("answer", nil, nil)
	r := x.f.waitFor(t, "POST", "/channels/c1/messages")
	if !strings.Contains(r.Body, `"message_reference":{"message_id":"m9","channel_id":"c1","fail_if_not_exists":false}`) || !strings.Contains(r.Body, `"parse":[]`) {
		t.Errorf("reply = %s", r.Body)
	}
}

func TestReadyRegistersTheCommands(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("ping", "Answer pong", func(*airut.Ctx) error { return nil })
	x := start(t, b)
	x.f.on("PUT", "/applications/42/commands", 200, "[]")
	x.s.onReady(x.s.dg, &discordgo.Ready{Application: &discordgo.Application{ID: "42"}})
	r := x.f.waitFor(t, "PUT", "/applications/42/commands")
	if !strings.Contains(r.Body, `"name":"ping"`) {
		t.Errorf("registered %s", r.Body)
	}
	x.s.stop()
	if x.err != nil {
		t.Errorf("fail called with %v", x.err)
	}
}

func TestRegistrationFailureStopsRun(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("PUT", "/applications/42/commands", 403, `{"message": "Missing Access", "code": 50001}`)
	x.s.onReady(x.s.dg, &discordgo.Ready{Application: &discordgo.Application{ID: "42"}})
	x.s.stop()
	if x.err == nil {
		t.Error("a failed registration did not stop Run")
	}
	if k := errKind(x.err); k != "discord_50001" {
		t.Errorf("error kind = %q", k)
	}
}

func TestFrom(t *testing.T) {
	b := airut.New(airut.Config{})
	var ok bool
	var ic *discordgo.InteractionCreate
	b.Command("native", "Reach the platform", func(c *airut.Ctx) error {
		_, ic, ok = From(c)
		return nil
	})
	x := start(t, b)
	x.call("native", nil, nil)
	if !ok || ic == nil || ic.ID != "i1" {
		t.Errorf("From = %v, %v", ic, ok)
	}
}

func TestRunNeedsAToken(t *testing.T) {
	err := New(&config.Discord{}, Options{}).Run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %v", err)
	}
}

func TestSplit(t *testing.T) {
	line := strings.Repeat("x", 1500) + "\n"
	for _, c := range []struct {
		name string
		in   string
		want []int // the length of each piece
	}{
		{"short", "hello", []int{5}},
		{"empty", "", []int{0}},
		{"exact", strings.Repeat("x", 2000), []int{2000}},
		{"at a line", line + line, []int{1501, 1501}},
		{"at a space", strings.Repeat("y ", 1500), []int{2000, 1000}},
		{"hard", strings.Repeat("z", 4001), []int{2000, 2000, 1}},
		// An emoji is two UTF-16 units, so 1000 fill a message.
		{"emoji", strings.Repeat("😀", 1001), []int{4000, 4}},
		// Each invalid byte is one unit and one byte.
		{"invalid", strings.Repeat("\xff", 2001), []int{2000, 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []int
			for _, p := range split(c.in) {
				got = append(got, len(p))
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("piece lengths = %v, want %v", got, c.want)
			}
			if strings.Join(split(c.in), "") != c.in {
				t.Error("the pieces do not rejoin to the text")
			}
		})
	}
}

func TestMentioned(t *testing.T) {
	for _, c := range []struct {
		kind  airut.Kind
		token string
		id    string
		ok    bool
	}{
		{airut.KindUser, "<@123>", "123", true},
		{airut.KindUser, "<@!123>", "123", true},
		{airut.KindUser, "123", "123", true},
		{airut.KindUser, "<#123>", "", false},
		{airut.KindChannel, "<#456>", "456", true},
		{airut.KindRole, "<@&789>", "789", true},
		{airut.KindRole, "mods", "", false},
		{airut.KindString, "123", "", false},
	} {
		id, ok := mentioned(c.kind, c.token)
		if ok != c.ok || (ok && id != c.id) {
			t.Errorf("mentioned(%s, %q) = %q, %v", c.kind, c.token, id, ok)
		}
	}
}

func TestResolveUserInTheGuild(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("GET", "/guilds/g1/members/123", 200, `{"user": {"id": "123", "username": "linus"}, "nick": "Linus"}`)
	got, err := x.s.resolve(context.Background(), "g1", airut.KindUser, "<@123>")
	if err != nil || got != (airut.User{Platform: airut.Discord, ID: "123", Name: "Linus"}) {
		t.Errorf("resolve = %+v, %v", got, err)
	}
	if _, err := x.s.resolve(context.Background(), "g1", airut.KindUser, "nobody"); err == nil {
		t.Error("resolved a name that is not a mention or an id")
	}
}

func TestReactionEmoji(t *testing.T) {
	for in, want := range map[string]string{"👍": "👍", "<:blob:123>": "blob:123", "<a:dance:456>": "dance:456", "blob:123": "blob:123"} {
		if got := reactionEmoji(in); got != want {
			t.Errorf("reactionEmoji(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrKindNeverCarriesText(t *testing.T) {
	if k := errKind(errors.New("the message said something private")); k != "other" {
		t.Errorf("errKind = %q", k)
	}
}

func TestAPrivateEditNeverSpillsIntoTheChannel(t *testing.T) {
	b := airut.New(airut.Config{})
	var x *harness
	b.Command("secret", "Say a lot privately", func(c *airut.Ctx) error {
		ref, err := c.ReplyPrivate("short")
		if err != nil {
			return err
		}
		return c.Edit(ref, strings.Repeat("s", 4500))
	})
	x = start(t, b)
	// The token runs out between rewriting the ephemeral message and
	// posting what no longer fits in it.
	x.f.reply("PATCH", "/webhooks/42/tok/messages/m1", func(string) fakeReply {
		x.advance(20 * time.Minute)
		return fakeReply{200, `{"id": "m1"}`}
	})
	x.f.on("POST", "/users/@me/channels", 200, `{"id": "dm1", "type": 1}`)
	x.f.messages("POST", "/channels/dm1/messages")
	x.call("secret", nil, nil)
	want := []string{
		"POST /interactions/i1/tok/callback",
		"PATCH /webhooks/42/tok/messages/@original",
		"POST /webhooks/42/tok",
		"PATCH /webhooks/42/tok/messages/m1",
		"POST /users/@me/channels",
		"POST /channels/dm1/messages",
		"POST /users/@me/channels",
		"POST /channels/dm1/messages",
	}
	if got := x.paths(); !same(got, want) {
		t.Errorf("requests =\n%v\nwant\n%v", got, want)
	}
}

func TestInvalidUTF8IsSentValid(t *testing.T) {
	b := airut.New(airut.Config{})
	b.Command("bytes", "Send bad bytes", func(c *airut.Ctx) error {
		_, err := c.Reply("ok \xff")
		return err
	})
	x := start(t, b)
	x.call("bytes", nil, nil)
	r := x.f.waitFor(t, "PATCH", "/webhooks/42/tok/messages/@original")
	if !strings.Contains(r.Body, "\"content\":\"ok \uFFFD\"") {
		t.Errorf("sent %s", r.Body)
	}
}

func TestAReplyThatOutlivesTheToken(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprint("private=", private), func(t *testing.T) {
			b := airut.New(airut.Config{})
			b.Command("long", "Say a lot", func(c *airut.Ctx) error {
				_, err := c.Send(airut.Message{Text: strings.Repeat("a", 4500), Private: private})
				return err
			})
			x := start(t, b)
			// The token runs out once the first part is through.
			first := "PATCH /webhooks/42/tok/messages/@original"
			x.f.reply("PATCH", "/webhooks/42/tok/messages/@original", func(string) fakeReply {
				x.advance(20 * time.Minute)
				return fakeReply{200, `{"id": "orig"}`}
			})
			x.f.reply("POST", "/webhooks/42/tok", func(string) fakeReply {
				x.advance(20 * time.Minute)
				return fakeReply{200, `{"id": "m1"}`}
			})
			x.f.on("POST", "/users/@me/channels", 200, `{"id": "dm1", "type": 1}`)
			x.f.messages("POST", "/channels/dm1/messages")
			x.call("long", nil, nil)
			want := []string{"POST /interactions/i1/tok/callback", first, "POST /channels/c1/messages", "POST /channels/c1/messages"}
			if private {
				want = []string{
					"POST /interactions/i1/tok/callback",
					// The note, then the first part as an ephemeral follow-up.
					"PATCH /webhooks/42/tok/messages/@original",
					"POST /webhooks/42/tok",
					"POST /users/@me/channels",
					"POST /channels/dm1/messages",
					"POST /channels/dm1/messages",
				}
			}
			if got := x.paths(); !same(got, want) {
				t.Errorf("requests =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

func TestTheTokenRunsOutBeforeTheFirstPart(t *testing.T) {
	b := airut.New(airut.Config{})
	errs := make(chan error, 1)
	var x *harness
	b.Command("whisper", "Whisper", func(c *airut.Ctx) error {
		// Live for the reply's first look at the clock, then expired.
		calls := 0
		x.s.now = func() time.Time {
			if calls++; calls == 1 {
				return x.now
			}
			return x.now.Add(time.Hour)
		}
		_, err := c.ReplyPrivate("psst")
		errs <- err
		return nil
	})
	x = start(t, b)
	x.f.on("POST", "/users/@me/channels", 403, `{"message": "Cannot send messages to this user", "code": 50007}`)
	x.call("whisper", nil, nil)
	if err := <-errs; errKind(err) != "discord_50007" {
		t.Errorf("err = %v", err)
	}
}
