package matrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"terva.sh/airut"
)

var bot = names{id: "@baphomet:example.org", local: "baphomet", display: "Baphomet"}

func known(name string) bool { return name == "roll" || name == "tags" }

func TestTrigger(t *testing.T) {
	cases := []struct {
		text, prefix, name, rest string
		ok                       bool
	}{
		{"!roll 2d6", "!", "roll", "2d6", true},
		{"  !roll", "!", "roll", "", true},
		{"Baphomet: roll 2d6", "!", "roll", "2d6", true},
		{"Baphomet: !roll 2d6", "!", "roll", "2d6", true},
		{"baphomet, roll", "!", "roll", "", true},
		{"@baphomet:example.org roll", "!", "roll", "", true},
		{"BAPHOMET: tags +cat", "!", "tags", "+cat", true},
		// Chat that is not addressed to the bot, or not a command.
		{"roll 2d6", "!", "", "", false},
		{"Baphomets are great", "!", "", "", false},
		{"I asked Baphomet: roll", "!", "", "", false},
		// A mention calls a command without the prefix only when it is one.
		{"Baphomet: what is this?", "!", "", "", false},
		{"Baphomet: !what is this?", "!", "what", "is this?", true},
		{"! roll", "!", "roll", "", true},
		{"!", "!", "", "", false},
		// Without a prefix, only a mention triggers.
		{"roll 2d6", "", "", "", false},
		{"Baphomet: roll 2d6", "", "roll", "2d6", true},
	}
	for _, c := range cases {
		name, rest, ok := trigger(c.text, c.prefix, bot, known)
		if name != c.name || rest != c.rest || ok != c.ok {
			t.Errorf("trigger(%q, %q) = %q, %q, %v; want %q, %q, %v", c.text, c.prefix, name, rest, ok, c.name, c.rest, c.ok)
		}
	}
}

func message(c *event.MessageEventContent) *event.Event {
	return &event.Event{
		ID: "$m", RoomID: "!room:example.org", Sender: "@alice:example.org", Type: event.EventMessage,
		Content: event.Content{Parsed: c},
	}
}

func TestReadMessage(t *testing.T) {
	// A reply whose body still carries the old quoted fallback.
	reply := &event.MessageEventContent{
		MsgType: event.MsgText, Body: "> <@bob:example.org> earlier\n\n!tags +cat",
		RelatesTo: (&event.RelatesTo{}).SetReplyTo("$earlier"),
	}
	in, ok := readMessage(message(reply))
	if !ok || in.text != "!tags +cat" || in.reply != "$earlier" || in.thread != "" {
		t.Errorf("reply read as %+v, %v", in, ok)
	}

	// A message in a thread whose reply relation is only the fallback.
	threaded := &event.MessageEventContent{MsgType: event.MsgText, Body: "hi",
		RelatesTo: (&event.RelatesTo{}).SetThread("$root", "$last")}
	in, _ = readMessage(message(threaded))
	if in.thread != "$root" || in.reply != "" {
		t.Errorf("thread %q, reply %q; want $root and no reply", in.thread, in.reply)
	}

	// An image: the caption is the text, and the file an attachment.
	img := &event.MessageEventContent{MsgType: event.MsgImage, Body: "a cat", FileName: "cat.png",
		URL: "mxc://example.org/cat", Info: &event.FileInfo{MimeType: "image/png", Size: 7}}
	in, _ = readMessage(message(img))
	want := airut.Attachment{Name: "cat.png", ContentType: "image/png", Size: 7, URL: "mxc://example.org/cat"}
	if in.text != "a cat" || len(in.files) != 1 || in.files[0] != want {
		t.Errorf("image read as text %q, files %+v", in.text, in.files)
	}
	img.Body, img.FileName = "cat.png", ""
	if in, _ = readMessage(message(img)); in.text != "" || in.files[0].Name != "cat.png" {
		t.Errorf("an image without a caption read as text %q, files %+v", in.text, in.files)
	}

	// An encrypted file is marked, so Open knows it needs the key.
	enc := &event.MessageEventContent{MsgType: event.MsgFile, Body: "notes.txt",
		File: &event.EncryptedFileInfo{URL: "mxc://example.org/notes"}}
	if in, _ = readMessage(message(enc)); in.files[0].URL != "mxc://example.org/notes"+encryptedMark {
		t.Errorf("encrypted attachment URL %q", in.files[0].URL)
	}

	// Edits and redacted messages are not acted on.
	edit := &event.MessageEventContent{MsgType: event.MsgText, Body: "* !roll"}
	edit.SetEdit("$m0")
	for name, c := range map[string]*event.MessageEventContent{"an edit": edit, "a redacted message": {}} {
		if _, ok := readMessage(message(c)); ok {
			t.Errorf("%s was read as a message", name)
		}
	}
}

