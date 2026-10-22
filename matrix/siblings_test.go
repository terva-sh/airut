package matrix

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut/config"
)

// The sibling protocol's tests run against fakeHS, which publishes each
// bot's master key and records who signed whom as rihma would, and
// fakeChat, a coordination room whose posts the test delivers.

const coord = id.RoomID("!coord:example.org")

var (
	fleetSecret = strings.Repeat("F", 24) + "-fleet-secret-new-0123"
	oldSecret   = strings.Repeat("O", 24) + "-fleet-secret-old-0123"
)

type fakeHS struct {
	mu        sync.Mutex
	published map[id.UserID]id.Ed25519
	// held is the master key each bot's device holds.
	held map[id.UserID]id.Ed25519
	// signed maps signer and signee to the key the signer signed.
	signed map[[2]id.UserID]id.Ed25519
	signs  int
}

func newHS() *fakeHS {
	return &fakeHS{published: map[id.UserID]id.Ed25519{}, held: map[id.UserID]id.Ed25519{}, signed: map[[2]id.UserID]id.Ed25519{}}
}

// rekey gives u a new master key, held and published.
func (hs *fakeHS) rekey(u id.UserID, k id.Ed25519) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.published[u], hs.held[u] = k, k
}

func (hs *fakeHS) signedKey(signer, signee id.UserID) id.Ed25519 {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.signed[[2]id.UserID{signer, signee}]
}

type fakeIDs struct {
	hs   *fakeHS
	self id.UserID
}

func (f fakeIDs) OwnMasterKey(context.Context) (id.Ed25519, error) {
	f.hs.mu.Lock()
	defer f.hs.mu.Unlock()
	held := f.hs.held[f.self]
	switch {
	case held == "":
		return "", rihma.ErrNoCrossSigningKeys
	case f.hs.published[f.self] != held:
		return "", rihma.ErrMasterKeyMismatch
	}
	return held, nil
}

func (f fakeIDs) UserIdentity(_ context.Context, u id.UserID) (*rihma.UserIdentity, error) {
	if u == f.self {
		return nil, rihma.ErrOwnUser
	}
	f.hs.mu.Lock()
	defer f.hs.mu.Unlock()
	pub, ok := f.hs.published[u]
	if !ok {
		return nil, rihma.ErrNoUserIdentity
	}
	return &rihma.UserIdentity{UserID: u, MasterKey: pub, SignedByUs: f.hs.signed[[2]id.UserID{f.self, u}] == pub}, nil
}

func (f fakeIDs) SignUserIdentity(_ context.Context, u id.UserID, expected id.Ed25519) error {
	if u == f.self {
		return rihma.ErrOwnUser
	}
	f.hs.mu.Lock()
	defer f.hs.mu.Unlock()
	pub, ok := f.hs.published[u]
	switch {
	case !ok:
		return rihma.ErrNoUserIdentity
	case pub != expected:
		return rihma.ErrMasterKeyMismatch
	}
	f.hs.signed[[2]id.UserID{f.self, u}] = expected
	f.hs.signs++
	return nil
}

type posted struct {
	sender id.UserID
	raw    json.RawMessage
}

// fakeChat is the coordination room. Posts queue until pump delivers
// them to every bot but the sender.
type fakeChat struct {
	mu    sync.Mutex
	queue []posted
	all   []posted
}

type fakeRoom struct {
	chat *fakeChat
	self id.UserID
	st   roomState
}

func (r *fakeRoom) state(context.Context) (roomState, error) { return r.st, nil }

func (r *fakeRoom) post(_ context.Context, m *proofMessage) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	r.chat.mu.Lock()
	defer r.chat.mu.Unlock()
	p := posted{sender: r.self, raw: raw}
	r.chat.queue = append(r.chat.queue, p)
	r.chat.all = append(r.chat.all, p)
	return nil
}

// fleet is a set of sibling bots sharing a fake homeserver, a fake room,
// a clock, and a log.
type fleet struct {
	t     *testing.T
	hs    *fakeHS
	chat  *fakeChat
	now   time.Time
	logs  *bytes.Buffer
	logMu sync.Mutex
	bots  map[id.UserID]*siblings
	list  []string
}

