package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut/config"
)

// Each error from the verification becomes a message that names its
// cause, still matches the error it came from, and says whether anything
// was sent or signed.
func TestVerifyErrorMessages(t *testing.T) {
	const peer, device = id.UserID("@owner:example.org"), id.DeviceID("BOTDEVICE")
	cases := []struct {
		err  error
		want []string
	}{
		{rihma.ErrSyncInProgress, []string{"the bot is running", "stop it first"}},
		{rihma.ErrOwnUser, []string{"bot's own account"}},
		{ErrNoDirectRoom, []string{"no private encrypted direct chat", "Nothing was sent", "start a direct chat with the bot"}},
		{rihma.ErrNoCrossSigningKeys, []string{"BOTDEVICE", "Nothing was sent", "cross-signing setup first", "recovery key"}},
		{rihma.ErrNoUserIdentity, []string{"no cross-signing identity", "Nothing was sent", "in Element"}},
		{rihma.ErrPeerDeviceIDConflict, []string{"device id BOTDEVICE", "Nothing was sent", "another id"}},
		{rihma.ErrSASMismatch, []string{"not confirmed as matching", "nothing was signed"}},
		{rihma.ErrSASCancelled, []string{"cancelled", "not answered in time", "nothing was signed", "direct chat"}},
		{fmt.Errorf("%w: %w", rihma.ErrSASCancelled, context.DeadlineExceeded), []string{"not answered in time"}},
		{rihma.ErrSASUnsigned, []string{"without covering their master key", "nothing was signed"}},
		{fmt.Errorf("rihma: signing: %w", rihma.ErrMasterKeyMismatch), []string{"identity changed during the verification", "not signed"}},
	}
	for _, c := range cases {
		got := verifyError(c.err, peer, device)
		if !errors.Is(got, c.err) {
			t.Errorf("verifyError(%v) = %v, which does not wrap it", c.err, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got.Error(), w) {
				t.Errorf("verifyError(%v) = %q, want it to say %q", c.err, got, w)
			}
		}
	}
	other := errors.New("other")
	if got := verifyError(other, peer, device); !errors.Is(got, other) || !strings.Contains(got.Error(), string(peer)) {
		t.Errorf("verifyError(other) = %v", got)
	}
}

// Verify refuses a malformed user, the bot itself, and a missing confirm
// before it opens the account, so these need no homeserver.
func TestVerifyRefusesBeforeOpening(t *testing.T) {
	cfg := &config.Matrix{UserID: "@bot:example.org"}
	yes := func(context.Context) (bool, error) { return true, nil }
	var out bytes.Buffer
	if _, err := Verify(context.Background(), cfg, "owner", VerifyIO{Out: &out, Confirm: yes}); err == nil || !strings.Contains(err.Error(), "not a Matrix user id") {
		t.Errorf("a malformed user = %v", err)
	}
	if _, err := Verify(context.Background(), cfg, "@owner:example.org", VerifyIO{Out: &out}); !errors.Is(err, ErrNoConfirm) {
		t.Errorf("no confirm = %v", err)
	}
	if _, err := Verify(context.Background(), cfg, "@bot:example.org", VerifyIO{Out: &out, Confirm: yes}); !errors.Is(err, rihma.ErrOwnUser) {
		t.Errorf("the bot itself = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q", out.String())
	}
}

