package matrix

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut/config"
)

// Sibling cross-signing. Bots of one owner, each listing the others in
// [matrix.siblings], vouch for each other's Matrix identities without a
// person: each signs a sibling's master key with its user-signing key
// once the sibling has proven, with the fleet secret they share, that the
// key is its own.
//
// A proof is an m.notice in the coordination room. Its body says, for
// the owner reading the room, which bot vouches for which master key;
// its fields, under proofField, carry the protocol label, the sender's
// user id and master key, a nonce, a timestamp, the room's id, in an
// answer the nonce of the request it answers, and an HMAC-SHA256 over
// all of them, keyed with a key derived from the secret. A bot signs a
// sibling's key only when the sender is on the list and is the user the
// proof names, the proof is for this room and this label, the MAC
// verifies, the timestamp is within proofWindow, the nonce is new, an
// answer answers a request of this bot's, and the homeserver publishes
// the proven key now. The room grants nothing: its members, a message's
// sender, and its history prove nothing by themselves.
//
// A bot posts a request at start when it has not signed some sibling's
// current key, or when its own master key differs from the one it last
// posted. Each sibling that accepts a request answers it with its own
// proof, so the requester signs the answerer and the answerer the
// requester. An answer is never answered.

// trustLabel is the protocol's label. It derives the MAC key from the
// secret and is covered by every MAC, so neither is mistaken for another
// use of the secret.
const trustLabel = "airut-trust-v1"

// proofField is the event content key that carries a proof's fields.
const proofField = "sh.terva.airut.sibling_proof"

// proofWindow is how old a proof's own timestamp may be, and proofSkew
// how far ahead of this bot's clock. The timestamp is the one inside the
// MAC, not the homeserver's, so a proof read again from the room's
// history after a restart is refused.
const (
	proofWindow = 5 * time.Minute
	proofSkew   = time.Minute
)

// maxNonce bounds a nonce, which the bot keeps for the window.
const maxNonce = 64

// siblingsFile is the file in the state directory that records the
// master key the bot last posted a proof for.
const siblingsFile = "siblings.json"

// proof is a proof's fields as the event carries them.
type proof struct {
	Label     string `json:"label"`
	UserID    string `json:"user_id"`
	MasterKey string `json:"master_key"`
	Nonce     string `json:"nonce"`
	// TS is when the proof was made, in milliseconds since the epoch.
	TS     int64  `json:"ts"`
	RoomID string `json:"room_id"`
	// Answers is the nonce of the request this proof answers, and empty
	// in a request.
	Answers string `json:"answers,omitempty"`
	// MAC is the base64 HMAC-SHA256 of canonical. It is never logged.
	MAC string `json:"mac"`
}

// proofMessage is the event a proof is posted as.
type proofMessage struct {
	MsgType event.MessageType `json:"msgtype"`
	Body    string            `json:"body"`
	Proof   *proof            `json:"sh.terva.airut.sibling_proof,omitempty"`
}