func TestAddressed(t *testing.T) {
	cases := []struct {
		name     string
		content  *event.MessageEventContent
		request  string
		mentions bool
	}{
		{"the display name and a colon", &event.MessageEventContent{MsgType: event.MsgText, Body: "Baphomet: find a fox"}, "find a fox", true},
		{"the localpart and a comma", &event.MessageEventContent{MsgType: event.MsgText, Body: "  baphomet, find a fox\n\n    in snow\n"}, "find a fox\n\n    in snow\n", true},
		{"the user id", &event.MessageEventContent{MsgType: event.MsgText, Body: "@baphomet:example.org find a fox"}, "find a fox", true},
		{"a mention alone", &event.MessageEventContent{MsgType: event.MsgText, Body: "Baphomet:"}, "", true},
		{"a pill in the middle", &event.MessageEventContent{MsgType: event.MsgText, Body: "  what do you think, Baphomet?\n    code\n",
			Mentions: &event.Mentions{UserIDs: []id.UserID{"@baphomet:example.org"}}}, "  what do you think, Baphomet?\n    code\n", true},
		{"a caption", &event.MessageEventContent{MsgType: event.MsgImage, Body: "Baphomet: tag this", FileName: "cat.png", URL: "mxc://example.org/cat"}, "tag this", true},
		{"a room mention", &event.MessageEventContent{MsgType: event.MsgText, Body: "@room lunch", Mentions: &event.Mentions{Room: true}}, "", false},
		{"someone else", &event.MessageEventContent{MsgType: event.MsgText, Body: "Bob: lunch",
			Mentions: &event.Mentions{UserIDs: []id.UserID{"@bob:example.org"}}}, "", false},
		{"the name inside a word", &event.MessageEventContent{MsgType: event.MsgText, Body: "Baphomets are great"}, "", false},
		{"plain chat", &event.MessageEventContent{MsgType: event.MsgText, Body: "lunch?"}, "", false},
	}
	alice := airut.User{Platform: airut.Matrix, ID: "@alice:example.org"}
	for _, c := range cases {
		in, ok := readMessage(message(c.content))
		if !ok {
			t.Fatalf("%s: not read as a message", c.name)
		}
		ev := in.messageEvent(alice, bot)
		if ev.Mentioned != c.mentions || ev.Request != c.request {
			t.Errorf("%s: Mentioned %v, Request %q; want %v, %q", c.name, ev.Mentioned, ev.Request, c.mentions, c.request)
		}
		if ev.Text != in.text {
			t.Errorf("%s: Text changed to %q", c.name, ev.Text)
		}
	}
}

func TestNoticesAreNeverCommands(t *testing.T) {
	in, _ := readMessage(message(&event.MessageEventContent{MsgType: event.MsgNotice, Body: "!roll"}))
	if _, _, ok := in.command("!", bot, known); ok {
		t.Error("a notice was read as a command")
	}
}

func TestInvocationAndEvent(t *testing.T) {
	c := &event.MessageEventContent{MsgType: event.MsgText, Body: "!tags +cat",
		RelatesTo: (&event.RelatesTo{}).SetThread("$root", "").SetReplyTo("$review")}
	evt := message(c)
	in, _ := readMessage(evt)
	alice := airut.User{Platform: airut.Matrix, ID: "@alice:example.org", Name: "alice"}
	inv := in.invocation(alice, "tags", "+cat")
	if inv.ID != "$m" || inv.Message.ID != "$m" || inv.ReplyTo.ID != "$review" || inv.Thread != "$root" ||
		inv.Channel != (airut.Channel{Platform: airut.Matrix, ID: "!room:example.org"}) || inv.Native != evt || inv.Text != "+cat" {
		t.Errorf("invocation %+v", inv)
	}
	ev := in.messageEvent(alice, bot)
	if ev.Text != "!tags +cat" || ev.ReplyTo.ID != "$review" || ev.Thread != "$root" || ev.Sender != alice || !ev.Sent.IsZero() {
		t.Errorf("event %+v", ev)
	}
	evt.Timestamp = 1792425600123
	if ev = in.messageEvent(alice, bot); !ev.Sent.Equal(time.UnixMilli(1792425600123)) {
		t.Errorf("an event stamped by the homeserver was sent at %v", ev.Sent)
	}
}

func TestNormalizeEmoji(t *testing.T) {
	if normalizeEmoji("✔️") != "✔" || normalizeEmoji("✅") != "✅" {
		t.Errorf("normalizeEmoji: %q, %q", normalizeEmoji("✔️"), normalizeEmoji("✅"))
	}
}

