//go:build e2e

package matrix_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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

// TestE2EVerifyInChat verifies a running bot to its owner from chat, on
// Synapse. The owner's side is a rihma client with in-room SAS, which
// starts the verification in the owner's encrypted direct chat with the
// bot, as Element does from the bot's profile. The bot posts the emoji
// there; the owner compares them with its own, answers yes in the chat,
// and confirms. The answer never reaches the bot's handlers.
//
// "as Element" sends the way Element Web does (checked against 1.12.30
// in rihma PR #65): every verification event in plaintext, even in the
// encrypted DM, while ordinary chat, the yes among it, is encrypted. A
// plaintext yes from the owner first is chat, not the answer. "all
// encrypted" sends everything encrypted.
func TestE2EVerifyInChat(t *testing.T) {
	hs := homeserver(t)
	t.Run("as Element", func(t *testing.T) { liveVerifyInChat(t, hs, true) })
	t.Run("all encrypted", func(t *testing.T) { liveVerifyInChat(t, hs, false) })
}

func liveVerifyInChat(t *testing.T, hs string, asElement bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tg := tag()
	botName, ownerName := "icbot-"+tg, "icowner-"+tg
	register(t, ctx, hs, botName)
	register(t, ctx, hs, ownerName)
	botID, ownerID := id.UserID("@"+botName+":localhost"), id.UserID("@"+ownerName+":localhost")
	cfg := &config.Matrix{Homeserver: hs, UserID: string(botID), Password: config.NewSecret("pw-" + botName),
		StateDir: t.TempDir(), DeviceName: "airut e2e in-chat verify", VerifyInChat: true}
	if _, err := matrix.CrossSign(ctx, cfg, matrix.CrossSignIO{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}

	owner := openChatOwner(t, ctx, hs, ownerName, botID)
	ownerKey, err := owner.OwnMasterKey(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The bot, whose only owner is the owner, and which records every
	// message its handlers see.
	logs := &lockedBuffer{}
	b := airut.New(airut.Config{
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
		Owners: map[airut.Platform][]string{airut.Matrix: {string(ownerID)}},
	})
	var seenMu sync.Mutex
	var seen []string
	b.OnMessage(func(_ *airut.Ctx, m airut.MessageEvent) error {
		seenMu.Lock()
		seen = append(seen, m.Text)
		seenMu.Unlock()
		return nil
	})
	a := matrix.New(cfg, matrix.Options{
		AcceptInvite: func(matrix.Invite) bool { return true },
		Owners:       func(ctx context.Context) ([]string, error) { return b.Owners(ctx, airut.Matrix) },
	})
	runCtx, stopBot := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- b.Run(runCtx, a) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			stopBot()
			if err := <-done; err != nil {
				t.Errorf("Run = %v", err)
			}
		}
	}
	defer stop()

	// The owner opens a direct chat with the bot, as from Element.
	resp, err := owner.CreateRoom(ctx, &mautrix.ReqCreateRoom{
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
		listed := false
		for _, r := range readDirect(t, ctx, hs, botName)[ownerID] {
			listed = listed || r == dm
		}
		if listed {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the bot never recorded the direct chat")
		case <-time.After(200 * time.Millisecond):
		}
	}
	// The owner's client learns that the bot joined before it asks.
	time.Sleep(2 * time.Second)
	if asElement {
		// Test code only, as rihma's docs/verification.md gives it: once
		// the owner has synced, every event this client sends through
		// SendMessageEvent is plaintext, the verification events among
		// them, and it still decrypts what it receives.
		owner.Client.Crypto = nil
	}

	txn, err := owner.sas.StartInRoom(ctx, dm, botID)
	if err != nil {
		t.Fatal("starting the verification:", err)
	}
	showing := owner.waitState(t, ctx, txn, rihma.SASShowing)

	// The bot posts the owner's emoji, in order, in the direct chat.
	shown := owner.waitMessage(t, ctx, dm, "Compare these")
	for i, e := range showing.Emoji {
		if !strings.Contains(shown, fmt.Sprintf("%d. %s %s", i+1, e.Emoji, e.Description)) {
			t.Fatalf("the bot's emoji differ from the owner's at %d:\n%s", i+1, shown)
		}
	}
	if !strings.Contains(shown, "trusts the homeserver") {
		t.Errorf("the emoji message does not say it trusts the homeserver:\n%s", shown)
	}

	yes := &event.MessageEventContent{MsgType: event.MsgText, Body: "yes"}
	wantSeen := 0
	if asElement {
		// A plaintext yes, which the homeserver could have written, is
		// chat: it reaches the bot's handlers and confirms nothing.
		if _, err := owner.SendMessageEvent(ctx, dm, event.EventMessage, yes, mautrix.ReqSendEvent{DontEncrypt: true}); err != nil {
			t.Fatal(err)
		}
		wantSeen = 1
		for seenYes(&seenMu, &seen) < 1 {
			select {
			case <-ctx.Done():
				t.Fatal("the plaintext yes never reached the bot as chat")
			case <-time.After(200 * time.Millisecond):
			}
		}
		if owner.hasMessage(dm, "Confirmed on my side") {
			t.Fatal("a plaintext yes confirmed the verification")
		}
	}

	// The owner answers yes in the chat, encrypted as Element encrypts
	// chat, and confirms in its client.
	if err := owner.sendEncrypted(ctx, dm, yes); err != nil {
		t.Fatal(err)
	}
	owner.waitMessage(t, ctx, dm, "Confirmed on my side")
	if err := owner.sas.Confirm(ctx, txn, true); err != nil {
		t.Fatal("the owner's confirm:", err)
	}
	owner.waitState(t, ctx, txn, rihma.SASDone)
	verified := owner.waitMessage(t, ctx, dm, "Verified")
	if !strings.Contains(strings.ReplaceAll(verified, " ", ""), string(ownerKey)) {
		t.Errorf("the outcome does not name the owner's key:\n%s", verified)
	}

	stop()
	if n := seenYes(&seenMu, &seen); n != wantSeen {
		t.Errorf("the bot's handlers saw %d yes, want %d: the encrypted answer must reach none", n, wantSeen)
	}
	for _, leak := range []string{showing.Emoji[0].Description, "Compare these"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("the bot's log holds %q", leak)
		}
	}
	if !strings.Contains(logs.String(), "outcome=done") {
		t.Errorf("the bot's log does not record the outcome:\n%s", logs.String())
	}
	if ident, err := owner.UserIdentity(ctx, botID); err != nil || !ident.SignedByUs {
		t.Error("the owner did not sign the bot")
	}
	bot := openState(t, ctx, cfg)
	if ident, err := bot.UserIdentity(ctx, ownerID); err != nil || !ident.SignedByUs {
		t.Error("the bot did not sign the owner")
	}
}

