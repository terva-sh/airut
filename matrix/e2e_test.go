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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	c.Handlers().OnEventType(event.EphemeralEventTyping, record)
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

// reply sends text as a reply to the message to.
func (h *human) reply(t *testing.T, ctx context.Context, room id.RoomID, to id.EventID, text string) id.EventID {
	t.Helper()
	c := &event.MessageEventContent{MsgType: event.MsgText, Body: text,
		RelatesTo: &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: to}}}
	resp, err := h.SendMessageEvent(ctx, room, event.EventMessage, c)
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
	// chains are the reply chains the chain command saw.
	chains [][]airut.Quoted
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
	// Only the bot's counterpart Alice, named by the same tag, may start
	// the slow agent turn the work command runs.
	alice := "@" + strings.Replace(localpart, "bot-", "alice-", 1) + ":localhost"
	b := airut.New(airut.Config{
		Logger: log, AskTimeout: 30 * time.Second,
		Agent: slowAgent{},
		Gate:  airut.Gate{Access: map[airut.Platform]airut.Access{airut.Matrix: {Users: []string{alice}}}},
		// Typing at once, renewed well inside its 30 seconds.
		TypingDelay: -1, TypingEvery: 2 * time.Second,
	})
	s := &seen{}
	b.Command("work", "Run a slow agent turn", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "work"}).StreamTo(c)
	})
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
	b.Command("bundle", "Ask three things at once", func(c *airut.Ctx) error {
		got, err := c.AskAll(airut.Bundle{
			Intro: "Before I retag these:",
			Questions: []airut.Question{
				{Key: "post", Text: "Which post?", Choices: []string{"the troll", "the Tolkien one"}},
				{Key: "rating", Text: "Keep the rating?", Choices: []string{"yes", "no"}},
				{Key: "why", Text: "Why?"},
			},
		})
		if err != nil {
			return err
		}
		_, err = c.Reply("got " + got["post"].Text + " | " + got["rating"].Text + " | " + got["why"].Text + " from " + got["why"].By.ID)
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
	b.Command("post", "Reply, then edit the reply, as a streamed answer is", func(c *airut.Ctx) error {
		ref, err := c.Reply("draft")
		if err != nil {
			return err
		}
		return c.Edit(ref, "final post")
	})
	b.Command("chain", "Record the reply chain", func(c *airut.Ctx) error {
		s.mu.Lock()
		s.chains = append(s.chains, c.ReplyChain())
		s.mu.Unlock()
		return nil
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
	if o.AcceptInvite == nil && o.CheckInvite == nil {
		o.AcceptInvite = func(matrix.Invite) bool { return true }
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
		for _, leak := range []string{"pw-" + localpart, "just for you", "final words", "hello there", "Proceed?", "final post", "which post?"} {
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

// slowAgent works for three seconds and then answers.
type slowAgent struct{}

func (slowAgent) Run(c *airut.Ctx, _ airut.Turn, out airut.TurnOutput) error {
	select {
	case <-time.After(3 * time.Second):
	case <-c.Context().Done():
		return c.Context().Err()
	}
	out.Text("worked")
	return nil
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

// TestE2EKeyedSendAcrossARestart sends a keyed message with a file,
// restarts the bot on the same state, as the same device, and sends it
// again, as a bot resuming work after a crash would. The room gets one
// text event and one file event.
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
			keyed := airut.Message{Text: "once only", Key: "e2e/" + tg,
				Files: []airut.File{{Name: "once.txt", ContentType: "text/plain", Data: []byte("one file")}}}

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
			texts, files := 0, 0
			for _, evt := range alice.got {
				if bodyIs("once only")(evt) {
					texts++
				}
				if bodyIs("once.txt")(evt) {
					files++
				}
			}
			alice.mu.Unlock()
			if texts != 1 || files != 1 {
				t.Errorf("a keyed message sent before and after a restart arrived as %d text and %d file events, want 1 and 1", texts, files)
			}
		})
	}
}

// TestE2EConnReactions swaps a status reaction through the Conn, as a
// pipeline outside any invocation does, and checks that Unreact redacts
// only the bot's own reactions.
func TestE2EConnReactions(t *testing.T) {
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
			ch := airut.Channel{Platform: airut.Matrix, ID: string(room)}
			s.wait(t, ctx, "the Conn", func(s *seen) bool { return s.conn != nil })
			conn := s.conn
			picture := airut.MessageRef{ID: string(alice.send(t, ctx, room, "a picture"))}
			reacted := func(key string) func(*event.Event) bool {
				return func(evt *event.Event) bool {
					return evt.Type == event.EventReaction && evt.Sender == botID && evt.Content.AsReaction().RelatesTo.Key == key
				}
			}
			redacted := func(ref airut.MessageRef) bool {
				evt, err := alice.GetEvent(ctx, room, id.EventID(ref.ID))
				if err != nil {
					t.Fatal(err)
				}
				return evt.Unsigned.RedactedBecause != nil
			}

			seen, err := conn.React(ctx, ch, picture, "👀")
			if err != nil {
				t.Fatal(err)
			}
			alice.waitFor(t, ctx, "the bot's 👀", reacted("👀"))
			// The same reaction again, as after a restart that fell
			// before the ref was recorded, returns the standing one.
			if again, err := conn.React(ctx, ch, picture, "👀"); err != nil || again != seen {
				t.Errorf("reacting again = %v, %v; want %v", again, err, seen)
			}
			if err := conn.Unreact(ctx, ch, seen); err != nil {
				t.Fatal(err)
			}
			if !redacted(seen) {
				t.Error("the retracted reaction was not redacted")
			}
			if err := conn.Unreact(ctx, ch, seen); err != nil {
				t.Errorf("retracting it again = %v", err)
			}
			done, err := conn.React(ctx, ch, picture, "✅")
			if err != nil || done == seen {
				t.Fatalf("the swap = %v, %v", done, err)
			}
			alice.waitFor(t, ctx, "the bot's ✅", reacted("✅"))
			if back, err := conn.React(ctx, ch, picture, "👀"); err != nil || back == seen {
				t.Errorf("reacting with a retracted emoji = %v, %v; want a new reaction", back, err)
			}

			// The standing reaction is found however many reactions came
			// after it: here three of hers, read one a page.
			for _, e := range []string{"🐈", "🐕", "🐇"} {
				if _, err := alice.SendReaction(ctx, room, id.EventID(picture.ID), e); err != nil {
					t.Fatal(err)
				}
			}
			defer matrix.SetRelationPage(1)()
			if again, err := conn.React(ctx, ch, picture, "✅"); err != nil || again != done {
				t.Errorf("reacting again past a page of reactions = %v, %v; want %v", again, err, done)
			}

			// Nothing but the bot's own reaction is redacted.
			hers, err := alice.SendReaction(ctx, room, id.EventID(picture.ID), "👍")
			if err != nil {
				t.Fatal(err)
			}
			mine, err := conn.Send(ctx, ch, airut.Message{Text: "a post of the bot's"})
			if err != nil {
				t.Fatal(err)
			}
			for name, ref := range map[string]airut.MessageRef{
				"her message": picture, "her reaction": {ID: string(hers.EventID)}, "the bot's message": mine, "no event": {},
			} {
				if err := conn.Unreact(ctx, ch, ref); !errors.Is(err, airut.ErrNotOwnReaction) {
					t.Errorf("Unreact(%s) = %v, want ErrNotOwnReaction", name, err)
				}
				if ref.ID != "" && redacted(ref) {
					t.Errorf("%s was redacted", name)
				}
			}
		})
	}
}