func TestSplit(t *testing.T) {
	if got := split("", 10); len(got) != 0 {
		t.Errorf("split of nothing = %q", got)
	}
	if got := split("short", 10); len(got) != 1 || got[0] != "short" {
		t.Errorf("split of short text = %q", got)
	}
	// A line break in the second half of a piece is where it is cut.
	if got := split("aaaaaaa\nbbbbbbbbbb", 10); len(got) != 2 || got[0] != "aaaaaaa\n" {
		t.Errorf("split at a line = %q", got)
	}
	// Never inside a character.
	text := strings.Repeat("é", 50)
	pieces := split(text, 9)
	if strings.Join(pieces, "") != text {
		t.Fatal("split lost text")
	}
	for _, p := range pieces {
		if len(p) > 9 || !utf8.ValidString(p) {
			t.Errorf("piece %q is too long or cut a character", p)
		}
	}
	if got := truncate(strings.Repeat("é", 50), 10); len(got) > 10 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate = %q", got)
	}
}

func TestRelation(t *testing.T) {
	cases := []struct {
		thread, reply id.EventID
		want          string
	}{
		{"", "", "null"},
		{"", "$r", `{"m.in_reply_to":{"event_id":"$r"}}`},
		{"$t", "", `{"rel_type":"m.thread","event_id":"$t","m.in_reply_to":{"event_id":"$t"},"is_falling_back":true}`},
		{"$t", "$r", `{"rel_type":"m.thread","event_id":"$t","m.in_reply_to":{"event_id":"$r"}}`},
	}
	for _, c := range cases {
		got, _ := json.Marshal(relation(c.thread, c.reply))
		if string(got) != c.want {
			t.Errorf("relation(%q, %q) = %s, want %s", c.thread, c.reply, got, c.want)
		}
	}
}

func TestRenderPingsNobody(t *testing.T) {
	c := render("hello @room and <b>@alice:example.org</b>")
	if c.Mentions == nil || c.Mentions.Room || len(c.Mentions.UserIDs) != 0 {
		t.Errorf("mentions = %+v; want empty, so nobody is pinged", c.Mentions)
	}
	if strings.Contains(c.FormattedBody, "<b>") {
		t.Errorf("HTML in the text was rendered: %s", c.FormattedBody)
	}
	if c.Body != "hello @room and <b>@alice:example.org</b>" {
		t.Errorf("body = %q", c.Body)
	}
}

func TestIdentifier(t *testing.T) {
	cases := []struct {
		kind  airut.Kind
		token string
		want  string
		ok    bool
	}{
		{airut.KindUser, "@alice:example.org", "@alice:example.org", true},
		{airut.KindUser, "https://matrix.to/#/@alice:example.org", "@alice:example.org", true},
		{airut.KindUser, "matrix:u/alice:example.org", "@alice:example.org", true},
		{airut.KindUser, "alice", "", false},
		{airut.KindUser, "!room:example.org", "", false},
		{airut.KindChannel, "!room:example.org", "!room:example.org", true},
		{airut.KindChannel, "#general:example.org", "#general:example.org", true},
		{airut.KindChannel, "https://matrix.to/#/#general:example.org", "#general:example.org", true},
		{airut.KindChannel, "#general", "", false},
		{airut.KindChannel, "@alice:example.org", "", false},
		{airut.KindRole, "admins", "", false},
	}
	for _, c := range cases {
		got, ok := identifier(c.kind, c.token)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("identifier(%s, %q) = %q, %v; want %q, %v", c.kind, c.token, got, ok, c.want, c.ok)
		}
	}
}

func TestLRU(t *testing.T) {
	l := newLRU[int, string](2)
	l.put(1, "a")
	l.put(2, "b")
	l.get(1) // 2 is now the least recently used
	l.put(3, "c")
	if _, ok := l.get(2); ok {
		t.Error("the least recently used entry was kept")
	}
	if v, ok := l.get(1); !ok || v != "a" {
		t.Errorf("get(1) = %q, %v", v, ok)
	}
	l.put(1, "z")
	if v, _ := l.get(1); v != "z" {
		t.Errorf("a second put did not replace: %q", v)
	}
}

