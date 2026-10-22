//go:build e2e

package matrix_test

import (
	"bytes"
	"context"
	"errors"
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
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut"
	"terva.sh/airut/config"
	"terva.sh/airut/matrix"
)

// TestE2EVerify verifies a bot to its owner on Synapse, as an operator
// runs it on the bot's host. The owner's side is a mautrix verification
// helper standing in for Element: it accepts the request, shows its
// emoji, and confirms them. The operator's Confirm compares the emoji
// Verify printed with the owner's.
func TestE2EVerify(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tg := tag()
	botName, ownerName := "bot-"+tg, "owner-"+tg
	register(t, ctx, hs, botName)
	register(t, ctx, hs, ownerName)
	botID, ownerID := id.UserID("@"+botName+":localhost"), "@"+ownerName+":localhost"
	cfg := &config.Matrix{Homeserver: hs, UserID: string(botID), Password: config.NewSecret("pw-" + botName),
		StateDir: t.TempDir(), DeviceName: "airut e2e verify"}

	// Before the bot has its cross-signing keys or a direct chat, nothing
	// is sent, and the operator is told what to do first.
	noAnswer := func(context.Context) (bool, error) {
		t.Error("asked to confirm before a request was sent")
		return false, nil
	}
	var out bytes.Buffer
	if _, err := matrix.Verify(ctx, cfg, ownerID, matrix.VerifyIO{Out: &out, Confirm: noAnswer}); !errors.Is(err, matrix.ErrNoDirectRoom) {
		t.Fatalf("Verify with no direct chat = %v\n%s", err, out.String())
	}
	if _, err := matrix.CrossSign(ctx, cfg, matrix.CrossSignIO{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}

	owner := openOwner(t, ctx, hs, ownerName)

	// The bot runs, and the owner opens a direct chat with it, as from
	// Element.
	stop := runVerifyBot(t, ctx, cfg)
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
	for {
		listed := false
		for _, r := range readDirect(t, ctx, hs, botName)[id.UserID(ownerID)] {
			listed = listed || r == resp.RoomID
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

	// While the bot runs, the verification refuses to start.
	out.Reset()
	if _, err := matrix.Verify(ctx, cfg, ownerID, matrix.VerifyIO{Out: &out, Confirm: noAnswer}); !errors.Is(err, rihma.ErrSyncInProgress) || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("Verify while the bot runs = %v", err)
	}
	stop()

	// The bot itself is refused.
	if _, err := matrix.Verify(ctx, cfg, string(botID), matrix.VerifyIO{Out: &out, Confirm: noAnswer}); !errors.Is(err, rihma.ErrOwnUser) {
		t.Errorf("Verify of the bot itself = %v", err)
	}

	// With the bot stopped, the owner accepts, both compare, and both
	// confirm.
	ownerKey, err := owner.OwnMasterKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	term := &lockedBuffer{}
	confirms := 0
	got, err := matrix.Verify(ctx, cfg, ownerID, matrix.VerifyIO{Out: term, Confirm: func(ctx context.Context) (bool, error) {
		confirms++
		theirs, err := owner.emoji(ctx)
		if err != nil {
			return false, err
		}
		shown := term.String()
		for i, e := range theirs {
			if !strings.Contains(shown, fmt.Sprintf("%d. %s  %s\n", i+1, e.Emoji, e.Description)) {
				return false, nil
			}
		}
		if err := owner.confirm(ctx); err != nil {
			return false, err
		}
		time.Sleep(2 * time.Second) // the owner's MAC reaches the bot first
		return true, nil
	}})
	if err != nil || got.MasterKey != string(ownerKey) || got.User != ownerID || confirms != 1 {
		t.Fatalf("Verify = %+v (owner's key %v), %v, %d confirms\n%s", got, got.MasterKey == string(ownerKey), err, confirms, term.String())
	}
	for _, w := range []string{"Accept it in Element", "is verified"} {
		if !strings.Contains(term.String(), w) {
			t.Errorf("the terminal lacks %q:\n%s", w, term.String())
		}
	}
	if ident, err := owner.UserIdentity(ctx, botID); err != nil || !ident.SignedByUs {
		t.Error("the owner did not sign the bot")
	}
	bot := openState(t, ctx, cfg)
	if ident, err := bot.UserIdentity(ctx, id.UserID(ownerID)); err != nil || !ident.SignedByUs {
		t.Error("the bot did not sign the owner")
	}
}

// verifyOwner is the owner's client, with a verification helper that
// accepts the bot's request, as Element does when the owner clicks.
type verifyOwner struct {
	*rihma.Client
	helper *verificationhelper.VerificationHelper
	cb     *ownerCallbacks
	quiet  context.Context
}

func openOwner(t *testing.T, ctx context.Context, hs, localpart string) *verifyOwner {
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
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	o := &verifyOwner{Client: c, cb: &ownerCallbacks{
		requested: make(chan id.VerificationTransactionID, 4),
		shown:     make(chan []rihma.SASEmoji, 4),
	}, quiet: zerolog.Nop().WithContext(ctx)}
	o.helper = verificationhelper.NewVerificationHelper(c.Client, c.OlmMachine(), verificationhelper.NewInMemoryVerificationStore(), o.cb, false, false, true)
	if err := o.helper.Init(o.quiet); err != nil {
		t.Fatal(err)
	}
	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { c.Sync(syncCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; c.Close() })
	// Accept the bot's request when it comes.
	go func() {
		for {
			select {
			case txn := <-o.cb.requested:
				o.cb.mu.Lock()
				o.cb.txn = txn
				o.cb.mu.Unlock()
				o.helper.AcceptVerification(o.quiet, txn)
			case <-syncCtx.Done():
				return
			}
		}
	}()
	return o
}

// emoji waits for the emoji the owner's client shows.
func (o *verifyOwner) emoji(ctx context.Context) ([]rihma.SASEmoji, error) {
	select {
	case e := <-o.cb.shown:
		return e, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// confirm is the owner pressing "They match".
func (o *verifyOwner) confirm(ctx context.Context) error {
	o.cb.mu.Lock()
	txn := o.cb.txn
	o.cb.mu.Unlock()
	return o.helper.ConfirmSAS(o.quiet, txn)
}

// ownerCallbacks only enqueue, because the helper may hold its lock
// while it calls them.
type ownerCallbacks struct {
	requested chan id.VerificationTransactionID
	shown     chan []rihma.SASEmoji
	mu        sync.Mutex
	txn       id.VerificationTransactionID
}

func (c *ownerCallbacks) VerificationRequested(_ context.Context, txn id.VerificationTransactionID, _ id.UserID, _ id.DeviceID) {
	select {
	case c.requested <- txn:
	default:
	}
}

func (c *ownerCallbacks) VerificationReady(context.Context, id.VerificationTransactionID, id.DeviceID, bool, bool, *verificationhelper.QRCode) {
}

func (c *ownerCallbacks) VerificationCancelled(context.Context, id.VerificationTransactionID, event.VerificationCancelCode, string) {
}

func (c *ownerCallbacks) VerificationDone(context.Context, id.VerificationTransactionID, event.VerificationMethod) {
}

func (c *ownerCallbacks) ShowSAS(_ context.Context, _ id.VerificationTransactionID, emojis []rune, descriptions []string, _ []int) {
	out := make([]rihma.SASEmoji, len(emojis))
	for i, e := range emojis {
		out[i] = rihma.SASEmoji{Emoji: string(e), Description: descriptions[i]}
	}
	select {
	case c.shown <- out:
	default:
	}
}

// runVerifyBot runs a bot that joins any room it is invited to on the
// account cfg describes, and returns a function that stops it.
func runVerifyBot(t *testing.T, ctx context.Context, cfg *config.Matrix) func() {
	t.Helper()
	a := matrix.New(cfg, matrix.Options{AcceptInvite: func(matrix.Invite) bool { return true }})
	bot := airut.New(airut.Config{Logger: slog.New(slog.DiscardHandler)})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- bot.Run(runCtx, a) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run = %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// lockedBuffer is a terminal the test reads while Verify writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