// TestE2EReplyChain checks that a message that replies to others
// arrives with what it replies to, read as it reads now: the bot's own
// streamed answer by its last edit, and decrypted in an encrypted room.
// Fetch and History read the bot's messages the same way.
func TestE2EReplyChain(t *testing.T) {
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
			ch := airut.Channel{Platform: airut.Matrix, ID: string(room)}

			// The bot answers, in a reply, then edits its answer.
			command := alice.send(t, ctx, room, "!post")
			post := alice.waitFor(t, ctx, "the draft", bodyIs("draft")).ID
			alice.waitFor(t, ctx, "the edit", func(evt *event.Event) bool {
				c := evt.Content.AsMessage()
				return evt.Type == event.EventMessage && c.NewContent != nil && c.NewContent.Body == "final post"
			})
			isPost := func(q airut.Quoted) bool {
				return q.Ref.ID == string(post) && q.Text == "final post" && q.FromBot && q.Sender.ID == string(botID)
			}
			isCommand := func(q airut.Quoted) bool {
				return q.Ref.ID == string(command) && q.Text == "!post" && !q.FromBot && q.Sender.ID == string(alice.UserID)
			}

			// Chat that mentions the bot, replying to its answer.
			question := alice.reply(t, ctx, room, post, botName+": which post?")
			s.wait(t, ctx, "the mention", func(s *seen) bool {
				return len(s.messages) > 0 && s.messages[len(s.messages)-1].Message.ID == string(question)
			})
			s.mu.Lock()
			if got := s.messages[len(s.messages)-1].ReplyChain; len(got) != 2 || !isPost(got[0]) || !isCommand(got[1]) {
				t.Errorf("the mention's chain = %+v, want the bot's edited answer and the command it answered", got)
			}
			s.mu.Unlock()

			// A command replying to that sees all three, nearest first,
			// which is the default depth.
			alice.reply(t, ctx, room, question, "!chain")
			s.wait(t, ctx, "the chain command", func(s *seen) bool { return len(s.chains) == 1 })
			s.mu.Lock()
			got := s.chains[0]
			s.mu.Unlock()
			if len(got) != 3 || got[0].Ref.ID != string(question) || got[0].Text != botName+": which post?" ||
				got[0].FromBot || got[0].Sender.ID != string(alice.UserID) || !isPost(got[1]) || !isCommand(got[2]) {
				t.Errorf("the command's chain = %+v", got)
			}

			// Fetch and History read the bot's answer as it reads now.
			s.wait(t, ctx, "the Conn", func(s *seen) bool { return s.conn != nil })
			if m, err := s.conn.Fetch(ctx, ch, airut.MessageRef{ID: string(post)}); err != nil || m.Text != "final post" || !m.FromBot {
				t.Errorf("Fetch = %+v, %v", m, err)
			}
			if m, err := s.conn.Fetch(ctx, ch, airut.MessageRef{ID: string(question)}); err != nil || m.FromBot || m.Sender.ID != string(alice.UserID) {
				t.Errorf("Fetch of hers = %+v, %v", m, err)
			}
			find := func(opts ...airut.HistoryOption) (airut.MessageEvent, bool) {
				page, err := s.conn.History(ctx, ch, "", 50, opts...)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range page.Messages {
					if m.Message.ID == string(post) {
						return m, true
					}
				}
				return airut.MessageEvent{}, false
			}
			if _, ok := find(); ok {
				t.Error("History returned the bot's own message without IncludeBot")
			}
			if m, ok := find(airut.IncludeBot); !ok || m.Text != "final post" || !m.FromBot {
				t.Errorf("History with IncludeBot = %+v, %v; want the edited answer", m, ok)
			}

			// Edits by anyone else do not count, however many come after
			// the bot's own: here two of hers, read one a page.
			for range 2 {
				if _, err := alice.SendMessageEvent(ctx, room, event.EventMessage, &event.MessageEventContent{
					MsgType: event.MsgText, Body: "* not the bot's words",
					NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: "not the bot's words"},
					RelatesTo:  &event.RelatesTo{Type: event.RelReplace, EventID: post},
				}); err != nil {
					t.Fatal(err)
				}
			}
			defer matrix.SetEditPage(1)()
			if m, err := s.conn.Fetch(ctx, ch, airut.MessageRef{ID: string(post)}); err != nil || m.Text != "final post" {
				t.Errorf("Fetch past a page of her edits = %+v, %v; want the bot's own edit", m, err)
			}
		})
	}
}

