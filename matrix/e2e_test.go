//go:build e2e

package matrix_test

// The live suite for the Matrix adapter, against the throwaway Synapse
// from `just synapse`. `just e2e` runs it. It never touches a real
// account.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut"
	"terva.sh/airut/config"
	"terva.sh/airut/matrix"
)

func homeserver(t *testing.T) string {
	hs := os.Getenv("AIRUT_E2E_HS")
	if hs == "" {
		t.Skip("AIRUT_E2E_HS not set; run `just e2e`")
	}
	return hs
}

func tag() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func register(t *testing.T, ctx context.Context, hs, localpart string) {
	t.Helper()
	cli, err := mautrix.NewClient(hs, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.RegisterDummy(ctx, &mautrix.ReqRegister[any]{Username: localpart, Password: "pw-" + localpart, InhibitLogin: true}); err != nil {
		t.Fatalf("register %s (is `just synapse` up?): %v", localpart, err)
	}
}

// human is a person's client, with every event the bot sends it.
type human struct {
	*rihma.Client
	mu  sync.Mutex
	got []*event.Event
}

func openHuman(t *testing.T, ctx context.Context, hs, localpart string) *human {
	t.Helper()
	dir := t.TempDir()
	c, err := rihma.Open(ctx, rihma.Options{
		Homeserver: hs,
		StateDir:   filepath.Join(dir, "state"),
		Sessions:   rihma.FileSessionStore{Path: filepath.Join(dir, "session.json")},
		Login: &mautrix.ReqLogin{
			Type:       mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: localpart},
			Password:   "pw-" + localpart,
		},
		DeviceName: "airut e2e",
		Logger:     zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &human{Client: c}
	record := func(_ context.Context, evt *event.Event) {
		if evt.Sender == c.UserID {
			return
		}
		h.mu.Lock()
		h.got = append(h.got, evt)
		h.mu.Unlock()
	}
	c.Handlers().OnEventType(event.EventMessage, record)
	c.Handlers().OnEventType(event.EventReaction, record)
	c.Handlers().OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		if evt.GetStateKey() == string(c.UserID) && evt.Content.AsMember().Membership == event.MembershipInvite {
			c.JoinRoomByID(ctx, evt.RoomID)
			record(ctx, evt)
		}
	})
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Sync(syncCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; c.Close() })
	return h
}