func newFleet(t *testing.T, users ...string) *fleet {
	f := &fleet{t: t, hs: newHS(), chat: &fakeChat{}, now: time.UnixMilli(1_800_000_000_000), logs: &bytes.Buffer{},
		bots: map[id.UserID]*siblings{}, list: users}
	for i, u := range users {
		f.hs.rekey(id.UserID(u), id.Ed25519(strings.Repeat(string(rune('A'+i)), 43)))
	}
	return f
}

func (f *fleet) Write(p []byte) (int, error) {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return f.logs.Write(p)
}

func (f *fleet) log() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return f.logs.String()
}

// bot starts the sibling protocol for u, with secret and previous.
func (f *fleet) bot(u, secret, previous string) *siblings {
	f.t.Helper()
	cfg := &config.Siblings{Users: f.list, Room: string(coord), Secret: config.NewSecret(secret)}
	if previous != "" {
		cfg.PreviousSecret = config.NewSecret(previous)
	}
	log := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})).With("bot", u)
	b := newSiblings(cfg, id.UserID(u), f.t.TempDir(), log)
	b.now = func() time.Time { return f.now }
	b.ids = fakeIDs{hs: f.hs, self: id.UserID(u)}
	b.rm = &fakeRoom{chat: f.chat, self: id.UserID(u), st: roomState{Joined: true, Encrypted: true, JoinRule: event.JoinRuleInvite}}
	f.bots[id.UserID(u)] = b
	return b
}

// pump delivers every queued post, and those its delivery makes, and
// returns how many it delivered.
func (f *fleet) pump() int {
	n := 0
	for {
		f.chat.mu.Lock()
		if len(f.chat.queue) == 0 {
			f.chat.mu.Unlock()
			return n
		}
		p := f.chat.queue[0]
		f.chat.queue = f.chat.queue[1:]
		f.chat.mu.Unlock()
		n++
		for u, b := range f.bots {
			if u != p.sender {
				b.handle(context.Background(), p.sender, p.raw)
			}
		}
	}
}

func (f *fleet) posts() []posted {
	f.chat.mu.Lock()
	defer f.chat.mu.Unlock()
	return append([]posted(nil), f.chat.all...)
}

func proofOf(t *testing.T, p posted) *proof {
	t.Helper()
	var m proofMessage
	if err := json.Unmarshal(p.raw, &m); err != nil || m.Proof == nil {
		t.Fatalf("not a proof: %s, %v", p.raw, err)
	}
	return m.Proof
}

const (
	alice = "@alice:example.org"
	bob   = "@bob:example.org"
	carol = "@carol:example.org"
)

// TestSiblingsSignMutually starts two siblings: the first posts a
// request, the second signs it and answers, and the first signs the
// answer. The answer is not answered, and neither posts at its next
// start.
func TestSiblingsSignMutually(t *testing.T) {
	f := newFleet(t, alice, bob)
	a, b := f.bot(alice, fleetSecret, ""), f.bot(bob, fleetSecret, "")
	a.start(context.Background())
	if n := f.pump(); n != 2 {
		t.Fatalf("delivered %d posts, want a request and its answer\n%s", n, f.log())
	}
	if f.hs.signedKey(bob, alice) != f.hs.published[alice] || f.hs.signedKey(alice, bob) != f.hs.published[bob] {
		t.Fatalf("signed: %v\n%s", f.hs.signed, f.log())
	}
	ps := f.posts()
	req, ans := proofOf(t, ps[0]), proofOf(t, ps[1])
	if ps[0].sender != alice || req.Answers != "" || ps[1].sender != bob || ans.Answers != req.Nonce {
		t.Errorf("request %+v from %s, answer %+v from %s", req, ps[0].sender, ans, ps[1].sender)
	}
	var m proofMessage
	json.Unmarshal(ps[0].raw, &m)
	if m.MsgType != event.MsgNotice || !strings.Contains(m.Body, alice) || !strings.Contains(m.Body, spaced(string(f.hs.published[alice]))) {
		t.Errorf("the request's body: %+v", m)
	}

	// Both signed, both posted their key: a restart posts nothing.
	a.start(context.Background())
	b.start(context.Background())
	if n := f.pump(); n != 0 {
		t.Errorf("a restart posted %d proofs\n%s", n, f.log())
	}
	if f.hs.signs != 2 {
		t.Errorf("%d signatures, want 2", f.hs.signs)
	}
}

