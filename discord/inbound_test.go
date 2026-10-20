package discord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

func message(id, author, text string) *discordgo.Message {
	return &discordgo.Message{
		ID: id, ChannelID: "c1", GuildID: "g1", Content: text, Type: discordgo.MessageTypeDefault,
		Author:    &discordgo.User{ID: author, Username: "ada"},
		Member:    &discordgo.Member{Nick: "Ada", Roles: []string{"r1"}},
		Timestamp: time.Unix(1_800_000_000, 0).UTC(),
	}
}

func TestMessagesReachOnMessage(t *testing.T) {
	b := airut.New(airut.Config{})
	got := make(chan airut.MessageEvent, 4)
	b.OnMessage(func(c *airut.Ctx, ev airut.MessageEvent) error {
		got <- ev
		_, err := c.Reply("hi")
		return err
	})
	x := start(t, b)
	x.s.me = "bot"
	m := message("m7", "u1", "<@bot> hello there")
	m.Mentions = []*discordgo.User{{ID: "bot"}}
	m.Attachments = []*discordgo.MessageAttachment{{Filename: "cat.png", ContentType: "image/png", Size: 9, URL: "https://cdn.discordapp.com/attachments/1/2/cat.png"}}
	x.s.onMessage(x.s.dg, &discordgo.MessageCreate{Message: m})
	// Neither the bot's own message nor a system message is delivered.
	x.s.onMessage(x.s.dg, &discordgo.MessageCreate{Message: message("m8", "bot", "mine")})
	join := message("m9", "u2", "")
	join.Type = discordgo.MessageTypeGuildMemberJoin
	x.s.onMessage(x.s.dg, &discordgo.MessageCreate{Message: join})
	x.s.stop()
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(got))
	}
	ev := <-got
	if ev.ID != "m7" || ev.Message.ID != "m7" || ev.Sender != (airut.User{Platform: airut.Discord, ID: "u1", Name: "Ada"}) ||
		ev.Text != "<@bot> hello there" || !ev.Mentioned || ev.Request != "hello there" ||
		len(ev.Roles) != 1 || ev.Roles[0].ID != "r1" || ev.Sent.IsZero() ||
		len(ev.Files) != 1 || ev.Files[0].Name != "cat.png" {
		t.Errorf("event = %+v", ev)
	}
	r := x.f.waitFor(t, "POST", "/channels/c1/messages")
	if !strings.Contains(r.Body, `"message_id":"m7"`) {
		t.Errorf("the reply did not answer the message: %s", r.Body)
	}
}

func TestReactionsReachOnReaction(t *testing.T) {
	b := airut.New(airut.Config{})
	got := make(chan airut.ReactionEvent, 4)
	b.OnReaction(func(c *airut.Ctx, ev airut.ReactionEvent) error {
		got <- ev
		return nil
	})
	x := start(t, b)
	x.s.me = "bot"
	react := func(user string, e discordgo.Emoji) {
		x.s.onReaction(x.s.dg, &discordgo.MessageReactionAdd{
			MessageReaction: &discordgo.MessageReaction{UserID: user, MessageID: "m7", Emoji: e, ChannelID: "c1", GuildID: "g1"},
			Member:          &discordgo.Member{User: &discordgo.User{ID: user, Username: "ada"}, Roles: []string{"r1"}},
		})
	}
	react("u1", discordgo.Emoji{Name: "blob", ID: "5"})
	react("u1", discordgo.Emoji{Name: "✅"})
	react("bot", discordgo.Emoji{Name: "✅"})
	x.s.stop()
	if len(got) != 2 {
		t.Fatalf("delivered %d reactions, want 2", len(got))
	}
	first, second := <-got, <-got
	if first.Emoji == "✅" {
		first, second = second, first
	}
	if first.Emoji != "blob:5" || first.Target.ID != "m7" || first.Sender.ID != "u1" || len(first.Roles) != 1 {
		t.Errorf("custom reaction = %+v", first)
	}
	if second.Emoji != "✅" {
		t.Errorf("unicode reaction = %+v", second)
	}
	if first.ID == second.ID || !strings.HasPrefix(first.ID, "reaction:m7:u1:") || strings.Contains(first.ID+second.ID, "blob") || strings.Contains(second.ID, "✅") {
		t.Errorf("reaction ids %q and %q", first.ID, second.ID)
	}
}