// TestE2EDirectRoom opens a direct chat with the bot as a client does,
// and checks that the bot takes every message there as addressed to it,
// and stops the moment a third person is invited.
func TestE2EDirectRoom(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tg := tag()
	botName, aliceName, carolName := "bot-"+tg, "alice-"+tg, "carol-"+tg
	for _, n := range []string{botName, aliceName, carolName} {
		register(t, ctx, hs, n)
	}
	botID := id.UserID("@" + botName + ":localhost")
	// The bot reads its m.direct afresh for every check, so a change from
	// another client shows at once. Registered before the bot starts, the
	// restore runs after it stops.
	t.Cleanup(matrix.SetDirectReload(0))
	var invites sync.Map
	s := startBot(t, ctx, hs, botName, matrix.Options{AcceptInvite: func(inv matrix.Invite) bool {
		invites.Store(inv.Room, inv.Direct)
		return true
	}})
	alice := openHuman(t, ctx, hs, aliceName)
	s.wait(t, ctx, "the Conn", func(s *seen) bool { return s.conn != nil })

	// A direct chat, as a client opens one.
	resp, err := alice.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Preset: "trusted_private_chat", IsDirect: true, Invite: []id.UserID{botID},
		InitialState: []*event.Event{{
			Type:    event.StateEncryption,
			Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dm := resp.RoomID
	for {
		m, err := alice.StateStore.GetMember(ctx, dm, botID)
		if err == nil && m != nil && m.Membership == event.MembershipJoin {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the bot never joined the direct chat")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if direct, _ := invites.Load(string(dm)); direct != true {
		t.Errorf("the invite to a direct chat arrived with Direct %v", direct)
	}
	// A plain private room with the same two people is not direct.
	group := createRoom(t, ctx, alice, botID, true)

	last := func(text string) func(*seen) bool {
		return func(s *seen) bool { return len(s.messages) > 0 && s.messages[len(s.messages)-1].Text == text }
	}
	ping := alice.send(t, ctx, dm, "ping")
	alice.waitFor(t, ctx, "a pong for a bare command", func(evt *event.Event) bool { return bodyIs("pong")(evt) && replyTo(evt) == ping })
	alice.send(t, ctx, dm, "hello there")
	s.wait(t, ctx, "the chat in the direct chat", last("hello there"))
	s.mu.Lock()
	if m := s.messages[len(s.messages)-1]; !m.Direct || !m.Mentioned || m.Request != "hello there" {
		t.Errorf("chat in a direct chat arrived with Direct %v, Mentioned %v, Request %q", m.Direct, m.Mentioned, m.Request)
	}
	s.mu.Unlock()
	alice.send(t, ctx, group, "in the group")
	s.wait(t, ctx, "the chat in the plain room", last("in the group"))
	s.mu.Lock()
	if m := s.messages[len(s.messages)-1]; m.Direct || m.Mentioned {
		t.Errorf("chat in a plain private room arrived with Direct %v, Mentioned %v", m.Direct, m.Mentioned)
	}
	s.mu.Unlock()
	conn := s.conn
	if who, ok, err := conn.Direct(ctx, airut.Channel{Platform: airut.Matrix, ID: string(dm)}); err != nil || !ok || who.ID != string(alice.UserID) {
		t.Errorf("Conn.Direct(the direct chat) = %v, %v, %v", who, ok, err)
	}
	if _, ok, err := conn.Direct(ctx, airut.Channel{Platform: airut.Matrix, ID: string(group)}); err != nil || ok {
		t.Errorf("Conn.Direct(the plain room) = %v, %v", ok, err)
	}

	// A room another client takes out of m.direct is no longer direct,
	// and is again once it is put back.
	setDirect(t, ctx, hs, botName, event.DirectChatsEventContent{})
	alice.send(t, ctx, dm, "after unlisting")
	s.wait(t, ctx, "the chat after unlisting", last("after unlisting"))
	s.mu.Lock()
	if m := s.messages[len(s.messages)-1]; m.Direct || m.Mentioned {
		t.Errorf("chat in a room taken out of m.direct arrived with Direct %v, Mentioned %v", m.Direct, m.Mentioned)
	}
	s.mu.Unlock()
	setDirect(t, ctx, hs, botName, event.DirectChatsEventContent{alice.UserID: {dm}})
	alice.send(t, ctx, dm, "after relisting")
	s.wait(t, ctx, "the chat after relisting", last("after relisting"))
	s.mu.Lock()
	if m := s.messages[len(s.messages)-1]; !m.Direct {
		t.Error("chat in a room put back in m.direct arrived without Direct")
	}
	s.mu.Unlock()

	// Once a third person is invited, the room is no longer direct.
	if _, err := alice.InviteUser(ctx, dm, &mautrix.ReqInviteUser{UserID: id.UserID("@" + carolName + ":localhost")}); err != nil {
		t.Fatal(err)
	}
	alice.send(t, ctx, dm, "after the invite")
	s.wait(t, ctx, "the chat after the invite", last("after the invite"))
	s.mu.Lock()
	if m := s.messages[len(s.messages)-1]; m.Direct || m.Mentioned {
		t.Errorf("chat after a third person was invited arrived with Direct %v, Mentioned %v", m.Direct, m.Mentioned)
	}
	s.mu.Unlock()
	if _, ok, err := conn.Direct(ctx, airut.Channel{Platform: airut.Matrix, ID: string(dm)}); err != nil || ok {
		t.Errorf("Conn.Direct after a third person was invited = %v, %v", ok, err)
	}
}

// edited matches the bot's edit of ev to text holding want.
func edited(ev id.EventID, want string) func(*event.Event) bool {
	return func(evt *event.Event) bool {
		c := evt.Content.AsMessage()
		return evt.Type == event.EventMessage && c.RelatesTo.GetReplaceID() == ev && c.NewContent != nil && strings.Contains(c.NewContent.Body, want)
	}
}

func TestE2EBundle(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			hs := homeserver(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			tg := tag()
			botName, aliceName, carolName := "bot-"+tg, "alice-"+tg, "carol-"+tg
			for _, n := range []string{botName, aliceName, carolName} {
				register(t, ctx, hs, n)
			}
			botID := id.UserID("@" + botName + ":localhost")
			s := startBot(t, ctx, hs, botName, matrix.Options{})
			alice := openHuman(t, ctx, hs, aliceName)
			carol := openHuman(t, ctx, hs, carolName)
			room := createRoom(t, ctx, alice, botID, encrypted)
			if _, err := alice.InviteUser(ctx, room, &mautrix.ReqInviteUser{UserID: id.UserID("@" + carolName + ":localhost")}); err != nil {
				t.Fatal(err)
			}
			for {
				if m, err := alice.StateStore.GetMember(ctx, room, carol.UserID); err == nil && m != nil && m.Membership == event.MembershipJoin {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("carol never joined")
				case <-time.After(100 * time.Millisecond):
				}
			}

			alice.send(t, ctx, room, "!bundle")
			asked := alice.waitFor(t, ctx, "the bundle", func(evt *event.Event) bool {
				return evt.Type == event.EventMessage && strings.Contains(body(evt), "Reply to this message")
			})
			if b := body(asked); !strings.Contains(b, "1. Which post? (the troll / the Tolkien one)") || strings.Contains(b, "all yes") {
				t.Errorf("the bundle reads:\n%s", b)
			}

			// Carol may not answer, so her reply is ordinary chat.
			carol.reply(t, ctx, room, asked.ID, "1 the troll")
			s.wait(t, ctx, "carol's reply as chat", func(s *seen) bool {
				return len(s.messages) > 0 && s.messages[len(s.messages)-1].Text == "1 the troll"
			})
			// Alice answers two, then the third; each lands on the bundle.
			alice.reply(t, ctx, room, asked.ID, "1 the Tolkien one, 2 yes")
			alice.waitFor(t, ctx, "the first answers shown", edited(asked.ID, "1. Which post? Answer: the Tolkien one"))
			alice.reply(t, ctx, room, asked.ID, "3 because, of the elves")
			alice.waitFor(t, ctx, "the answers", bodyIs("got the Tolkien one | yes | because, of the elves from "+string(alice.UserID)))
			s.mu.Lock()
			for _, m := range s.messages {
				if strings.Contains(m.Text, "Tolkien") || strings.Contains(m.Text, "elves") {
					t.Errorf("an answer reached the bot's handlers as chat: %q", m.Text)
				}
			}
			s.mu.Unlock()
		})
	}
}

// typing matches a typing event that does or does not list user.
func typing(user id.UserID, listed bool) func(*event.Event) bool {
	return func(evt *event.Event) bool {
		return evt.Type == event.EphemeralEventTyping && slices.Contains(evt.Content.AsTyping().UserIDs, user) == listed
	}
}

// after returns, of the events the human has received, those after
// the first that match accepts.
func (h *human) after(match func(*event.Event) bool) []*event.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, evt := range h.got {
		if match(evt) {
			return slices.Clone(h.got[i+1:])
		}
	}
	return nil
}