// TestSiblingsThree has a new bot post once and every sibling answer,
// and the answer to one bot's request is ignored by the other.
func TestSiblingsThree(t *testing.T) {
	f := newFleet(t, alice, bob, carol)
	for _, u := range []string{alice, bob, carol} {
		f.bot(u, fleetSecret, "")
	}
	for _, u := range []id.UserID{alice, bob, carol} {
		f.bots[u].start(context.Background())
		f.pump()
	}
	for _, s := range []id.UserID{alice, bob, carol} {
		for _, o := range []id.UserID{alice, bob, carol} {
			if s != o && f.hs.signedKey(s, o) == "" {
				t.Errorf("%s did not sign %s\n%s", s, o, f.log())
			}
		}
	}
	if l := f.log(); strings.Contains(l, "refused") {
		t.Errorf("a refusal among siblings:\n%s", l)
	}
}

// proof returns a proof from u of key, made with secret now, its MAC
// set after mutate changes its fields.
func (f *fleet) proof(u id.UserID, key id.Ed25519, secret string, mutate func(*proof)) *proof {
	p := &proof{Label: trustLabel, UserID: string(u), MasterKey: string(key), Nonce: nonce(), TS: f.now.UnixMilli(), RoomID: string(coord)}
	if mutate != nil {
		mutate(p)
	}
	p.MAC = base64.RawStdEncoding.EncodeToString(p.mac(trustKey(secret)))
	return p
}

func TestSiblingsRefuse(t *testing.T) {
	f := newFleet(t, alice, bob, carol)
	b := f.bot(bob, fleetSecret, "")
	aliceKey := f.hs.published[alice]
	for _, c := range []struct {
		name   string
		sender id.UserID
		p      *proof
		want   error
	}{
		{"a stale timestamp", alice, f.proof(alice, aliceKey, fleetSecret, func(p *proof) { p.TS = f.now.Add(-proofWindow - time.Second).UnixMilli() }), errStale},
		{"a timestamp ahead", alice, f.proof(alice, aliceKey, fleetSecret, func(p *proof) { p.TS = f.now.Add(proofSkew + time.Second).UnixMilli() }), errStale},
		{"another sender than named", carol, f.proof(alice, aliceKey, fleetSecret, nil), errWrongUser},
		{"another room", alice, f.proof(alice, aliceKey, fleetSecret, func(p *proof) { p.RoomID = "!elsewhere:example.org" }), errWrongRoom},
		{"another label", alice, f.proof(alice, aliceKey, fleetSecret, func(p *proof) { p.Label = "airut-trust-v2" }), errWrongLabel},
		{"another secret", alice, f.proof(alice, aliceKey, oldSecret, nil), errBadMAC},
		{"an unpublished key", alice, f.proof(alice, "Z"+aliceKey[1:], fleetSecret, nil), errNotPublished},
		{"an answer to nothing", alice, f.proof(alice, aliceKey, fleetSecret, func(p *proof) { p.Answers = "never-asked" }), errNotAsked},
		{"no nonce", alice, f.proof(alice, aliceKey, fleetSecret, func(p *proof) { p.Nonce = "" }), errBadNonce},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := b.receive(context.Background(), c.sender, c.p); !errors.Is(err, c.want) {
				t.Errorf("receive = %v, want %v", err, c.want)
			}
			if f.hs.signs != 0 || len(f.posts()) != 0 {
				t.Errorf("signed %d, posted %d", f.hs.signs, len(f.posts()))
			}
		})
	}

	// A MAC changed after it was made does not verify.
	p := f.proof(alice, aliceKey, fleetSecret, nil)
	p.MAC = base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if err := b.receive(context.Background(), alice, p); !errors.Is(err, errBadMAC) {
		t.Errorf("a forged MAC: %v", err)
	}

	// A seen nonce is refused, and the first use is not.
	p = f.proof(alice, aliceKey, fleetSecret, nil)
	if err := b.receive(context.Background(), alice, p); err != nil {
		t.Fatalf("a good proof: %v", err)
	}
	if err := b.receive(context.Background(), alice, p); !errors.Is(err, errSeenNonce) {
		t.Errorf("a replayed proof: %v", err)
	}
	if f.hs.signs != 1 || len(f.posts()) != 1 {
		t.Errorf("signed %d, posted %d, want one each", f.hs.signs, len(f.posts()))
	}
}