// waitFor returns the first event the bot sent that match accepts.
func (h *human) waitFor(t *testing.T, ctx context.Context, what string, match func(*event.Event) bool) *event.Event {
	t.Helper()
	for {
		h.mu.Lock()
		for _, evt := range h.got {
			if match(evt) {
				h.mu.Unlock()
				return evt
			}
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatalf("never received %s", what)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (h *human) send(t *testing.T, ctx context.Context, room id.RoomID, text string) id.EventID {
	t.Helper()
	resp, err := h.SendMessageEvent(ctx, room, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: text})
	if err != nil {
		t.Fatal(err)
	}
	return resp.EventID
}

func body(evt *event.Event) string { return evt.Content.AsMessage().Body }

func bodyIs(text string) func(*event.Event) bool {
	return func(evt *event.Event) bool { return evt.Type == event.EventMessage && body(evt) == text }
}

// seen collects what the bot's handlers saw.
type seen struct {
	mu        sync.Mutex
	messages  []airut.MessageEvent
	reactions []airut.ReactionEvent
	conn      airut.Conn
	// threads are the ids of the threads the thread command opened.
	threads []string
	// stop stops the bot and waits for it, as a restart would.
	stop func()
}

func (s *seen) wait(t *testing.T, ctx context.Context, what string, ok func(*seen) bool) {
	t.Helper()
	for {
		s.mu.Lock()
		done := ok(s)
		s.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the bot never saw %s", what)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// testBot is a bot with a command for each part of the surface: a plain
// reply, an ask, a private reply, and a thread. Its message handler reads
// each attachment and posts its size, through the Conn, as a reply.
func startBot(t *testing.T, ctx context.Context, hs, localpart string, o matrix.Options) *seen {
	t.Helper()
	return startBotIn(t, ctx, hs, localpart, o, t.TempDir())
}

// startBotIn starts the bot with its state in dir, so a test can stop it
// and start it again as the same device.
func startBotIn(t *testing.T, ctx context.Context, hs, localpart string, o matrix.Options, dir string) *seen {
	t.Helper()
	var logs bytes.Buffer
	var logMu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	b := airut.New(airut.Config{Logger: log, AskTimeout: 30 * time.Second})
	s := &seen{}
	b.Command("ping", "Answer pong", func(c *airut.Ctx) error {
		_, err := c.Reply("pong")
		return err
	})
	b.Command("ask", "Ask a question", func(c *airut.Ctx) error {
		got, err := c.Ask("Proceed?", "yes", "no")
		if err != nil {
			return err
		}
		_, err = c.Reply("chose " + got)
		return err
	})
	b.Command("whisper", "Reply privately", func(c *airut.Ctx) error {
		_, err := c.ReplyPrivate("just for you")
		return err
	})
	b.Command("whisperedit", "Reply privately, then edit and react to it", func(c *airut.Ctx) error {
		ref, err := c.ReplyPrivate("draft")
		if err != nil {
			return err
		}
		if err := c.Edit(ref, "final words"); err != nil {
			return err
		}
		return c.React(ref, "👍")
	})
	b.Command("thread", "Reply in a thread", func(c *airut.Ctx) error {
		tc, err := c.Thread("a thread")
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.threads = append(s.threads, tc.ThreadID())
		s.mu.Unlock()
		_, err = tc.Reply("in the thread")
		return err
	})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		s.mu.Lock()
		s.messages = append(s.messages, m)
		s.mu.Unlock()
		conn, err := c.Conn()
		if err != nil {
			return err
		}
		for _, f := range m.Files {
			r, err := conn.Open(c.Context(), f)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(r, 1<<20))
			r.Close()
			if err != nil {
				return err
			}
			if _, err := conn.Send(c.Context(), c.Channel(), airut.Message{Text: fmt.Sprintf("read %s: %q", f.Name, data), ReplyTo: m.Message}); err != nil {
				return err
			}
		}
		return nil
	})
	b.OnReaction(func(c *airut.Ctx, r airut.ReactionEvent) error {
		s.mu.Lock()
		s.reactions = append(s.reactions, r)
		s.mu.Unlock()
		return nil
	})
	b.OnReady(func(_ context.Context, c airut.Conn) {
		s.mu.Lock()
		s.conn = c
		s.mu.Unlock()
	})
	if o.AcceptInvite == nil {
		o.AcceptInvite = func(string, string) bool { return true }
	}
	a := matrix.New(&config.Matrix{
		Homeserver: hs,
		UserID:     "@" + localpart + ":localhost",
		Password:   config.NewSecret("pw-" + localpart),
		StateDir:   dir,
		DeviceName: "airut e2e bot",
	}, o)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- b.Run(runCtx, a) }()
	var once sync.Once
	s.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("the bot's Run returned %v", err)
			}
		})
	}
	t.Cleanup(func() {
		s.stop()
		logMu.Lock()
		defer logMu.Unlock()
		for _, leak := range []string{"pw-" + localpart, "just for you", "final words", "hello there", "Proceed?"} {
			if strings.Contains(logs.String(), leak) {
				t.Errorf("the bot's log holds %q:\n%s", leak, logs.String())
			}
		}
		if t.Failed() {
			t.Logf("the bot's log:\n%s", logs.String())
		}
	})
	return s
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func createRoom(t *testing.T, ctx context.Context, h *human, bot id.UserID, encrypted bool) id.RoomID {
	t.Helper()
	req := &mautrix.ReqCreateRoom{Preset: "private_chat", Invite: []id.UserID{bot}}
	if encrypted {
		req.InitialState = []*event.Event{{
			Type:    event.StateEncryption,
			Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}},
		}}
	}
	resp, err := h.CreateRoom(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for {
		m, err := h.StateStore.GetMember(ctx, resp.RoomID, bot)
		if err == nil && m != nil && m.Membership == event.MembershipJoin {
			return resp.RoomID
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the bot never joined %s", resp.RoomID)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// setDirect sets the bot account's m.direct from a second device, as
// another client of the same account would.
func setDirect(t *testing.T, ctx context.Context, hs, localpart string, direct event.DirectChatsEventContent) {
	t.Helper()
	cli, err := mautrix.NewClient(hs, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Login(ctx, &mautrix.ReqLogin{
		Type:             mautrix.AuthTypePassword,
		Identifier:       mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: localpart},
		Password:         "pw-" + localpart,
		StoreCredentials: true,
	}); err != nil {
		t.Fatal(err)
	}
	defer cli.Logout(ctx)
	if err := cli.SetAccountData(ctx, event.AccountDataDirectChats.Type, direct); err != nil {
		t.Fatal(err)
	}
}

func replyTo(evt *event.Event) id.EventID { return evt.Content.AsMessage().RelatesTo.GetReplyTo() }

func TestE2ECommandsEventsAndConn(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			hs := homeserver(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			tg := tag()
			botName, aliceName := "bot-"+tg, "alice-"+tg
			register(t, ctx, hs, botName)
			register(t, ctx, hs, aliceName)
			botID := id.UserID("@" + botName + ":localhost")
			s := startBot(t, ctx, hs, botName, matrix.Options{})
			alice := openHuman(t, ctx, hs, aliceName)
			room := createRoom(t, ctx, alice, botID, encrypted)
			arrived := func(evt *event.Event) bool { return evt.Mautrix.WasEncrypted == encrypted }

			// A prefix command, answered as a reply.
			ping := alice.send(t, ctx, room, "!ping")
			pong := alice.waitFor(t, ctx, "pong", bodyIs("pong"))
			if replyTo(pong) != ping || !arrived(pong) {
				t.Errorf("pong replied to %q, encrypted %v; want a reply to %q, encrypted %v", replyTo(pong), pong.Mautrix.WasEncrypted, ping, encrypted)
			}
			// A mention with a command name, and one with chat.
			mention := alice.send(t, ctx, room, botName+": ping")
			alice.waitFor(t, ctx, "a pong for the mention", func(evt *event.Event) bool { return bodyIs("pong")(evt) && replyTo(evt) == mention })
			alice.send(t, ctx, room, botName+": hello there")
			s.wait(t, ctx, "the chat that mentioned it", func(s *seen) bool {
				return len(s.messages) > 0 && s.messages[len(s.messages)-1].Text == botName+": hello there"
			})
			s.mu.Lock()
			if m := s.messages[len(s.messages)-1]; !m.Mentioned || m.Request != "hello there" {
				t.Errorf("the chat that mentioned the bot arrived with Mentioned %v, Request %q", m.Mentioned, m.Request)
			} else if age := time.Since(m.Sent); age < -time.Minute || age > time.Minute {
				t.Errorf("the chat arrived sent at %v, %v ago", m.Sent, age)
			}
			s.mu.Unlock()

			// A reaction.
			alice.SendReaction(ctx, room, ping, "✔️")
			s.wait(t, ctx, "the reaction", func(s *seen) bool {
				return len(s.reactions) == 1 && s.reactions[0].Target.ID == string(ping) && s.reactions[0].Emoji == "✔" && s.reactions[0].Sender.ID == string(alice.UserID)
			})

			// The Conn reads an attachment, encrypted or not, and posts.
			s.wait(t, ctx, "the Conn", func(s *seen) bool { return s.conn != nil })
			img, err := alice.SendMedia(ctx, room, rihma.Media{Data: []byte("PNGDATA"), Name: "cat.png", MimeType: "image/png", MsgType: event.MsgImage})
			if err != nil {
				t.Fatal(err)
			}
			read := alice.waitFor(t, ctx, "the attachment's bytes", bodyIs(`read cat.png: "PNGDATA"`))
			if replyTo(read) != img || !arrived(read) {
				t.Errorf("the Conn's post replied to %q, encrypted %v", replyTo(read), read.Mautrix.WasEncrypted)
			}

			// History holds the chat and the image, newest first, and
			// leaves out the commands.
			conn := s.conn
			var texts, requests []string
			var files []airut.Attachment
			cursor := ""
			for range 10 {
				page, err := conn.History(ctx, airut.Channel{Platform: airut.Matrix, ID: string(room)}, cursor, 2)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range page.Messages {
					texts = append(texts, m.Text)
					requests = append(requests, m.Request)
					if age := time.Since(m.Sent); age < -time.Minute || age > 10*time.Minute {
						t.Errorf("history held a message sent at %v", m.Sent)
					}
					files = append(files, m.Files...)
				}
				if cursor = page.Next; cursor == "" {
					break
				}
			}
			if strings.Join(texts, "|") != "|"+botName+": hello there" || len(files) != 1 || files[0].Name != "cat.png" {
				t.Errorf("history held %q and files %+v; want the image, then the chat", texts, files)
			}
			if strings.Join(requests, "|") != "|hello there" {
				t.Errorf("history's requests were %q; want none for the image, then the chat's", requests)
			}
			if r, err := conn.Open(ctx, files[0]); err != nil {
				t.Errorf("opening an attachment from history: %v", err)
			} else if data, _ := io.ReadAll(r); string(data) != "PNGDATA" {
				t.Errorf("an attachment from history read %q", data)
			}

			// Membership.
			ch := airut.Channel{Platform: airut.Matrix, ID: string(room)}
			if ok, err := conn.Member(ctx, ch, airut.User{Platform: airut.Matrix, ID: string(alice.UserID)}); !ok || err != nil {
				t.Errorf("Member(alice) = %v, %v", ok, err)
			}
			if ok, err := conn.Member(ctx, ch, airut.User{Platform: airut.Matrix, ID: "@nobody:localhost"}); ok || err != nil {
				t.Errorf("Member(nobody) = %v, %v", ok, err)
			}
		})
	}
}

// TestE2EKeyedSendAcrossARestart sends a keyed message, restarts the
// bot on the same state, as the same device, and sends it again, as a
// bot resuming work after a crash would. The room gets one message.
func TestE2EKeyedSendAcrossARestart(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			hs := homeserver(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			tg := tag()
			botName, aliceName := "bot-"+tg, "alice-"+tg
			register(t, ctx, hs, botName)
			register(t, ctx, hs, aliceName)
			dir := t.TempDir()
			s := startBotIn(t, ctx, hs, botName, matrix.Options{}, dir)
			alice := openHuman(t, ctx, hs, aliceName)
			room := createRoom(t, ctx, alice, id.UserID("@"+botName+":localhost"), encrypted)
			ch := airut.Channel{Platform: airut.Matrix, ID: string(room)}
			keyed := airut.Message{Text: "once only", Key: "e2e/" + tg}

			s.wait(t, ctx, "the Conn", func(s *seen) bool { return s.conn != nil })
			first, err := s.conn.Send(ctx, ch, keyed)
			if err != nil {
				t.Fatal(err)
			}
			s.stop()
			s = startBotIn(t, ctx, hs, botName, matrix.Options{}, dir)
			s.wait(t, ctx, "the Conn after the restart", func(s *seen) bool { return s.conn != nil })
			again, err := s.conn.Send(ctx, ch, keyed)
			if err != nil || again != first {
				t.Errorf("the keyed send after a restart = %v, %v; want %v", again, err, first)
			}
			if _, err := s.conn.Send(ctx, ch, airut.Message{Text: "after the keyed sends"}); err != nil {
				t.Fatal(err)
			}
			alice.waitFor(t, ctx, "the message after the keyed sends", bodyIs("after the keyed sends"))
			alice.mu.Lock()
			n := 0
			for _, evt := range alice.got {
				if bodyIs("once only")(evt) {
					n++
				}
			}
			alice.mu.Unlock()
			if n != 1 {
				t.Errorf("a keyed message sent before and after a restart arrived %d times", n)
			}
			if _, err := s.conn.Send(ctx, ch, airut.Message{Text: "x", Key: "k", Files: []airut.File{{Name: "a.png", Data: []byte("x")}}}); err == nil {
				t.Error("a keyed message with files was sent")
			}
		})
	}
}