func TestZerologBridge(t *testing.T) {
	var buf bytes.Buffer
	log := zerologTo(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.Debug().Msg("routine detail")
	log.Warn().Str("room", "!room:example.org").Int("count", 3).Msg("undecryptable events")
	log.Error().Msg("sync failed")
	out := buf.String()
	if strings.Contains(out, "routine detail") {
		t.Errorf("debug lines were forwarded:\n%s", out)
	}
	for _, want := range []string{"level=WARN", `msg="undecryptable events"`, "room=!room:example.org", "count=3", "source=rihma", "level=ERROR"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %s:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("logged %d lines, want 2:\n%s", n, out)
	}
	fmt.Fprint(slogWriter{slog.New(slog.NewTextHandler(&buf, nil))}, "not json")
	if !strings.Contains(buf.String(), "not JSON") {
		t.Error("a line that is not JSON went unreported")
	}
}

var _ zerolog.Logger = zerologTo(slog.Default())

func TestMessagesFitAnEvent(t *testing.T) {
	for name, text := range map[string]string{
		"backslashes": strings.Repeat(`\`, 60000),
		"angles":      strings.Repeat("<", 60000),
		"ampersands":  strings.Repeat("&", 60000),
		"markdown":    strings.Repeat("**a** _b_ `c` ", 6000),
		"emoji":       strings.Repeat("🐐", 20000),
		"short":       "hello",
	} {
		var bodies []string
		for _, c := range messages(text) {
			if n := contentSize(c); n > maxContent {
				t.Errorf("%s: a message is %d bytes serialized, over %d", name, n, maxContent)
			}
			bodies = append(bodies, c.Body)
		}
		if strings.Join(bodies, "") != text {
			t.Errorf("%s: the messages do not add up to the text", name)
		}
	}
	if got := messages(""); len(got) != 0 {
		t.Errorf("empty text made %d messages", len(got))
	}
}

func TestEditsFitAnEvent(t *testing.T) {
	for name, text := range map[string]string{
		"backslashes": strings.Repeat(`\`, 60000),
		"angles":      strings.Repeat("<", 60000),
		"short":       "hello",
	} {
		c := editContent("$m", text)
		if n := contentSize(c); n > maxContent {
			t.Errorf("%s: the edit is %d bytes serialized, over %d", name, n, maxContent)
		}
		if c.NewContent == nil || !strings.HasPrefix(text, strings.TrimSuffix(c.NewContent.Body, "…")) {
			t.Errorf("%s: the edit's new content is not a prefix of the text", name)
		}
	}
	if c := editContent("$m", "hello"); c.NewContent.Body != "hello" {
		t.Errorf("a short edit was changed: %q", c.NewContent.Body)
	}
}

func TestSplitBelowOneCharacter(t *testing.T) {
	if got := split("éé", 1); len(got) != 2 || got[0] != "é" {
		t.Errorf("split(éé, 1) = %q", got)
	}
	if got := truncate("abcdef", 1); got != "…" {
		t.Errorf("truncate below the mark = %q", got)
	}
}

func TestOnlyThem(t *testing.T) {
	const bot, alice, carol = id.UserID("@bot:x"), id.UserID("@alice:x"), id.UserID("@carol:x")
	m := func(ms event.Membership) *event.MemberEventContent { return &event.MemberEventContent{Membership: ms} }
	type members = map[id.UserID]*event.MemberEventContent
	cases := []struct {
		name    string
		members members
		want    bool
	}{
		{"both joined", members{bot: m(event.MembershipJoin), alice: m(event.MembershipJoin)}, true},
		{"alice invited", members{bot: m(event.MembershipJoin), alice: m(event.MembershipInvite)}, true},
		{"a third member", members{bot: m(event.MembershipJoin), alice: m(event.MembershipJoin), carol: m(event.MembershipJoin)}, false},
		{"a third invited", members{bot: m(event.MembershipJoin), alice: m(event.MembershipJoin), carol: m(event.MembershipInvite)}, false},
		{"a third who left", members{bot: m(event.MembershipJoin), alice: m(event.MembershipJoin), carol: m(event.MembershipLeave)}, true},
		{"alice left", members{bot: m(event.MembershipJoin), alice: m(event.MembershipLeave)}, false},
		{"the bot only invited", members{bot: m(event.MembershipInvite), alice: m(event.MembershipJoin)}, false},
		{"nobody", members{}, false},
	}
	for _, c := range cases {
		if got := onlyThem(c.members, bot, alice); got != c.want {
			t.Errorf("%s: onlyThem = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTxnID(t *testing.T) {
	if txnID("", 0) != "" {
		t.Error("a message without a key got a transaction id")
	}
	a, b := txnID("event-1/reply", 0), txnID("event-1/reply", 1)
	if a == b || a != txnID("event-1/reply", 0) || a == txnID("event-1/alert", 0) {
		t.Errorf("transaction ids %q and %q are not one per key and part", a, b)
	}
	// A transaction id is a path segment; the key may hold anything.
	if id := txnID("a/b c?#%"+strings.Repeat("x", 300), 2); strings.ContainsAny(id, "/ ?#%") || len(id) > 64 {
		t.Errorf("transaction id %q is not a short, safe path segment", id)
	}
}
