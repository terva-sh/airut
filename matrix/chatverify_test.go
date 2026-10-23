package matrix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

const (
	cvBot     = id.UserID("@bot:example.org")
	cvOwner   = id.UserID("@owner:example.org")
	cvMallory = id.UserID("@mallory:example.org")
	cvDM      = id.RoomID("!dm:example.org")
	cvShared  = id.RoomID("!shared:example.org")
	cvDevice  = id.DeviceID("OWNERDEVICE")
)

var cvEmoji = []rihma.SASEmoji{
	{Emoji: "🐶", Description: "Dog"}, {Emoji: "🐱", Description: "Cat"}, {Emoji: "🦁", Description: "Lion"},
	{Emoji: "🐎", Description: "Horse"}, {Emoji: "🦄", Description: "Unicorn"}, {Emoji: "🐷", Description: "Pig"},
	{Emoji: "🐘", Description: "Elephant"},
}

// fakeSAS stands in for rihma's SASController. Its calls are recorded;
// it changes state only when a test says so.
type fakeSAS struct {
	changed chan struct{}

	mu        sync.Mutex
	snap      rihma.SASSnapshot
	acceptErr error
	accepts   []id.VerificationTransactionID
	confirms  []bool
	cancels   []id.VerificationTransactionID
	calls     chan string
}

func newFakeSAS() *fakeSAS {
	return &fakeSAS{changed: make(chan struct{}, 1), calls: make(chan string, 16)}
}

func (f *fakeSAS) Changed() <-chan struct{} { return f.changed }

func (f *fakeSAS) Snapshot() rihma.SASSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeSAS) Accept(_ context.Context, txn id.VerificationTransactionID) error {
	f.mu.Lock()
	f.accepts = append(f.accepts, txn)
	err := f.acceptErr
	f.mu.Unlock()
	f.calls <- "accept"
	return err
}

func (f *fakeSAS) Confirm(_ context.Context, txn id.VerificationTransactionID, match bool) error {
	f.mu.Lock()
	f.confirms = append(f.confirms, match)
	f.mu.Unlock()
	f.calls <- fmt.Sprintf("confirm %v", match)
	return nil
}

func (f *fakeSAS) Cancel(_ context.Context, txn id.VerificationTransactionID) error {
	f.mu.Lock()
	f.cancels = append(f.cancels, txn)
	f.mu.Unlock()
	f.calls <- "cancel"
	return nil
}