func TestReadyAnnouncesTheConn(t *testing.T) {
	b := airut.New(airut.Config{})
	conns := make(chan airut.Conn, 2)
	b.OnReady(func(_ context.Context, c airut.Conn) { conns <- c })
	x := start(t, b)
	x.f.on("PUT", "/applications/42/commands", 200, "[]")
	x.s.onReady(x.s.dg, &discordgo.Ready{User: &discordgo.User{ID: "42"}})
	x.s.onResumed(x.s.dg, &discordgo.Resumed{})
	for range 2 {
		select {
		case c := <-conns:
			if c.Platform() != airut.Discord {
				t.Errorf("platform = %s", c.Platform())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no Conn announced")
		}
	}
	if x.s.self() != "42" {
		t.Errorf("self = %q", x.s.self())
	}
}

func TestSendWithAKey(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	ch := airut.Channel{Platform: airut.Discord, ID: "c1"}
	for range 2 {
		if _, err := x.s.Send(context.Background(), ch, airut.Message{Text: strings.Repeat("k", 2500), Key: "review:m7"}); err != nil {
			t.Fatal(err)
		}
	}
	sent := x.f.sent()
	if len(sent) != 4 {
		t.Fatalf("sent %d requests, want 4", len(sent))
	}
	n0, n1 := nonce("review:m7", 0), nonce("review:m7", 1)
	if len(n0) != 25 || n0 == n1 || strings.Contains(n0, "review") {
		t.Errorf("nonces %q and %q", n0, n1)
	}
	for i, r := range sent {
		want := []string{n0, n1}[i%2]
		if !strings.Contains(r.Body, `"nonce":"`+want+`"`) || !strings.Contains(r.Body, `"enforce_nonce":true`) {
			t.Errorf("request %d = %s, want nonce %s", i, r.Body, want)
		}
	}
	if _, err := x.s.Send(context.Background(), ch, airut.Message{Text: "x", Private: true}); err == nil {
		t.Error("a private Send was accepted")
	}
	if _, err := x.s.Send(context.Background(), ch, airut.Message{Text: "plain"}); err != nil {
		t.Fatal(err)
	}
	if last := x.f.sent()[4]; strings.Contains(last.Body, "nonce") {
		t.Errorf("an unkeyed send carried a nonce: %s", last.Body)
	}
}

func TestOpen(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("GET", "/attachments/1/2/cat.png", 200, "meow")
	for _, u := range []string{"https://example.com/attachments/1/2/cat.png", "http://cdn.discordapp.com/attachments/1/2/cat.png", "file:///etc/passwd"} {
		if _, err := x.s.Open(context.Background(), airut.Attachment{URL: u}); err == nil {
			t.Errorf("opened %s", u)
		}
	}
	rc, err := x.s.Open(context.Background(), airut.Attachment{URL: "https://cdn.discordapp.com/attachments/1/2/cat.png?ex=1&hm=2"})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "meow" {
		t.Errorf("read %q", b)
	}
}

// headerSpy records the Authorization header of each request it passes
// on.
type headerSpy struct {
	next http.RoundTripper
	auth *string
}

func (h headerSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	*h.auth = r.Header.Get("Authorization")
	return h.next.RoundTrip(r)
}

func TestOpenSendsNoToken(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	var auth string
	x.f.reply("GET", "/attachments/1/2/cat.png", func(string) fakeReply { return fakeReply{200, "meow"} })
	x.s.dg.Client.Transport = headerSpy{x.f, &auth}
	rc, err := x.s.Open(context.Background(), airut.Attachment{URL: "https://cdn.discordapp.com/attachments/1/2/cat.png"})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if auth != "" {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestHistory(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.s.me = "bot"
	x.f.on("GET", "/channels/c1/messages", 200, `[
		{"id": "m3", "channel_id": "c1", "type": 0, "content": "newest", "author": {"id": "u1"}, "timestamp": "2027-01-15T08:00:00Z"},
		{"id": "m2", "channel_id": "c1", "type": 0, "content": "mine", "author": {"id": "bot"}, "timestamp": "2027-01-15T07:00:00Z"}
	]`)
	page, err := x.s.History(context.Background(), airut.Channel{ID: "c1"}, "m4", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Text != "newest" || page.Next != "m2" {
		t.Errorf("page = %+v", page)
	}
	page, err = x.s.History(context.Background(), airut.Channel{ID: "c1"}, "m4", 10)
	if err != nil || page.Next != "" {
		t.Errorf("a short page has Next %q, err %v", page.Next, err)
	}
}

func TestMember(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("GET", "/channels/c1", 200, `{"id": "c1", "guild_id": "g1", "type": 0}`)
	x.f.on("GET", "/guilds/g1/members/u1", 200, `{"user": {"id": "u1"}}`)
	x.f.on("GET", "/guilds/g1/members/u2", 404, `{"message": "Unknown Member", "code": 10007}`)
	x.f.on("GET", "/channels/d1", 200, `{"id": "d1", "type": 1, "recipients": [{"id": "u1"}]}`)
	for _, c := range []struct {
		ch, u string
		want  bool
	}{{"c1", "u1", true}, {"c1", "u2", false}, {"d1", "u1", true}, {"d1", "u2", false}} {
		got, err := x.s.Member(context.Background(), airut.Channel{ID: c.ch}, airut.User{ID: c.u})
		if err != nil || got != c.want {
			t.Errorf("Member(%s, %s) = %v, %v", c.ch, c.u, got, err)
		}
	}
	x.f.on("GET", "/guilds/g1/members/u3", 500, `{"message": "oops", "code": 0}`)
	if _, err := x.s.Member(context.Background(), airut.Channel{ID: "c1"}, airut.User{ID: "u3"}); err == nil {
		t.Error("a server error read as an answer")
	}
}

func TestIntents(t *testing.T) {
	if (Options{}).intents()&discordgo.IntentsMessageContent != 0 {
		t.Error("message content is asked for without the option")
	}
	if (Options{MessageContent: true}).intents()&discordgo.IntentsMessageContent == 0 {
		t.Error("message content is not asked for with the option")
	}
}

func TestRequest(t *testing.T) {
	for in, want := range map[string]string{
		"<@bot> roll 2d6":  "roll 2d6",
		"<@!bot>  roll":    "roll",
		"hey <@bot> there": "hey <@bot> there",
	} {
		if got := request(in, "bot"); got != want {
			t.Errorf("request(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenFollowsNoRedirectOffDiscord(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.reply("GET", "/attachments/1/2/cat.png", func(string) fakeReply { return fakeReply{302, ""} })
	// The fake would serve the redirect's target, so only the check
	// on redirects keeps Open from reading it.
	x.f.on("GET", "/elsewhere", 200, "not an attachment")
	x.s.dg.Client.Transport = redirector{x.f, "https://example.com/elsewhere"}
	if _, err := x.s.Open(context.Background(), airut.Attachment{URL: "https://cdn.discordapp.com/attachments/1/2/cat.png"}); err == nil {
		t.Error("Open followed a redirect off Discord")
	}
}

// redirector answers a 302 from next with a Location of to.
type redirector struct {
	next http.RoundTripper
	to   string
}

func (r redirector) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err == nil && resp.StatusCode == 302 {
		resp.Header.Set("Location", r.to)
	}
	return resp, err
}

func TestConnReactions(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("PUT", "/channels/10/messages/20/reactions/*", 204, ``)
	x.f.on("DELETE", "/channels/10/messages/20/reactions/*", 204, ``)
	ctx, ch, msg := context.Background(), airut.Channel{ID: "10"}, airut.MessageRef{ID: "20"}

	seen, err := x.s.React(ctx, ch, msg, "👀")
	if err != nil || seen.ID != "20/👀" {
		t.Fatalf("React = %v, %v", seen, err)
	}
	x.f.waitFor(t, "PUT", "/channels/10/messages/20/reactions/👀/@me")
	if err := x.s.Unreact(ctx, ch, seen); err != nil {
		t.Fatal(err)
	}
	// Only the endpoint for the bot's own reaction is ever called.
	x.f.waitFor(t, "DELETE", "/channels/10/messages/20/reactions/👀/@me")
	custom, err := x.s.React(ctx, ch, msg, "<:blob:30>")
	if err != nil || custom.ID != "20/blob:30" {
		t.Errorf("React with a custom emoji = %v, %v", custom, err)
	}

	// discordgo escapes a '#' in an emoji, such as the keycap's, so it
	// cannot end the path early and drop the @me that limits a removal
	// to the bot's own reaction. These requests are what was sent.
	keycap, err := x.s.React(ctx, ch, msg, "#️⃣")
	if err != nil {
		t.Fatal(err)
	}
	x.f.waitFor(t, "PUT", "/channels/10/messages/20/reactions/#️⃣/@me")
	for _, ref := range []airut.MessageRef{keycap, {ID: "20/👍#"}, {ID: "20/#"}} {
		if err := x.s.Unreact(ctx, ch, ref); err != nil {
			t.Fatal(err)
		}
		_, e, _ := strings.Cut(ref.ID, "/")
		x.f.waitFor(t, "DELETE", "/channels/10/messages/20/reactions/"+e+"/@me")
	}
	for _, r := range x.f.sent() {
		if r.Method == "DELETE" && !strings.HasSuffix(r.Path, "/@me") {
			t.Errorf("a removal reached %s, which is not the bot's own reaction", r.Path)
		}
	}

	before := len(x.f.sent())
	for _, ref := range []string{"20", "", "20/", "x/👀", "20/👀/../../..", "20/..", "20/a?b", "20/%2F"} {
		if err := x.s.Unreact(ctx, ch, airut.MessageRef{ID: ref}); !errors.Is(err, airut.ErrNotOwnReaction) {
			t.Errorf("Unreact(%q) = %v, want ErrNotOwnReaction", ref, err)
		}
	}
	if err := x.s.Unreact(ctx, airut.Channel{ID: "c/1"}, seen); !errors.Is(err, airut.ErrNotOwnReaction) {
		t.Errorf("Unreact in a channel that is not an id = %v", err)
	}
	for _, c := range []struct{ ch, msg, emoji string }{{"10", "20/x", "👀"}, {"10", "20", "a/b"}, {"1/0", "20", "👀"}, {"10", "20", ""}} {
		if _, err := x.s.React(ctx, airut.Channel{ID: c.ch}, airut.MessageRef{ID: c.msg}, c.emoji); err == nil {
			t.Errorf("React(%q, %q, %q) was sent", c.ch, c.msg, c.emoji)
		}
	}
	if n := len(x.f.sent()) - before; n != 0 {
		t.Errorf("%d requests left for refused refs: %v", n, x.f.sent()[before:])
	}
}