// TestSiblingsIgnoreOutsiders ignores a proof from a sender not on the
// list, however good, and logs the sender id only.
func TestSiblingsIgnoreOutsiders(t *testing.T) {
	f := newFleet(t, alice, bob)
	b := f.bot(bob, fleetSecret, "")
	const mallory = "@mallory:example.org"
	f.hs.rekey(mallory, "MALLORYKEY")
	p := f.proof(mallory, "MALLORYKEY", fleetSecret, nil)
	raw, _ := json.Marshal(&proofMessage{MsgType: event.MsgNotice, Body: "chat text from mallory", Proof: p})
	b.handle(context.Background(), mallory, raw)
	if f.hs.signs != 0 || len(f.posts()) != 0 {
		t.Errorf("an outsider's proof signed %d and posted %d", f.hs.signs, len(f.posts()))
	}
	l := f.log()
	if !strings.Contains(l, "not on the sibling list") || !strings.Contains(l, mallory) {
		t.Errorf("the log does not name the outsider:\n%s", l)
	}
	for _, leak := range []string{"MALLORYKEY", "chat text", p.Nonce, p.MAC} {
		if strings.Contains(l, leak) {
			t.Errorf("the log holds %q:\n%s", leak, l)
		}
	}
}

// TestSiblingsChangedKey logs a signed sibling's new key once, however
// often rihma reports it, and signs it only when a proof shows it.
func TestSiblingsChangedKey(t *testing.T) {
	f := newFleet(t, alice, bob)
	a, b := f.bot(alice, fleetSecret, ""), f.bot(bob, fleetSecret, "")
	a.start(context.Background())
	f.pump()
	oldKey := f.hs.published[alice]
	if f.hs.signedKey(bob, alice) != oldKey {
		t.Fatal("bob did not sign alice")
	}
	// A proof made before alice's key changed, delivered after.
	stale := f.proof(alice, oldKey, fleetSecret, nil)
	f.hs.rekey(alice, "NEWALICEKEY")
	ch := rihma.IdentityChange{UserID: alice, Signed: oldKey, Published: "NEWALICEKEY"}
	b.identityChanged(ch)
	b.identityChanged(ch)
	if n := strings.Count(f.log(), "master key changed"); n != 1 {
		t.Errorf("the change was logged %d times:\n%s", n, f.log())
	}
	if err := b.receive(context.Background(), alice, stale); !errors.Is(err, errNotPublished) {
		t.Errorf("a proof of the old key: %v", err)
	}
	if f.hs.signedKey(bob, alice) != oldKey {
		t.Error("the new key was signed without a proof")
	}
	// alice restarts: her own key differs from the one she posted.
	a.start(context.Background())
	f.pump()
	if f.hs.signedKey(bob, alice) != "NEWALICEKEY" {
		t.Errorf("the proven new key was not signed\n%s", f.log())
	}
}