func TestE2ETypingKeepalive(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tg := tag()
	botName, aliceName := "bot-"+tg, "alice-"+tg
	register(t, ctx, hs, botName)
	register(t, ctx, hs, aliceName)
	botID := id.UserID("@" + botName + ":localhost")
	startBot(t, ctx, hs, botName, matrix.Options{})
	alice := openHuman(t, ctx, hs, aliceName)
	room := createRoom(t, ctx, alice, botID, false)

	alice.send(t, ctx, room, "!work")
	alice.waitFor(t, ctx, "the bot typing", typing(botID, true))
	answer := alice.waitFor(t, ctx, "the answer", bodyIs("worked"))
	// The notice comes down when the turn ends, not when its 30 seconds
	// run out.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if slices.ContainsFunc(alice.after(func(e *event.Event) bool { return e == answer }), typing(botID, false)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the typing notice stayed up after the answer")
		}
		time.Sleep(100 * time.Millisecond)
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
		AcceptInvite: func(inv matrix.Invite) bool {
			_, refused := invites.Load(inv.Room)
			return !refused && inv.Inviter == string(alice.UserID)
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

// CheckInvite admits alice, and errors for carol while saying yes, which
// must still refuse her invite.
func TestE2ECheckInvite(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tg := tag()
	botName, aliceName, carolName := "bot-"+tg, "alice-"+tg, "carol-"+tg
	for _, n := range []string{botName, aliceName, carolName} {
		register(t, ctx, hs, n)
	}
	botID := id.UserID("@" + botName + ":localhost")
	alice := openHuman(t, ctx, hs, aliceName)
	carol := openHuman(t, ctx, hs, carolName)
	startBot(t, ctx, hs, botName, matrix.Options{CheckInvite: func(_ context.Context, inv matrix.Invite) (bool, error) {
		if inv.Inviter == string(alice.UserID) {
			return true, nil
		}
		return true, errors.New("the check could not finish")
	}})

	createRoom(t, ctx, alice, botID, false)
	resp, err := carol.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "private_chat", Invite: []id.UserID{botID}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if m, _ := carol.StateStore.GetMember(ctx, resp.RoomID, botID); m != nil && m.Membership == event.MembershipJoin {
		t.Error("the bot joined a room whose invite check failed")
	}
}

func TestE2EProfile(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tg := tag()
	botName, aliceName := "bot-"+tg, "alice-"+tg
	register(t, ctx, hs, botName)
	register(t, ctx, hs, aliceName)
	botID := id.UserID("@" + botName + ":localhost")
	s := startBot(t, ctx, hs, botName, matrix.Options{})
	alice := openHuman(t, ctx, hs, aliceName)
	s.wait(t, ctx, "the Conn", func(s *seen) bool { return s.conn != nil })
	conn := s.conn

	name := "Baphomet " + tg
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")
	if err := conn.SetProfile(ctx, airut.ProfileChange{Name: &name, Avatar: &airut.Image{Data: png, Type: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	p, err := conn.Profile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != name || !strings.HasPrefix(p.Avatar, "mxc://") || p.About != "" {
		t.Errorf("profile = %+v", p)
	}
	// Another user sees the same, and reads back the picture as sent.
	theirs, err := alice.GetProfile(ctx, botID)
	if err != nil {
		t.Fatal(err)
	}
	if theirs.DisplayName != name || theirs.AvatarURL.String() != p.Avatar {
		t.Errorf("alice sees %+v, want %q and %q", theirs, name, p.Avatar)
	}
	got, err := alice.DownloadBytes(ctx, theirs.AvatarURL)
	if err != nil || !bytes.Equal(got, png) {
		t.Errorf("the avatar downloads as %d bytes, err %v", len(got), err)
	}

	about := "The archive's librarian."
	if err := conn.SetProfile(ctx, airut.ProfileChange{Name: ptr("Changed"), About: &about}); !errors.Is(err, airut.ErrUnsupported) {
		t.Errorf("setting a description = %v, want ErrUnsupported", err)
	}
	if p, _ := conn.Profile(ctx); p.Name != name {
		t.Errorf("a refused change renamed the bot to %q", p.Name)
	}
}

func ptr[T any](v T) *T { return &v }