// findDirect takes the first room m.direct lists for the user that is
// private to the two of them, and makes none.
func TestFindDirect(t *testing.T) {
	const bot, owner, eve = id.UserID("@bot:example.org"), id.UserID("@owner:example.org"), id.UserID("@eve:example.org")
	var direct map[id.UserID][]id.RoomID
	var writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			http.Error(w, "{}", http.StatusForbidden)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/account_data/m.direct") {
			http.NotFound(w, r)
			return
		}
		if direct == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"no data"}`))
			return
		}
		json.NewEncoder(w).Encode(direct)
	}))
	defer srv.Close()
	cli, err := mautrix.NewClient(srv.URL, bot, "token")
	if err != nil {
		t.Fatal(err)
	}
	st := mautrix.NewMemoryStateStore()
	cli.StateStore = st
	c := &rihma.Client{Client: cli}
	ctx := context.Background()

	room := func(name string, encrypted bool, rule event.JoinRule, members ...id.UserID) id.RoomID {
		r := id.RoomID("!" + name + ":example.org")
		if encrypted {
			st.SetEncryptionEvent(ctx, r, &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1})
		}
		st.SetJoinRules(ctx, r, &event.JoinRulesEventContent{JoinRule: rule})
		for _, m := range members {
			st.SetMember(ctx, r, m, &event.MemberEventContent{Membership: event.MembershipJoin})
		}
		return r
	}
	plain := room("plain", false, event.JoinRuleInvite, bot, owner)
	public := room("public", true, event.JoinRulePublic, bot, owner)
	crowded := room("crowded", true, event.JoinRuleInvite, bot, owner, eve)
	private := room("private", true, event.JoinRuleInvite, bot, owner)

	if got, err := findDirect(ctx, c, owner); err != nil || got != "" {
		t.Errorf("with no m.direct = %q, %v", got, err)
	}
	direct = map[id.UserID][]id.RoomID{owner: {plain, public, crowded}, eve: {private}}
	if got, err := findDirect(ctx, c, owner); err != nil || got != "" {
		t.Errorf("with no private room = %q, %v", got, err)
	}
	direct[owner] = append(direct[owner], private)
	if got, err := findDirect(ctx, c, owner); err != nil || got != private {
		t.Errorf("with a private room = %q, %v", got, err)
	}
	if writes != 0 {
		t.Errorf("findDirect wrote %d times", writes)
	}
}

// fakeVerifier stands in for rihma: it calls confirm with emoji, and
// completes only when confirm said yes, as VerifyUser does.
type fakeVerifier struct {
	room  id.RoomID
	err   error // returned instead of running the exchange
	key   id.Ed25519
	calls int
}

var testEmoji = func() []rihma.SASEmoji {
	var out []rihma.SASEmoji
	for _, e := range [][2]string{{"🐶", "Dog"}, {"🐱", "Cat"}, {"🦁", "Lion"}, {"🐎", "Horse"}, {"🦄", "Unicorn"}, {"🐷", "Pig"}, {"🐘", "Elephant"}} {
		out = append(out, rihma.SASEmoji{Emoji: e[0], Description: e[1]})
	}
	return out
}()

func (f *fakeVerifier) ids() (id.UserID, id.DeviceID) { return "@bot:example.org", "BOTDEVICE" }

func (f *fakeVerifier) direct(context.Context, id.UserID) (id.RoomID, error) { return f.room, nil }

func (f *fakeVerifier) verifyUser(ctx context.Context, _ id.UserID, confirm func([]rihma.SASEmoji) bool) (id.Ed25519, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if !confirm(testEmoji) {
		return "", rihma.ErrSASMismatch
	}
	return f.key, nil
}

func TestVerifyWith(t *testing.T) {
	const owner = id.UserID("@owner:example.org")
	ctx := context.Background()
	answer := func(ok bool, err error) (func(context.Context) (bool, error), *int) {
		n := 0
		return func(context.Context) (bool, error) { n++; return ok, err }, &n
	}

	t.Run("no direct chat", func(t *testing.T) {
		f := &fakeVerifier{}
		confirm, asked := answer(true, nil)
		var out bytes.Buffer
		_, err := verifyWith(ctx, f, owner, confirm, &operator{w: &out})
		if !errors.Is(err, ErrNoDirectRoom) || f.calls != 0 || *asked != 0 {
			t.Errorf("= %v, %d requests, %d answers", err, f.calls, *asked)
		}
	})

	t.Run("yes", func(t *testing.T) {
		f := &fakeVerifier{room: "!dm:example.org", key: "AAAABBBBCCCC"}
		confirm, asked := answer(true, nil)
		var out bytes.Buffer
		v, err := verifyWith(ctx, f, owner, confirm, &operator{w: &out})
		if err != nil || v != (Verified{User: string(owner), MasterKey: "AAAABBBBCCCC"}) || *asked != 1 {
			t.Fatalf("= %+v, %v, %d answers", v, err, *asked)
		}
		for _, w := range []string{"Accept it in Element", "1. 🐶  Dog", "7. 🐘  Elephant", "AAAA BBBB CCCC", "is verified"} {
			if !strings.Contains(out.String(), w) {
				t.Errorf("the output lacks %q:\n%s", w, out.String())
			}
		}
	})

	t.Run("no", func(t *testing.T) {
		f := &fakeVerifier{room: "!dm:example.org", key: "KEY"}
		confirm, _ := answer(false, nil)
		var out bytes.Buffer
		_, err := verifyWith(ctx, f, owner, confirm, &operator{w: &out})
		if !errors.Is(err, rihma.ErrSASMismatch) || !strings.Contains(out.String(), "nothing is signed") {
			t.Errorf("= %v\n%s", err, out.String())
		}
	})

	t.Run("an answer that could not be read", func(t *testing.T) {
		f := &fakeVerifier{room: "!dm:example.org", key: "KEY"}
		broken := errors.New("closed terminal")
		confirm, _ := answer(true, broken)
		var out bytes.Buffer
		_, err := verifyWith(ctx, f, owner, confirm, &operator{w: &out})
		if !errors.Is(err, broken) || !strings.Contains(err.Error(), "nothing was signed") {
			t.Errorf("= %v", err)
		}
	})

	t.Run("emoji that could not be shown", func(t *testing.T) {
		f := &fakeVerifier{room: "!dm:example.org", key: "KEY"}
		confirm, asked := answer(true, nil)
		broken := errors.New("broken terminal")
		// The header lines are written; the emoji are not.
		_, err := verifyWith(ctx, f, owner, confirm, &operator{w: failingAfter(3, broken)})
		if err == nil || *asked != 0 || f.calls != 1 {
			t.Errorf("= %v, %d answers", err, *asked)
		}
	})

	t.Run("an output that fails before the request", func(t *testing.T) {
		f := &fakeVerifier{room: "!dm:example.org", key: "KEY"}
		confirm, _ := answer(true, nil)
		broken := errors.New("broken terminal")
		_, err := verifyWith(ctx, f, owner, confirm, &operator{w: failingAfter(0, broken)})
		if !errors.Is(err, broken) || f.calls != 0 {
			t.Errorf("= %v, %d requests", err, f.calls)
		}
	})

	t.Run("rihma refuses", func(t *testing.T) {
		f := &fakeVerifier{room: "!dm:example.org", err: rihma.ErrPeerDeviceIDConflict}
		confirm, asked := answer(true, nil)
		var out bytes.Buffer
		_, err := verifyWith(ctx, f, owner, confirm, &operator{w: &out})
		if !errors.Is(err, rihma.ErrPeerDeviceIDConflict) || *asked != 0 {
			t.Errorf("= %v, %d answers", err, *asked)
		}
	})
}

// failingAfter is a writer that takes n writes and fails every one after.
func failingAfter(n int, err error) writerFn {
	return func(p []byte) (int, error) {
		if n == 0 {
			return 0, err
		}
		n--
		return len(p), nil
	}
}

type writerFn func([]byte) (int, error)

func (f writerFn) Write(p []byte) (int, error) { return f(p) }

func TestFingerprint(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "abcd": "abcd", "abcdefghi": "abcd efgh i"} {
		if got := fingerprint(in); got != want {
			t.Errorf("fingerprint(%q) = %q, want %q", in, got, want)
		}
	}
}