// TestSiblingsRoomSettings refuses to post in a room that is not
// encrypted or not joined, and warns of one anyone can join.
func TestSiblingsRoomSettings(t *testing.T) {
	for _, c := range []struct {
		name string
		st   roomState
		post bool
		log  string
	}{
		{"unencrypted", roomState{Joined: true, JoinRule: event.JoinRuleInvite}, false, "not encrypted"},
		{"not joined", roomState{Encrypted: true}, false, "not a member"},
		{"public", roomState{Joined: true, Encrypted: true, JoinRule: event.JoinRulePublic}, true, "anyone can join"},
		{"restricted", roomState{Joined: true, Encrypted: true, JoinRule: event.JoinRuleRestricted}, true, "anyone can join"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFleet(t, alice, bob)
			a := f.bot(alice, fleetSecret, "")
			a.rm.(*fakeRoom).st = c.st
			a.start(context.Background())
			if posted := len(f.posts()) == 1; posted != c.post {
				t.Errorf("posted %v, want %v", posted, c.post)
			}
			if !strings.Contains(f.log(), c.log) {
				t.Errorf("the log does not say %q:\n%s", c.log, f.log())
			}
		})
	}

	// An answer is refused too.
	f := newFleet(t, alice, bob)
	b := f.bot(bob, fleetSecret, "")
	b.rm.(*fakeRoom).st.Encrypted = false
	if err := b.receive(context.Background(), alice, f.proof(alice, f.hs.published[alice], fleetSecret, nil)); err != nil {
		t.Fatal(err)
	}
	if len(f.posts()) != 0 || !strings.Contains(f.log(), "not encrypted") {
		t.Errorf("answered in an unencrypted room:\n%s", f.log())
	}
}

// TestSiblingsJoinLater runs the start again when a bot that started
// outside the coordination room joins it, and only then.
func TestSiblingsJoinLater(t *testing.T) {
	f := newFleet(t, alice, bob)
	a, _ := f.bot(alice, fleetSecret, ""), f.bot(bob, fleetSecret, "")
	room := a.rm.(*fakeRoom)
	room.st.Joined = false
	a.start(context.Background())
	if n := f.pump(); n != 0 {
		t.Fatalf("posted %d outside the room", n)
	}
	room.st.Joined = true
	a.joined(context.Background())
	if n := f.pump(); n != 2 || f.hs.signedKey(alice, bob) == "" || f.hs.signedKey(bob, alice) == "" {
		t.Fatalf("after joining, delivered %d\n%s", n, f.log())
	}
	// Joined already at its start, or once the retry posted: a later
	// membership event, as from a profile change, posts nothing.
	a.joined(context.Background())
	if n := f.pump(); n != 0 {
		t.Errorf("a second join event posted %d", n)
	}
}

// TestSiblingsRotation accepts a proof under the previous secret during
// a rollout, and never makes one with it.
func TestSiblingsRotation(t *testing.T) {
	f := newFleet(t, alice, bob)
	f.bot(alice, oldSecret, "")
	b := f.bot(bob, fleetSecret, oldSecret)
	if err := b.receive(context.Background(), alice, f.proof(alice, f.hs.published[alice], oldSecret, nil)); err != nil {
		t.Fatalf("a proof under the previous secret: %v", err)
	}
	if f.hs.signedKey(bob, alice) == "" {
		t.Error("bob did not sign alice")
	}
	ans := proofOf(t, f.posts()[0])
	if !ans.verifies([][]byte{trustKey(fleetSecret)}) || ans.verifies([][]byte{trustKey(oldSecret)}) {
		t.Error("the answer was not made with the current secret alone")
	}
	// alice, not yet on the new secret, cannot verify it.
	f.pump()
	if f.hs.signedKey(alice, bob) != "" {
		t.Error("alice signed bob under a secret she does not have")
	}

	// Once alice has the new secret, the pair completes.
	f2 := newFleet(t, alice, bob)
	a2 := f2.bot(alice, fleetSecret, oldSecret)
	f2.bot(bob, fleetSecret, oldSecret)
	a2.start(context.Background())
	f2.pump()
	if f2.hs.signedKey(alice, bob) == "" || f2.hs.signedKey(bob, alice) == "" {
		t.Errorf("after the rollout: %v", f2.hs.signed)
	}
}