// chatOwner is the owner's client, with in-room SAS for the bot, and the
// texts of the messages the bot sends it.
type chatOwner struct {
	*rihma.Client
	sas *rihma.SASController

	mu   sync.Mutex
	msgs map[id.RoomID][]string
	new  chan struct{}
}

func openChatOwner(t *testing.T, ctx context.Context, hs, localpart string, bot id.UserID) *chatOwner {
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
		DeviceName: "airut e2e owner",
		Logger:     zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRecoveryKey(ctx, "pw-"+localpart); err != nil {
		t.Fatal("the owner's identity:", err)
	}
	s, err := c.EnableSASWith(rihma.SASOptions{Peers: []id.UserID{bot}, InRoom: true})
	if err != nil {
		t.Fatal(err)
	}
	o := &chatOwner{Client: c, sas: s, msgs: map[id.RoomID][]string{}, new: make(chan struct{}, 1)}
	c.Handlers().OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		if evt.Sender != bot {
			return
		}
		m := evt.Content.AsMessage()
		o.mu.Lock()
		o.msgs[evt.RoomID] = append(o.msgs[evt.RoomID], m.Body)
		o.mu.Unlock()
		select {
		case o.new <- struct{}{}:
		default:
		}
	})
	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Sync(syncCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; c.Close() })
	return o
}

// waitState waits for the owner's attempt txn to reach want, and fails
// on any other end.
func (o *chatOwner) waitState(t *testing.T, ctx context.Context, txn id.VerificationTransactionID, want rihma.SASState) rihma.SASSnapshot {
	t.Helper()
	for {
		s := o.sas.Snapshot()
		if s.TransactionID == txn {
			switch {
			case s.State == want:
				return s
			case s.State == rihma.SASCancelled || s.State == rihma.SASFailed || s.State == rihma.SASDone:
				t.Fatalf("the owner's verification ended %s (%s), want %s", s.State, s.Reason, want)
			}
		}
		select {
		case <-o.sas.Changed():
		case <-time.After(time.Second):
		case <-ctx.Done():
			t.Fatalf("the owner's verification never reached %s", want)
		}
	}
}

// waitMessage waits for a message from the bot in room that holds want,
// and returns its text.
func (o *chatOwner) waitMessage(t *testing.T, ctx context.Context, room id.RoomID, want string) string {
	t.Helper()
	for {
		o.mu.Lock()
		for _, m := range o.msgs[room] {
			if strings.Contains(m, want) {
				o.mu.Unlock()
				return m
			}
		}
		o.mu.Unlock()
		select {
		case <-o.new:
		case <-time.After(time.Second):
		case <-ctx.Done():
			t.Fatalf("the bot never posted %q", want)
		}
	}
}

// seenYes counts the yes messages the bot's handlers saw.
func seenYes(mu *sync.Mutex, seen *[]string) int {
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, m := range *seen {
		if strings.EqualFold(strings.TrimSpace(m), "yes") {
			n++
		}
	}
	return n
}

// hasMessage reports whether the bot has posted a message holding want
// in room.
func (o *chatOwner) hasMessage(room id.RoomID, want string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, m := range o.msgs[room] {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}

// sendEncrypted sends content to room encrypted, whether or not the
// client's Crypto is set, by the steps mautrix's SendMessageEvent takes
// when it is: share the room's megolm session with its members, encrypt,
// and send the m.room.encrypted event. This keeps the answer encrypted
// in the "as Element" case without putting Crypto back, which rihma's
// controller reads from its own goroutine whenever it sends.
func (o *chatOwner) sendEncrypted(ctx context.Context, room id.RoomID, content *event.MessageEventContent) error {
	members, err := o.StateStore.GetRoomJoinedOrInvitedMembers(ctx, room)
	if err != nil {
		return err
	}
	if err := o.OlmMachine().ShareGroupSession(ctx, room, members); err != nil {
		return err
	}
	enc, err := o.OlmMachine().EncryptMegolmEvent(ctx, room, event.EventMessage, content)
	if err != nil {
		return err
	}
	_, err = o.Client.Client.SendMessageEvent(ctx, room, event.EventEncrypted, enc)
	return err
}