// set publishes snap, as the controller does on a change.
func (f *fakeSAS) set(snap rihma.SASSnapshot) {
	f.mu.Lock()
	f.snap = snap
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// cvPost is a message the verification sent.
type cvPost struct {
	room id.RoomID
	text string
}

type cvHarness struct {
	t     *testing.T
	v     *chatVerify
	sas   *fakeSAS
	posts chan cvPost
	logs  *lockedLog
	wg    sync.WaitGroup
	// public makes cvDM stop being private, as when someone joins.
	public atomic.Bool
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// newCV runs a verification for cvBot whose only owner is cvOwner, whose
// only direct chat is cvDM, with a fake controller, and stops it at the
// end of the test.
func newCV(t *testing.T, timeout time.Duration) *cvHarness {
	t.Helper()
	h := &cvHarness{t: t, sas: newFakeSAS(), posts: make(chan cvPost, 16), logs: &lockedLog{}}
	h.v = &chatVerify{
		ctl:  h.sas,
		self: cvBot,
		owners: func(context.Context) ([]string, error) {
			return []string{string(cvOwner)}, nil
		},
		direct: func(_ context.Context, room id.RoomID, u id.UserID) bool {
			return room == cvDM && u == cvOwner && !h.public.Load()
		},
		dmOf: func(_ context.Context, u id.UserID) id.RoomID {
			if u == cvOwner {
				return cvDM
			}
			return ""
		},
		post: func(_ context.Context, room id.RoomID, text string) error {
			h.posts <- cvPost{room, text}
			return nil
		},
		spawn: func(f func()) {
			h.wg.Add(1)
			go func() { defer h.wg.Done(); f() }()
		},
		log:     slog.New(slog.NewTextHandler(h.logs, nil)),
		timeout: timeout,
		quit:    make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.v.spawn(func() { h.v.watch(ctx) })
	t.Cleanup(func() {
		cancel()
		h.v.shutdown()
		h.wg.Wait()
		for _, leak := range []string{"Dog", "🐶", "yes", "Unicorn"} {
			if strings.Contains(h.logs.String(), leak) {
				t.Errorf("the log holds %q:\n%s", leak, h.logs.String())
			}
		}
	})
	return h
}

func (h *cvHarness) request(txn string, from id.UserID, room id.RoomID) {
	h.sas.set(rihma.SASSnapshot{State: rihma.SASRequested, TransactionID: id.VerificationTransactionID(txn), OtherUser: from, OtherDevice: cvDevice, Room: room})
}

func (h *cvHarness) state(txn string, st rihma.SASState, mod func(*rihma.SASSnapshot)) {
	s := h.sas.Snapshot()
	s.State, s.TransactionID, s.Emoji, s.Reason, s.MasterKey = st, id.VerificationTransactionID(txn), nil, "", ""
	if mod != nil {
		mod(&s)
	}
	h.sas.set(s)
}

func (h *cvHarness) call(want string) {
	h.t.Helper()
	select {
	case got := <-h.sas.calls:
		if got != want {
			h.t.Fatalf("the controller got %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("the controller never got %q", want)
	}
}

func (h *cvHarness) noCall() {
	h.t.Helper()
	select {
	case got := <-h.sas.calls:
		h.t.Fatalf("the controller got %q, want nothing", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func (h *cvHarness) post() cvPost {
	h.t.Helper()
	select {
	case p := <-h.posts:
		return p
	case <-time.After(5 * time.Second):
		h.t.Fatal("nothing was posted")
		return cvPost{}
	}
}

func (h *cvHarness) noPost() {
	h.t.Helper()
	select {
	case p := <-h.posts:
		h.t.Fatalf("posted %q in %s, want nothing", p.text, p.room)
	case <-time.After(100 * time.Millisecond):
	}
}

// waitAsking waits until the attempt takes an answer.
func (h *cvHarness) waitAsking() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.v.mu.Lock()
		asking := h.v.cur != nil && h.v.cur.asking
		h.v.mu.Unlock()
		if asking {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("the verification never asked for an answer")
}

// toEmoji runs a request from the owner in the DM up to the emoji.
func (h *cvHarness) toEmoji(txn string) cvPost {
	h.t.Helper()
	h.request(txn, cvOwner, cvDM)
	h.call("accept")
	h.state(txn, rihma.SASAccepted, nil)
	h.state(txn, rihma.SASShowing, func(s *rihma.SASSnapshot) { s.Emoji = cvEmoji })
	p := h.post()
	h.waitAsking()
	return p
}

func reply(from id.UserID, room id.RoomID, body string, encrypted bool) *event.Event {
	evt := &event.Event{
		Sender: from, RoomID: room, ID: "$reply", Type: event.EventMessage, Timestamp: time.Now().UnixMilli(),
		Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: body}},
	}
	evt.Mautrix.WasEncrypted = encrypted
	return evt
}

func TestChatVerifyOffByDefault(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()
	called := false
	enable := func(rihma.SASOptions) (sasControl, error) { called = true; return newFakeSAS(), nil }
	owners := func(context.Context) ([]string, error) { called = true; return []string{string(cvOwner)}, nil }
	ctl, err := enableChatVerify(ctx, false, owners, cvBot, enable, log)
	if ctl != nil || err != nil || called {
		t.Fatalf("off: ctl %v, err %v, called %v; want no controller and no call", ctl, err, called)
	}
	// The zero configuration leaves it off.
	if (config.Matrix{}).VerifyInChat {
		t.Fatal("verify_in_chat defaults to on")
	}
}

func TestChatVerifyListsTheOwners(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	var got rihma.SASOptions
	enable := func(o rihma.SASOptions) (sasControl, error) { got = o; return newFakeSAS(), nil }
	owners := func(context.Context) ([]string, error) {
		return []string{string(cvOwner), string(cvBot), "not-an-id", string(cvOwner), "@second:example.org"}, nil
	}
	ctl, err := enableChatVerify(ctx, true, owners, cvBot, enable, log)
	if ctl == nil || err != nil {
		t.Fatalf("on: %v, %v", ctl, err)
	}
	if !got.InRoom || len(got.Peers) != 2 || got.Peers[0] != cvOwner || got.Peers[1] != "@second:example.org" {
		t.Errorf("options = %+v; want the two owners, in-room, without the bot or the malformed id", got)
	}
	if !strings.Contains(logs.String(), "count=2") {
		t.Errorf("the skipped owners are not counted in the log:\n%s", logs.String())
	}

	// With no owner it stays off, and says so.
	logs.Reset()
	enabled := false
	none := func(context.Context) ([]string, error) { return []string{string(cvBot)}, nil }
	ctl, err = enableChatVerify(ctx, true, none, cvBot, func(rihma.SASOptions) (sasControl, error) { enabled = true; return nil, nil }, log)
	if ctl != nil || err != nil || enabled || !strings.Contains(logs.String(), "no Matrix owner") {
		t.Errorf("no owners: %v, %v, enabled %v\n%s", ctl, err, enabled, logs.String())
	}

	// An owners read that fails leaves it off without stopping the bot.
	failing := func(context.Context) ([]string, error) { return nil, errors.New("claims unreadable") }
	if ctl, err := enableChatVerify(ctx, true, failing, cvBot, enable, log); ctl != nil || err != nil {
		t.Errorf("failing owners: %v, %v", ctl, err)
	}

	// Without an owners function it cannot run, so it refuses.
	if _, err := enableChatVerify(ctx, true, nil, cvBot, enable, log); !errors.Is(err, errNoOwnersFunc) {
		t.Errorf("nil owners: %v", err)
	}
	// rihma's refusal stops the start.
	refused := func(rihma.SASOptions) (sasControl, error) { return nil, rihma.ErrSASUnavailable }
	if _, err := enableChatVerify(ctx, true, owners, cvBot, refused, log); !errors.Is(err, rihma.ErrSASUnavailable) {
		t.Errorf("refused: %v", err)
	}
}

func TestRunRefusesVerifyInChatWithoutOwners(t *testing.T) {
	a := New(&config.Matrix{VerifyInChat: true}, Options{AcceptInvite: func(Invite) bool { return true }})
	if err := a.Run(context.Background(), nil); !errors.Is(err, errNoOwnersFunc) {
		t.Fatalf("Run = %v, want errNoOwnersFunc", err)
	}
}

func TestChatVerifyRefusesANonOwner(t *testing.T) {
	h := newCV(t, time.Minute)
	h.request("$t1", cvMallory, cvDM)
	h.call("cancel")
	h.noPost()
	h.noCall()
	if !strings.Contains(h.logs.String(), "outcome=not_owner") {
		t.Errorf("log:\n%s", h.logs.String())
	}
}

func TestChatVerifyRefusesTheBotsOwnAccount(t *testing.T) {
	h := newCV(t, time.Minute)
	h.request("$t1", cvBot, "")
	h.call("cancel")
	h.noPost()
}

func TestChatVerifyRefusesOutsideTheDirectChat(t *testing.T) {
	for _, room := range []id.RoomID{cvShared, ""} {
		h := newCV(t, time.Minute)
		h.request("$t-"+string(room), cvOwner, room)
		h.call("cancel")
		p := h.post()
		if p.room != cvDM || p.text != textElsewhere {
			t.Errorf("room %q: posted %q in %s; want the fixed note in the direct chat", room, p.text, p.room)
		}
		h.noCall()
		h.noPost()
	}
}

func TestChatVerifyYesConfirms(t *testing.T) {
	h := newCV(t, time.Minute)
	p := h.toEmoji("$t1")
	if p.room != cvDM {
		t.Fatalf("the emoji went to %s", p.room)
	}
	for _, w := range []string{"in progress", string(cvDevice), "1. 🐶 Dog", "7. 🐘 Elephant", "Element", "**yes**", "**no**", "trusts the homeserver", "-verify"} {
		if !strings.Contains(p.text, w) {
			t.Errorf("the emoji message lacks %q:\n%s", w, p.text)
		}
	}
	// Another user's yes, an unencrypted yes, and the owner's yes in
	// another room do not count, nor does chat that is not an answer.
	for _, evt := range []*event.Event{
		reply(cvMallory, cvDM, "yes", true),
		reply(cvOwner, cvDM, "yes", false),
		reply(cvOwner, cvShared, "yes", true),
		reply(cvOwner, cvDM, "yes they look right to me", true),
	} {
		if h.v.answer(evt) {
			t.Errorf("took %q from %s in %s (encrypted %v) as the answer", evt.Content.AsMessage().Body, evt.Sender, evt.RoomID, evt.Mautrix.WasEncrypted)
		}
	}
	h.noCall()
	if !h.v.answer(reply(cvOwner, cvDM, " Yes ", true)) {
		t.Fatal("the owner's yes was not taken")
	}
	h.call("confirm true")
	if p := h.post(); p.text != textConfirmed {
		t.Errorf("after yes: %q", p.text)
	}
	// Once answered, another yes is chat again.
	if h.v.answer(reply(cvOwner, cvDM, "yes", true)) {
		t.Error("a second yes was taken")
	}
	h.state("$t1", rihma.SASConfirmed, nil)
	h.state("$t1", rihma.SASDone, func(s *rihma.SASSnapshot) { s.MasterKey = "AAAABBBBCCCCDDDD" })
	p = h.post()
	if !strings.Contains(p.text, "Verified") || !strings.Contains(p.text, "AAAA BBBB CCCC DDDD") {
		t.Errorf("outcome: %q", p.text)
	}
	h.noCall()
	h.waitDone()
	if !strings.Contains(h.logs.String(), "outcome=done") || !strings.Contains(h.logs.String(), "txn=$t1") || !strings.Contains(h.logs.String(), "user="+string(cvOwner)) {
		t.Errorf("log:\n%s", h.logs.String())
	}
}

func TestChatVerifyNoRejects(t *testing.T) {
	h := newCV(t, time.Minute)
	h.toEmoji("$t1")
	if !h.v.answer(reply(cvOwner, cvDM, "N", true)) {
		t.Fatal("the owner's no was not taken")
	}
	h.call("confirm false")
	h.state("$t1", rihma.SASCancelled, func(s *rihma.SASSnapshot) { s.Reason = "mismatch" })
	if p := h.post(); p.text != reasonText("mismatch") {
		t.Errorf("outcome: %q", p.text)
	}
	h.noCall()
}

func TestChatVerifyNeverConfirmsByItself(t *testing.T) {
	h := newCV(t, 300*time.Millisecond)
	h.toEmoji("$t1")
	// Nobody answers; the attempt times out and cancels.
	h.call("cancel")
	if p := h.post(); p.text != reasonText("timeout") {
		t.Errorf("outcome: %q", p.text)
	}
	h.sas.mu.Lock()
	confirms := len(h.sas.confirms)
	h.sas.mu.Unlock()
	if confirms != 0 {
		t.Errorf("confirmed %d times without an answer", confirms)
	}
	h.waitDone()
	if !strings.Contains(h.logs.String(), "outcome=timeout") {
		t.Errorf("log:\n%s", h.logs.String())
	}
}

func TestChatVerifyAnswerBeforeTheEmojiIsChat(t *testing.T) {
	h := newCV(t, time.Minute)
	h.request("$t1", cvOwner, cvDM)
	h.call("accept")
	if h.v.answer(reply(cvOwner, cvDM, "yes", true)) {
		t.Error("a yes before the emoji were shown was taken")
	}
}

func TestChatVerifyAcceptFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{rihma.ErrNoCrossSigningKeys, "cross-signing setup"},
		{rihma.ErrNoUserIdentity, "no cross-signing identity"},
		{rihma.ErrSASStale, "could not start"},
	} {
		h := newCV(t, time.Minute)
		h.sas.acceptErr = tc.err
		h.request("$t1", cvOwner, cvDM)
		h.call("accept")
		h.call("cancel")
		if p := h.post(); p.room != cvDM || !strings.Contains(p.text, tc.want) {
			t.Errorf("%v: posted %q", tc.err, p.text)
		}
	}
}

func TestChatVerifyANewRequestReplacesTheOld(t *testing.T) {
	h := newCV(t, time.Minute)
	h.toEmoji("$t1")
	// rihma admits another request only once the first has ended.
	h.request("$t2", cvOwner, cvDM)
	posts := []string{h.post().text}
	h.call("accept")
	h.state("$t2", rihma.SASShowing, func(s *rihma.SASSnapshot) { s.Emoji = cvEmoji })
	posts = append(posts, h.post().text)
	if !strings.Contains(strings.Join(posts, "\n"), textReplaced) {
		t.Errorf("the replaced attempt said nothing: %q", posts)
	}
	h.waitAsking()
	if !h.v.answer(reply(cvOwner, cvDM, "y", true)) {
		t.Fatal("the new attempt's yes was not taken")
	}
	h.call("confirm true")
}

func TestChatVerifyOutcomeTexts(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range []string{"cancelled", "mismatch", "timeout", "rejected", "operation_failed", "overflow", "unsigned", "identity_changed"} {
		txt := reasonText(r)
		if seen[txt] || !strings.Contains(txt, "sign") {
			t.Errorf("%s: %q is repeated or does not say what was signed", r, txt)
		}
		seen[txt] = true
		if reasonKind(r) != r {
			t.Errorf("reasonKind(%q) = %q", r, reasonKind(r))
		}
	}
	// An unknown reason never reaches the chat or the log as given.
	odd := "the server says: click https://evil.example"
	if strings.Contains(reasonText(odd), "evil") || reasonKind(odd) != "other" {
		t.Error("an unknown reason was shown")
	}
	for _, tc := range []struct {
		state  rihma.SASState
		reason string
	}{{rihma.SASCancelled, "rejected"}, {rihma.SASFailed, "identity_changed"}, {rihma.SASFailed, "server text"}} {
		h := newCV(t, time.Minute)
		h.toEmoji("$t1")
		h.state("$t1", tc.state, func(s *rihma.SASSnapshot) { s.Reason = tc.reason })
		if p := h.post(); p.text != reasonText(tc.reason) || p.room != cvDM {
			t.Errorf("%s %s: posted %q", tc.state, tc.reason, p.text)
		}
		h.waitDone()
		if !strings.Contains(h.logs.String(), "reason="+reasonKind(tc.reason)) {
			t.Errorf("log:\n%s", h.logs.String())
		}
	}
}

func TestParseYesNo(t *testing.T) {
	for in, want := range map[string][2]bool{
		"yes": {true, true}, "Y": {true, true}, " YES\n": {true, true},
		"no": {false, true}, "n": {false, true}, "No ": {false, true},
		"yep": {false, false}, "yes.": {false, false}, "": {false, false}, "ok": {false, false},
	} {
		if yes, ok := parseYesNo(in); yes != want[0] || ok != want[1] {
			t.Errorf("parseYesNo(%q) = %v, %v", in, yes, ok)
		}
	}
}

// recordingHost records what reaches the bot.
type recordingHost struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingHost) Specs() []airut.Spec { return nil }
func (r *recordingHost) Prefix() string      { return "!" }
func (r *recordingHost) Dispatch(context.Context, airut.Invocation) {
	r.mu.Lock()
	r.seen = append(r.seen, "dispatch")
	r.mu.Unlock()
}
func (r *recordingHost) Deliver(_ context.Context, ev airut.Event, _ airut.Responder) {
	r.mu.Lock()
	r.seen = append(r.seen, fmt.Sprintf("deliver %T", ev))
	r.mu.Unlock()
}
func (r *recordingHost) Ready(context.Context, airut.Conn) {}
func (r *recordingHost) Logger() *slog.Logger              { return slog.New(slog.DiscardHandler) }

// The owner's answer is the verification's alone: onMessage hands it
// over and nothing in the bot sees it.
func TestChatVerifyAnswerNeverReachesTheBot(t *testing.T) {
	h := newCV(t, time.Minute)
	h.toEmoji("$t1")
	host := &recordingHost{}
	s := &session{
		ctx: context.Background(), h: host, log: slog.New(slog.DiscardHandler),
		c:      &rihma.Client{Client: &mautrix.Client{UserID: cvBot}},
		verify: h.v,
	}
	s.onMessage(context.Background(), reply(cvOwner, cvDM, "yes", true))
	h.call("confirm true")
	s.stop()
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.seen) != 0 {
		t.Errorf("the answer reached the bot: %v", host.seen)
	}
}

// waitDone waits until the attempt has ended.
func (h *cvHarness) waitDone() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.v.mu.Lock()
		cur := h.v.cur
		h.v.mu.Unlock()
		if cur == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatal("the verification never ended")
}

func (h *cvHarness) confirmCount() int {
	h.sas.mu.Lock()
	defer h.sas.mu.Unlock()
	return len(h.sas.confirms)
}

// A room that stops being private between Accept and the emoji gets no
// emoji, and the attempt is cancelled.
func TestChatVerifyNoEmojiIntoARoomNoLongerPrivate(t *testing.T) {
	h := newCV(t, time.Minute)
	h.request("$t1", cvOwner, cvDM)
	h.call("accept")
	h.public.Store(true)
	h.state("$t1", rihma.SASShowing, func(s *rihma.SASSnapshot) { s.Emoji = cvEmoji })
	h.call("cancel")
	h.waitDone()
	h.noPost()
	if !strings.Contains(h.logs.String(), "outcome=not_direct") {
		t.Errorf("log:\n%s", h.logs.String())
	}
}

// A room that stops being private after the emoji and before the answer
// confirms nothing: the answer is dropped and the attempt cancelled.
func TestChatVerifyNoConfirmFromARoomNoLongerPrivate(t *testing.T) {
	h := newCV(t, time.Minute)
	h.toEmoji("$t1")
	h.public.Store(true)
	if !h.v.answer(reply(cvOwner, cvDM, "yes", true)) {
		t.Fatal("the owner's yes was not taken")
	}
	h.call("cancel")
	h.waitDone()
	if n := h.confirmCount(); n != 0 {
		t.Errorf("confirmed %d times", n)
	}
	h.noPost()
}

// Of two replies that race, exactly one is taken, and the controller is
// confirmed exactly once. Run it under -race with a high -count.
func TestChatVerifyTakesOneOfTwoRacingAnswers(t *testing.T) {
	h := newCV(t, time.Minute)
	h.toEmoji("$t1")
	var taken atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, body := range []string{"yes", "no"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if h.v.answer(reply(cvOwner, cvDM, body, true)) {
				taken.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := taken.Load(); n != 1 {
		t.Fatalf("took %d answers, want 1", n)
	}
	got := <-h.sas.calls
	switch got {
	case "confirm true":
		h.state("$t1", rihma.SASDone, func(s *rihma.SASSnapshot) { s.MasterKey = "AAAA" })
	case "confirm false":
		h.state("$t1", rihma.SASCancelled, func(s *rihma.SASSnapshot) { s.Reason = "mismatch" })
	default:
		t.Fatalf("the controller got %q", got)
	}
	h.waitDone()
	if n := h.confirmCount(); n != 1 {
		t.Errorf("confirmed %d times, want 1", n)
	}
}