// canonical is what the MAC covers: every field but the MAC, each
// prefixed with its length, so that no field can run into the next.
func (p *proof) canonical() []byte {
	var b []byte
	for _, f := range []string{p.Label, p.UserID, p.MasterKey, p.Nonce, strconv.FormatInt(p.TS, 10), p.RoomID, p.Answers} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

func (p *proof) mac(key []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(p.canonical())
	return m.Sum(nil)
}

// verifies reports whether the proof's MAC is right under one of keys,
// compared in constant time.
func (p *proof) verifies(keys [][]byte) bool {
	got, err := base64.RawStdEncoding.DecodeString(p.MAC)
	if err != nil {
		return false
	}
	ok := false
	for _, k := range keys {
		ok = hmac.Equal(got, p.mac(k)) || ok
	}
	return ok
}

// trustKey derives the MAC key from a fleet secret.
func trustKey(secret string) []byte {
	k, err := hkdf.Key(sha256.New, []byte(secret), nil, trustLabel, sha256.Size)
	if err != nil {
		// Only a FIPS-only build refuses SHA-256 with a key this long.
		panic("matrix: deriving the sibling key: " + err.Error())
	}
	return k
}

// identities is what the protocol needs of the account's cross-signing.
// *rihma.Client has it; a test fakes it.
type identities interface {
	OwnMasterKey(ctx context.Context) (id.Ed25519, error)
	UserIdentity(ctx context.Context, user id.UserID) (*rihma.UserIdentity, error)
	SignUserIdentity(ctx context.Context, user id.UserID, expected id.Ed25519) error
}

// roomState is what the bot knows of the coordination room.
type roomState struct {
	Joined, Encrypted bool
	JoinRule          event.JoinRule
}

// trustRoom is the coordination room, as the protocol reads and posts to
// it. The session has it; a test fakes it.
type trustRoom interface {
	state(ctx context.Context) (roomState, error)
	post(ctx context.Context, m *proofMessage) error
}

// siblings runs the protocol for one bot.
type siblings struct {
	self  id.UserID
	room  id.RoomID
	users map[id.UserID]bool
	// keys verify a proof; the first, from the current secret, also
	// makes one.
	keys [][]byte
	// stateFile records the master key last posted.
	stateFile string
	log       *slog.Logger
	now       func() time.Time

	// ids and rm are set once the client is open.
	ids identities
	rm  trustRoom

	// handling serializes the handling of proofs and the start.
	handling sync.Mutex
	// unjoined reports that the start found the bot outside the
	// coordination room, so joining it runs the start again. handling
	// guards it.
	unjoined bool

	mu sync.Mutex
	// seen holds each sibling's nonces until their proofs leave the
	// window, keyed by sender and nonce.
	seen map[string]time.Time
	// asked holds the nonces of this bot's requests until answers to
	// them leave the window.
	asked map[string]time.Time
	// warned holds the identity changes already logged, by user and
	// published key, so a repeated report is not logged twice.
	warned map[string]bool
}

func newSiblings(cfg *config.Siblings, self id.UserID, stateDir string, log *slog.Logger) *siblings {
	t := &siblings{
		self: self, room: id.RoomID(cfg.Room), users: map[id.UserID]bool{},
		keys:      [][]byte{trustKey(cfg.Secret.Value())},
		stateFile: filepath.Join(stateDir, siblingsFile),
		log:       log.With("component", "siblings"),
		now:       time.Now,
		seen:      map[string]time.Time{}, asked: map[string]time.Time{}, warned: map[string]bool{},
	}
	if !cfg.PreviousSecret.IsZero() {
		t.keys = append(t.keys, trustKey(cfg.PreviousSecret.Value()))
	}
	for _, u := range cfg.Users {
		if id.UserID(u) != self {
			t.users[id.UserID(u)] = true
		}
	}
	return t
}

// start posts a request when the bot has not signed some sibling's
// current key, or when its own master key differs from the one it last
// posted. A bot cannot learn whether a sibling signed it, so the rule
// rests on what it knows itself.
func (t *siblings) start(ctx context.Context) {
	t.handling.Lock()
	defer t.handling.Unlock()
	t.unjoined = false
	own, err := t.ids.OwnMasterKey(ctx)
	if err != nil {
		t.log.Warn("cannot vouch for itself to its siblings; run the cross-signing setup", "error_kind", identityErrKind(err))
		return
	}
	reason := ""
	if t.lastPosted() != own {
		reason = "own_key_not_posted"
	}
	for _, u := range slices.Sorted(maps.Keys(t.users)) {
		ident, err := t.ids.UserIdentity(ctx, u)
		switch {
		case err != nil:
			t.log.Info("could not read a sibling's identity", "user", string(u), "error_kind", identityErrKind(err))
		case !ident.SignedByUs:
			t.log.Info("a sibling's master key is not signed yet", "user", string(u), "master_key", string(ident.MasterKey))
		default:
			continue
		}
		if reason == "" {
			reason = "sibling_not_signed"
		}
	}
	if reason == "" {
		t.log.Info("every sibling is signed and the bot's own key was posted; posting nothing")
		return
	}
	if err := t.post(ctx, own, ""); err != nil {
		t.unjoined = errors.Is(err, errNotJoined)
		t.log.Warn("could not post a proof to its siblings", "reason", reason, "error", err.Error())
		return
	}
	t.log.Info("posted a proof to its siblings", "reason", reason, "master_key", string(own))
}

// joined is told that the bot joined the coordination room. When the
// start found it outside the room, it runs the start again, so a bot
// invited after it started still posts. The lock orders it after a start
// in flight, which has either posted or set unjoined by then.
func (t *siblings) joined(ctx context.Context) {
	t.handling.Lock()
	again := t.unjoined
	t.handling.Unlock()
	if again {
		t.start(ctx)
	}
}

// errNotJoined is post's refusal of a room the bot is not in.
var errNotJoined = errors.New("the bot is not a member of the coordination room")

// post posts this bot's proof of own, answering the request whose nonce
// is answers, or as a request when answers is empty. It refuses a room
// that is not encrypted, and warns of one anyone can join.
func (t *siblings) post(ctx context.Context, own id.Ed25519, answers string) error {
	st, err := t.rm.state(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("reading the coordination room: %s", errKind(err))
	case !st.Joined:
		return errNotJoined
	case !st.Encrypted:
		return errors.New("the coordination room is not encrypted, so the bot refuses to post there")
	}
	switch st.JoinRule {
	case event.JoinRulePublic, event.JoinRuleRestricted, event.JoinRuleKnockRestricted:
		t.log.Warn("anyone can join the coordination room without an invite; make it invite-only", "room", string(t.room), "join_rule", string(st.JoinRule))
	}
	now := t.now()
	p := &proof{Label: trustLabel, UserID: string(t.self), MasterKey: string(own), Nonce: nonce(),
		TS: now.UnixMilli(), RoomID: string(t.room), Answers: answers}
	p.MAC = base64.RawStdEncoding.EncodeToString(p.mac(t.keys[0]))
	what := "Sibling proof"
	if answers != "" {
		what = "Sibling answer"
	}
	m := &proofMessage{MsgType: event.MsgNotice, Proof: p,
		Body: fmt.Sprintf("%s: %s vouches for its master key %s.", what, t.self, spaced(string(own)))}
	if answers == "" {
		// Recorded before the post, which an answer can beat back.
		t.mu.Lock()
		t.asked[p.Nonce] = now.Add(proofWindow + proofSkew)
		t.mu.Unlock()
	}
	if err := t.rm.post(ctx, m); err != nil {
		return fmt.Errorf("posting: %s", errKind(err))
	}
	if err := t.recordPosted(own); err != nil {
		// The next start posts again, which is harmless.
		t.log.Warn("could not record the master key it posted in the state directory", "master_key", string(own))
	}
	return nil
}

// handle reads a message from the coordination room. A message from a
// sender not on the list is ignored, and logged by sender only.
func (t *siblings) handle(ctx context.Context, sender id.UserID, raw json.RawMessage) {
	if !t.users[sender] {
		t.log.Info("ignored a message in the coordination room from a sender not on the sibling list", "sender", string(sender))
		return
	}
	var m proofMessage
	if err := json.Unmarshal(raw, &m); err != nil || m.MsgType != event.MsgNotice || m.Proof == nil {
		t.log.Debug("a sibling's message in the coordination room is not a proof", "sender", string(sender))
		return
	}
	t.handling.Lock()
	defer t.handling.Unlock()
	switch err := t.receive(ctx, sender, m.Proof); {
	case errors.Is(err, errNotAsked):
		// Every sibling sees every answer, and this one answers another's
		// request.
		t.log.Debug("a sibling's answer is not to this bot", "sender", string(sender))
	case err != nil:
		t.log.Warn("refused a sibling's proof", "sender", string(sender), "reason", err.Error())
	}
}

// The refusals of a proof. Each names no secret and no MAC.
var (
	errWrongLabel   = errors.New("the proof is for another protocol")
	errWrongRoom    = errors.New("the proof is for another room")
	errWrongUser    = errors.New("the proof names another user than its sender")
	errBadMAC       = errors.New("the proof's MAC does not verify")
	errStale        = errors.New("the proof's timestamp is outside the window")
	errBadNonce     = errors.New("the proof's nonce is missing or too long")
	errSeenNonce    = errors.New("the proof's nonce was seen before")
	errNotAsked     = errors.New("the proof answers no request of this bot's")
	errNotPublished = errors.New("the homeserver does not publish the proven master key")
)

// receive checks a sibling's proof and, when every check holds, signs
// the sibling's key and answers a request. sender is on the list.
func (t *siblings) receive(ctx context.Context, sender id.UserID, p *proof) error {
	switch {
	case p.Label != trustLabel:
		return errWrongLabel
	case p.RoomID != string(t.room):
		return errWrongRoom
	case p.UserID != string(sender):
		return errWrongUser
	case !p.verifies(t.keys):
		return errBadMAC
	case p.Nonce == "" || len(p.Nonce) > maxNonce:
		return errBadNonce
	}
	now := t.now()
	ts := time.UnixMilli(p.TS)
	if ts.Before(now.Add(-proofWindow)) || ts.After(now.Add(proofSkew)) {
		return errStale
	}
	t.mu.Lock()
	for k, until := range t.seen {
		if now.After(until) {
			delete(t.seen, k)
		}
	}
	for k, until := range t.asked {
		if now.After(until) {
			delete(t.asked, k)
		}
	}
	seenKey := string(sender) + "\x00" + p.Nonce
	_, seen := t.seen[seenKey]
	_, asked := t.asked[p.Answers]
	if !seen && (p.Answers == "" || asked) {
		t.seen[seenKey] = ts.Add(proofWindow)
	}
	t.mu.Unlock()
	switch {
	case seen:
		return errSeenNonce
	case p.Answers != "" && !asked:
		return errNotAsked
	}

	proven := id.Ed25519(p.MasterKey)
	ident, err := t.ids.UserIdentity(ctx, sender)
	if err != nil {
		return fmt.Errorf("reading the sender's identity: %s", identityErrKind(err))
	}
	if ident.MasterKey != proven {
		t.log.Warn("a sibling's proof names a master key the homeserver does not publish", "sender", string(sender),
			"proven", string(proven), "published", string(ident.MasterKey))
		return errNotPublished
	}
	switch {
	case ident.SignedByUs:
		t.log.Info("a sibling's master key is already signed", "user", string(sender), "master_key", string(proven))
	default:
		if err := t.ids.SignUserIdentity(ctx, sender, proven); err != nil {
			// The sibling still gets an answer: its own signing of this
			// bot does not wait on this one.
			t.log.Warn("could not sign a sibling's master key", "user", string(sender), "master_key", string(proven), "error_kind", identityErrKind(err))
		} else {
			t.log.Info("signed a sibling's master key", "user", string(sender), "master_key", string(proven))
		}
	}
	if p.Answers != "" {
		return nil
	}
	own, err := t.ids.OwnMasterKey(ctx)
	if err != nil {
		t.log.Warn("cannot answer a sibling's proof", "user", string(sender), "error_kind", identityErrKind(err))
		return nil
	}
	if err := t.post(ctx, own, p.Nonce); err != nil {
		t.log.Warn("could not answer a sibling's proof", "user", string(sender), "error", err.Error())
	}
	return nil
}

// identityChanged is rihma's report that a user this account signed now
// publishes another master key. It is logged once per key, since rihma
// may report one again after a restart, and nothing is signed: a new
// key is signed only once a proof shows it.
func (t *siblings) identityChanged(ch rihma.IdentityChange) {
	k := string(ch.UserID) + "\x00" + string(ch.Published)
	t.mu.Lock()
	again := t.warned[k]
	t.warned[k] = true
	t.mu.Unlock()
	if again {
		return
	}
	t.log.Warn("a signed user's master key changed; it is not signed again until a sibling proof shows the new key",
		"user", string(ch.UserID), "signed", string(ch.Signed), "published", string(ch.Published), "sibling", t.users[ch.UserID])
}

// siblingsState is the siblings file.
type siblingsState struct {
	PostedMasterKey string `json:"posted_master_key"`
}

func (t *siblings) lastPosted() id.Ed25519 {
	b, err := os.ReadFile(t.stateFile)
	if err != nil {
		return ""
	}
	var st siblingsState
	if json.Unmarshal(b, &st) != nil {
		return ""
	}
	return id.Ed25519(st.PostedMasterKey)
}

func (t *siblings) recordPosted(own id.Ed25519) error {
	if t.lastPosted() == own {
		return nil
	}
	b, _ := json.Marshal(siblingsState{PostedMasterKey: string(own)})
	tmp := t.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.stateFile)
}