func TestE2EAskThreadAndPrivate(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tg := tag()
	botName, aliceName := "bot-"+tg, "alice-"+tg
	register(t, ctx, hs, botName)
	register(t, ctx, hs, aliceName)
	botID := id.UserID("@" + botName + ":localhost")
	s := startBot(t, ctx, hs, botName, matrix.Options{})
	alice := openHuman(t, ctx, hs, aliceName)
	room := createRoom(t, ctx, alice, botID, false)

	// The ask widget: the first choice is seeded as ①.
	alice.send(t, ctx, room, "!ask")
	q := alice.waitFor(t, ctx, "the question", func(evt *event.Event) bool {
		return evt.Type == event.EventMessage && strings.Contains(body(evt), "Proceed?")
	})
	alice.waitFor(t, ctx, "the seeded choices", func(evt *event.Event) bool {
		return evt.Type == event.EventReaction && evt.Content.AsReaction().RelatesTo.EventID == q.ID
	})
	if _, err := alice.SendReaction(ctx, room, q.ID, "①"); err != nil {
		t.Fatal(err)
	}
	alice.waitFor(t, ctx, "the answer", bodyIs("chose yes"))

	// A thread rooted at the command.
	trigger := alice.send(t, ctx, room, "!thread")
	in := alice.waitFor(t, ctx, "the threaded reply", bodyIs("in the thread"))
	if root := in.Content.AsMessage().RelatesTo.GetThreadParent(); root != trigger {
		t.Errorf("the reply's thread is %q, want %q", root, trigger)
	}
	s.mu.Lock()
	if len(s.threads) != 1 || s.threads[0] != string(trigger) {
		t.Errorf("the opened Ctx named its thread %q, want %q", s.threads, trigger)
	}
	s.mu.Unlock()
	// A reaction to the bot's own message in the thread knows the thread,
	// though sync never showed the bot that message.
	if _, err := alice.SendReaction(ctx, room, in.ID, "✅"); err != nil {
		t.Fatal(err)
	}
	s.wait(t, ctx, "the reaction in the thread", func(s *seen) bool {
		return len(s.reactions) == 1 && s.reactions[0].Target.ID == string(in.ID) && s.reactions[0].Thread == string(trigger)
	})

	// The bot's m.direct names rooms with alice that are not private: one
	// unencrypted, and one with carol invited too. None is used.
	carolName := "carol-" + tg
	register(t, ctx, hs, carolName)
	plain := createRoom(t, ctx, alice, botID, false)
	group := createRoom(t, ctx, alice, botID, true)
	if _, err := alice.InviteUser(ctx, group, &mautrix.ReqInviteUser{UserID: id.UserID("@" + carolName + ":localhost")}); err != nil {
		t.Fatal(err)
	}
	// And one that is encrypted and holds only the two of them, but that
	// anyone may join.
	public := createRoom(t, ctx, alice, botID, true)
	if _, err := alice.SendStateEvent(ctx, public, event.StateJoinRules, "", &event.JoinRulesEventContent{JoinRule: event.JoinRulePublic}); err != nil {
		t.Fatal(err)
	}
	setDirect(t, ctx, hs, botName, event.DirectChatsEventContent{alice.UserID: {plain, group, public}})

	// A private reply arrives in a direct room the bot creates.
	alice.send(t, ctx, room, "!whisper")
	dm := alice.waitFor(t, ctx, "the private reply", bodyIs("just for you"))
	if dm.RoomID == room || dm.RoomID == plain || dm.RoomID == group || dm.RoomID == public || !dm.Mautrix.WasEncrypted {
		t.Errorf("the private reply arrived in %s, encrypted %v; want a new encrypted room", dm.RoomID, dm.Mautrix.WasEncrypted)
	}
	// An edit of and a reaction to a private reply go to the private
	// room, and nothing of them to the room the command came from.
	alice.send(t, ctx, room, "!whisperedit")
	edit := alice.waitFor(t, ctx, "the edit of the private reply", func(evt *event.Event) bool {
		c := evt.Content.AsMessage()
		return evt.Type == event.EventMessage && c.NewContent != nil && c.NewContent.Body == "final words"
	})
	react := alice.waitFor(t, ctx, "the reaction to the private reply", func(evt *event.Event) bool {
		return evt.Type == event.EventReaction && evt.Content.AsReaction().RelatesTo.Key == "👍"
	})
	if edit.RoomID != dm.RoomID || react.RoomID != dm.RoomID {
		t.Errorf("the edit went to %s and the reaction to %s; want both in the private room %s", edit.RoomID, react.RoomID, dm.RoomID)
	}

	// A second one reuses the room.
	alice.send(t, ctx, room, "!whisper")
	for {
		alice.mu.Lock()
		n := 0
		var rooms []id.RoomID
		for _, evt := range alice.got {
			if bodyIs("just for you")(evt) {
				n++
				rooms = append(rooms, evt.RoomID)
			}
		}
		alice.mu.Unlock()
		if n == 2 {
			if rooms[0] != rooms[1] {
				t.Errorf("private replies went to %v; want one room", rooms)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the second private reply never arrived")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestE2EStaleCommandsAndInvites(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tg := tag()
	botName, aliceName := "bot-"+tg, "alice-"+tg
	register(t, ctx, hs, botName)
	register(t, ctx, hs, aliceName)
	botID := id.UserID("@" + botName + ":localhost")
	alice := openHuman(t, ctx, hs, aliceName)

	// Every command is stale to a bot that answers none older than a
	// nanosecond, and it joins only rooms alice did not make second.
	var invites sync.Map
	s := startBot(t, ctx, hs, botName, matrix.Options{
		MaxCommandAge: time.Nanosecond,
		AcceptInvite: func(room, inviter string) bool {
			_, refused := invites.Load(room)
			return !refused && inviter == string(alice.UserID)
		},
	})
	room := createRoom(t, ctx, alice, botID, false)
	alice.send(t, ctx, room, "!ping")
	alice.send(t, ctx, room, "after")
	s.wait(t, ctx, "the message after the command", func(s *seen) bool {
		return len(s.messages) > 0 && s.messages[len(s.messages)-1].Text == "after"
	})
	alice.mu.Lock()
	for _, evt := range alice.got {
		if bodyIs("pong")(evt) {
			t.Error("the bot answered a stale command")
		}
	}
	alice.mu.Unlock()
	for _, m := range s.messages {
		if m.Text == "!ping" {
			t.Error("a stale command reached the message handlers")
		}
	}

	// An invite AcceptInvite refuses is left alone.
	resp, err := alice.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "private_chat"})
	if err != nil {
		t.Fatal(err)
	}
	invites.Store(string(resp.RoomID), true)
	if _, err := alice.InviteUser(ctx, resp.RoomID, &mautrix.ReqInviteUser{UserID: botID}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if m, _ := alice.StateStore.GetMember(ctx, resp.RoomID, botID); m != nil && m.Membership == event.MembershipJoin {
		t.Error("the bot joined a room AcceptInvite refused")
	}
}
