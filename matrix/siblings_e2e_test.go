//go:build e2e

package matrix_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// TestE2ESiblings runs two cross-signed bots that list each other, in an
// encrypted coordination room the owner made, and requires each to sign
// the other's master key, with neither the secret nor a MAC in a log.
// The second bot is invited only after it started, so its proof follows
// its join.
func TestE2ESiblings(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	owner := "owner-" + tag()
	register(t, ctx, hs, owner)
	names := []string{"sib-a-" + tag(), "sib-b-" + tag()}
	users := make([]string, len(names))
	for i, n := range names {
		register(t, ctx, hs, n)
		users[i] = "@" + n + ":localhost"
	}
	secretBytes := make([]byte, 32)
	rand.Read(secretBytes)
	secret := hex.EncodeToString(secretBytes)

	cfgs := make([]*config.Matrix, len(names))
	for i, n := range names {
		cfgs[i] = &config.Matrix{Homeserver: hs, UserID: users[i], Password: config.NewSecret("pw-" + n),
			StateDir: t.TempDir(), DeviceName: "airut e2e sibling"}
		var out bytes.Buffer
		if st, err := matrix.CrossSign(ctx, cfgs[i], matrix.CrossSignIO{Out: &out}); err != nil || !st.DeviceSigned {
			t.Fatalf("the setup for %s = %+v, %v", n, st, err)
		}
	}

	h := openHuman(t, ctx, hs, owner)
	resp, err := h.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "private_chat", Invite: []id.UserID{id.UserID(users[0])},
		InitialState: []*event.Event{{Type: event.StateEncryption, Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	room := resp.RoomID
	joinAs(t, ctx, hs, names[0], room)
	for _, c := range cfgs {
		c.Siblings = &config.Siblings{Users: users, Room: string(room), Secret: config.NewSecret(secret)}
	}

	a := startSibling(t, ctx, cfgs[0], matrix.Options{})
	isProofFrom := func(u string) func(*event.Event) bool {
		return func(evt *event.Event) bool {
			return evt.Sender == id.UserID(u) && bytes.Contains(evt.Content.VeryRaw, []byte("sh.terva.airut.sibling_proof"))
		}
	}
	h.waitFor(t, ctx, "the first bot's proof", isProofFrom(users[0]))
	b := startSibling(t, ctx, cfgs[1], matrix.Options{AcceptInvite: func(inv matrix.Invite) bool { return inv.Room == string(room) }})
	waitLog(t, ctx, b, "not a member of the coordination room")
	if _, err := h.InviteUser(ctx, room, &mautrix.ReqInviteUser{UserID: id.UserID(users[1])}); err != nil {
		t.Fatal(err)
	}
	waitLog(t, ctx, a, `msg="signed a sibling's master key"`)
	waitLog(t, ctx, b, `msg="signed a sibling's master key"`)
	h.waitFor(t, ctx, "the second bot's proof", isProofFrom(users[1]))
	a.stop()
	b.stop()

	// Each bot's store now holds its signature of the other's key.
	for i, c := range cfgs {
		other := id.UserID(users[1-i])
		cl := openState(t, ctx, c)
		ident, err := cl.UserIdentity(ctx, other)
		cl.Close()
		if err != nil || !ident.SignedByUs {
			t.Errorf("%s's view of %s: %+v, %v", users[i], other, ident, err)
		}
	}

	// No log holds the secret or a MAC the owner saw in the room.
	leaks := []string{secret}
	h.mu.Lock()
	for _, evt := range h.got {
		var m struct {
			P *struct {
				MAC string `json:"mac"`
			} `json:"sh.terva.airut.sibling_proof"`
		}
		if json.Unmarshal(evt.Content.VeryRaw, &m) == nil && m.P != nil {
			leaks = append(leaks, m.P.MAC)
		}
	}
	h.mu.Unlock()
	if len(leaks) < 3 {
		t.Errorf("the owner saw %d proofs", len(leaks)-1)
	}
	for _, s := range []*sibling{a, b} {
		l := s.logs()
		for _, leak := range leaks {
			if strings.Contains(l, leak) {
				t.Errorf("a bot's log holds the secret or a MAC")
			}
		}
	}
}

// joinAs joins room as localpart from a throwaway device, so the bot is a
// member before it starts.
func joinAs(t *testing.T, ctx context.Context, hs, localpart string, room id.RoomID) {
	t.Helper()
	cli, err := mautrix.NewClient(hs, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Login(ctx, &mautrix.ReqLogin{Type: mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: localpart},
		Password:   "pw-" + localpart, StoreCredentials: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.JoinRoomByID(ctx, room); err != nil {
		t.Fatal(err)
	}
	cli.Logout(ctx)
}

// sibling is a running bot and its log.
type sibling struct {
	stop func()
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (s *sibling) logs() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func startSibling(t *testing.T, ctx context.Context, cfg *config.Matrix, o matrix.Options) *sibling {
	t.Helper()
	s := &sibling{}
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	bot := airut.New(airut.Config{Logger: log})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- bot.Run(runCtx, matrix.New(cfg, o)) }()
	var once sync.Once
	s.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run = %v", err)
			}
		})
	}
	t.Cleanup(s.stop)
	return s
}

func waitLog(t *testing.T, ctx context.Context, s *sibling, want string) {
	t.Helper()
	for !strings.Contains(s.logs(), want) {
		select {
		case <-ctx.Done():
			t.Fatalf("the log never said %q:\n%s", want, s.logs())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// openState opens a stopped bot's client from its state directory.
func openState(t *testing.T, ctx context.Context, cfg *config.Matrix) *rihma.Client {
	t.Helper()
	c, err := rihma.Open(ctx, rihma.Options{
		Homeserver: cfg.Homeserver,
		StateDir:   filepath.Join(cfg.StateDir, "rihma"),
		Sessions:   rihma.FileSessionStore{Path: filepath.Join(cfg.StateDir, "session.json")},
		Logger:     zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