// coordRoom is the coordination room as the session reaches it.
type coordRoom struct {
	s    *session
	room id.RoomID
}

func (r coordRoom) state(ctx context.Context) (roomState, error) {
	st := r.s.c.StateStore
	var out roomState
	out.Joined = st.IsInRoom(ctx, r.room, r.s.c.UserID)
	enc, err := st.IsEncrypted(ctx, r.room)
	if err != nil {
		return out, err
	}
	out.Encrypted = enc
	jr, err := st.GetJoinRules(ctx, r.room)
	if err != nil {
		return out, err
	}
	if jr != nil {
		out.JoinRule = jr.JoinRule
	}
	return out, nil
}

func (r coordRoom) post(ctx context.Context, m *proofMessage) error {
	if err := r.s.c.Connect(ctx); err != nil {
		return err
	}
	_, err := r.s.c.SendMessageEvent(ctx, r.room, event.EventMessage, m)
	return err
}

// identityErrKind names an error from rihma's identity calls for a log.
func identityErrKind(err error) string {
	switch {
	case errors.Is(err, rihma.ErrNoCrossSigningKeys):
		return "no_cross_signing_keys"
	case errors.Is(err, rihma.ErrNoUserIdentity):
		return "no_user_identity"
	case errors.Is(err, rihma.ErrMasterKeyMismatch):
		return "master_key_mismatch"
	case errors.Is(err, rihma.ErrOwnUser):
		return "own_user"
	}
	return errKind(err)
}

func nonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// spaced groups a key in fours, as clients show one.
func spaced(key string) string {
	var b strings.Builder
	for i, r := range key {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
