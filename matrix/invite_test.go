package matrix

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"terva.sh/airut/config"
)

// inviteSession is a session that has only what admit reads.
func inviteSession(o Options) (*session, *bytes.Buffer) {
	var buf bytes.Buffer
	return &session{opts: o, log: slog.New(slog.NewTextHandler(&buf, nil))}, &buf
}

func TestAdmitAsksTheInviteCallback(t *testing.T) {
	inv := Invite{Room: "!room:example.org", Inviter: "@alice:example.org", Direct: true}
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		o       Options
		want    bool
		logWant string
	}{
		{"neither", Options{}, false, "ignored an invite"},
		{"AcceptInvite yes", Options{AcceptInvite: func(Invite) bool { return true }}, true, "accepted an invite"},
		{"AcceptInvite no", Options{AcceptInvite: func(Invite) bool { return false }}, false, "ignored an invite"},
		{"CheckInvite yes", Options{CheckInvite: func(context.Context, Invite) (bool, error) { return true, nil }}, true, "accepted an invite"},
		{"CheckInvite no", Options{CheckInvite: func(context.Context, Invite) (bool, error) { return false, nil }}, false, "ignored an invite"},
		// An error refuses, even with a yes beside it.
		{"CheckInvite error", Options{CheckInvite: func(context.Context, Invite) (bool, error) {
			return true, errors.New("claims unreadable: /srv/secret/path")
		}}, false, "refused an invite its check could not decide"},
	} {
		s, logs := inviteSession(tc.o)
		if got := s.admit(ctx, inv); got != tc.want {
			t.Errorf("%s: admit = %v, want %v", tc.name, got, tc.want)
		}
		l := logs.String()
		if !strings.Contains(l, tc.logWant) || !strings.Contains(l, "inviter=@alice:example.org") || !strings.Contains(l, "room=!room:example.org") {
			t.Errorf("%s: the log does not name the decision, the inviter, and the room:\n%s", tc.name, l)
		}
		if strings.Contains(l, "/srv/secret/path") {
			t.Errorf("%s: the log holds the error's text:\n%s", tc.name, l)
		}
	}
}

func TestCheckInviteGetsTheInviteAndAContext(t *testing.T) {
	inv := Invite{Room: "!room:example.org", Inviter: "@alice:example.org"}
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "run")
	var got Invite
	var gotCtx context.Context
	s, _ := inviteSession(Options{CheckInvite: func(ctx context.Context, i Invite) (bool, error) {
		got, gotCtx = i, ctx
		return true, nil
	}})
	s.admit(ctx, inv)
	if got != inv || gotCtx == nil || gotCtx.Value(key{}) != "run" {
		t.Errorf("CheckInvite got %+v and a context without the session's value", got)
	}
}

// A check cut short by the bot stopping refuses quietly.
func TestACancelledCheckRefusesWithoutAWarning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, logs := inviteSession(Options{CheckInvite: func(ctx context.Context, _ Invite) (bool, error) { return false, ctx.Err() }})
	if s.admit(ctx, Invite{Room: "!room:example.org", Inviter: "@alice:example.org"}) {
		t.Error("a cancelled check admitted the invite")
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a cancelled check warned:\n%s", logs.String())
	}
}

func TestRunRefusesBothInviteCallbacks(t *testing.T) {
	a := New(&config.Matrix{}, Options{
		AcceptInvite: func(Invite) bool { return true },
		CheckInvite:  func(context.Context, Invite) (bool, error) { return true, nil },
	})
	if err := a.Run(context.Background(), nil); !errors.Is(err, errBothInviteChecks) {
		t.Errorf("Run = %v, want %v", err, errBothInviteChecks)
	}
}