// TestSiblingsStart posts at start only when a sibling is unsigned or
// the bot's own key is not the one it last posted.
func TestSiblingsStart(t *testing.T) {
	f := newFleet(t, alice, bob)
	a, _ := f.bot(alice, fleetSecret, ""), f.bot(bob, fleetSecret, "")
	a.start(context.Background())
	f.pump()
	if got := a.lastPosted(); got != f.hs.published[alice] {
		t.Errorf("recorded %q", got)
	}
	// Signed and posted: nothing.
	a.start(context.Background())
	if n := f.pump(); n != 0 {
		t.Errorf("posted %d", n)
	}
	// A new key of its own: post, though every sibling is signed.
	f.hs.rekey(alice, "ALICEROTATED")
	a.start(context.Background())
	if n := f.pump(); n != 2 {
		t.Errorf("after a new own key, delivered %d", n)
	}
	// Without cross-signing keys: nothing, and a warning.
	f.hs.mu.Lock()
	f.hs.held[alice] = ""
	f.hs.mu.Unlock()
	a.start(context.Background())
	if n := f.pump(); n != 0 || !strings.Contains(f.log(), "no_cross_signing_keys") {
		t.Errorf("without keys, delivered %d\n%s", n, f.log())
	}
	// Its own id on the list is skipped.
	if a.users[alice] {
		t.Error("the bot lists itself as a sibling")
	}
}

// TestProofMACCoversEveryField changes each field the ticket names and
// requires the MAC to change with it.
func TestProofMACCoversEveryField(t *testing.T) {
	key := trustKey(fleetSecret)
	base := proof{Label: trustLabel, UserID: alice, MasterKey: "KEY", Nonce: "n", TS: 1, RoomID: string(coord), Answers: "a"}
	want := base.mac(key)
	for name, mutate := range map[string]func(*proof){
		"label":      func(p *proof) { p.Label = "x" },
		"user id":    func(p *proof) { p.UserID = bob },
		"master key": func(p *proof) { p.MasterKey = "KEY2" },
		"nonce":      func(p *proof) { p.Nonce = "m" },
		"timestamp":  func(p *proof) { p.TS = 2 },
		"room id":    func(p *proof) { p.RoomID = "!other:example.org" },
		"answers":    func(p *proof) { p.Answers = "" },
		// Moving a byte between neighbours changes the MAC too.
		"a boundary": func(p *proof) { p.Nonce, p.Answers = "n"+p.RoomID[:1], "a" },
	} {
		p := base
		mutate(&p)
		if bytes.Equal(p.mac(key), want) {
			t.Errorf("changing the %s left the MAC as it was", name)
		}
	}
	if bytes.Equal(base.mac(trustKey(oldSecret)), want) {
		t.Error("another secret makes the same MAC")
	}
}

// TestSiblingsLogNoSecret runs a whole exchange at debug level and finds
// neither secret, nor the derived key, nor any MAC in the log.
func TestSiblingsLogNoSecret(t *testing.T) {
	f := newFleet(t, alice, bob, carol)
	for _, u := range []string{alice, bob, carol} {
		f.bot(u, fleetSecret, oldSecret)
	}
	for _, u := range []id.UserID{alice, bob, carol} {
		f.bots[u].start(context.Background())
		f.pump()
	}
	// And some refusals, which log more.
	b := f.bots[bob]
	b.receive(context.Background(), alice, f.proof(alice, "UNPUBLISHED", fleetSecret, nil))
	b.handle(context.Background(), "@mallory:example.org", json.RawMessage(`{"msgtype":"m.notice","body":"hi"}`))
	l := f.log()
	leaks := []string{fleetSecret, oldSecret,
		hex.EncodeToString(trustKey(fleetSecret)), base64.RawStdEncoding.EncodeToString(trustKey(fleetSecret)),
		hex.EncodeToString(trustKey(oldSecret)), base64.RawStdEncoding.EncodeToString(trustKey(oldSecret))}
	ps := f.posts()
	if len(ps) < 3 {
		t.Fatalf("only %d posts", len(ps))
	}
	for _, p := range ps {
		leaks = append(leaks, proofOf(t, p).MAC)
	}
	for _, leak := range leaks {
		if strings.Contains(l, leak) {
			t.Errorf("the log holds a secret or a MAC: %q", leak)
		}
	}
}
