//go:build e2e

package matrix_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/config"
	"terva.sh/airut/matrix"
)

// TestE2ECrossSigning runs the setup on a new account, again on a second
// device, and starts bots on further devices with and without the
// recovery key configured.
func TestE2ECrossSigning(t *testing.T) {
	hs := homeserver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := "bot-" + tag()
	register(t, ctx, hs, name)
	device := func(n string) *config.Matrix {
		return &config.Matrix{Homeserver: hs, UserID: "@" + name + ":localhost", Password: config.NewSecret("pw-" + name),
			StateDir: t.TempDir(), DeviceName: "airut e2e " + n}
	}

	// An output that cannot be written stops the setup before it creates
	// anything.
	first := device("first")
	broken := errors.New("broken terminal")
	if st, err := matrix.CrossSign(ctx, first, matrix.CrossSignIO{Out: failAfter(0, broken)}); !errors.Is(err, broken) || st != (matrix.CrossSigningState{}) {
		t.Fatalf("the setup on a broken output = %+v, %v", st, err)
	}

	// A new account gets an identity, and the key is shown once.
	var out bytes.Buffer
	st, err := matrix.CrossSign(ctx, first, matrix.CrossSignIO{Out: &out})
	if err != nil || st != (matrix.CrossSigningState{Identity: true, DeviceSigned: true}) {
		t.Fatalf("the first setup = %+v, %v\n%s", st, err, out.String())
	}
	key := ""
	lines := strings.Split(out.String(), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "Recovery key") && i+2 < len(lines) {
			key = strings.TrimSpace(lines[i+2])
		}
	}
	if key == "" {
		t.Fatalf("the setup showed no recovery key:\n%s", out.String())
	}
	// Run again on the same device, it changes nothing and shows no key.
	out.Reset()
	if st, err := matrix.CrossSign(ctx, first, matrix.CrossSignIO{Out: &out}); err != nil || !st.DeviceSigned || strings.Contains(out.String(), key) {
		t.Errorf("the setup again = %+v, %v\n%s", st, err, out.String())
	}
	if st, err := matrix.CrossSign(ctx, first, matrix.CrossSignIO{Out: failAfter(0, broken)}); !errors.Is(err, broken) || !st.DeviceSigned {
		t.Errorf("the setup again on a broken output = %+v, %v", st, err)
	}

	// Another device: the identity stays, and the pasted key signs it.
	second := device("second")
	out.Reset()
	st, err = matrix.CrossSign(ctx, second, matrix.CrossSignIO{Out: &out})
	if !errors.Is(err, matrix.ErrNeedRecoveryKey) || st != (matrix.CrossSigningState{Identity: true}) || !strings.Contains(out.String(), "never replaced") {
		t.Errorf("a second device with no key = %+v, %v\n%s", st, err, out.String())
	}
	st, err = matrix.CrossSign(ctx, second, matrix.CrossSignIO{Out: &out, RecoveryKey: func(context.Context) (string, error) { return key, nil }})
	if err != nil || st != (matrix.CrossSigningState{Identity: true, DeviceSigned: true}) {
		t.Errorf("a second device with the key = %+v, %v\n%s", st, err, out.String())
	}

	// A key that cannot be written is an error that says it is lost.
	other := "lost-" + tag()
	register(t, ctx, hs, other)
	lost := &config.Matrix{Homeserver: hs, UserID: "@" + other + ":localhost", Password: config.NewSecret("pw-" + other),
		StateDir: t.TempDir(), DeviceName: "airut e2e lost"}
	if _, err := matrix.CrossSign(ctx, lost, matrix.CrossSignIO{Out: failAfter(1, broken)}); !errors.Is(err, broken) || !strings.Contains(err.Error(), "lost") {
		t.Errorf("the setup that could not write its key = %v", err)
	}

	// A bot whose device is new signs itself with the configured key.
	third := device("third")
	third.RecoveryKey = config.NewSecret(key)
	a, logs := runAdapter(t, ctx, third)
	if st := waitCrossSigning(t, ctx, a, true); !st.Identity {
		t.Errorf("the bot reports %+v", st)
	}
	if l := logs(); !strings.Contains(l, "signed the device with the recovery key") || strings.Contains(l, key) {
		t.Errorf("the bot's log:\n%s", l)
	}

	// A bot with no key says its device is not signed, and runs anyway.
	fourth := device("fourth")
	b, logs := runAdapter(t, ctx, fourth)
	if st := waitCrossSigning(t, ctx, b, false); !st.Identity {
		t.Errorf("the unsigned bot reports %+v", st)
	}
	if l := logs(); !strings.Contains(l, "the device is not cross-signed") {
		t.Errorf("the unsigned bot's log:\n%s", l)
	}
}

// failAfter is a writer that takes n writes and fails every one after.
func failAfter(n int, err error) writerFunc {
	return func(p []byte) (int, error) {
		if n == 0 {
			return 0, err
		}
		n--
		return len(p), nil
	}
}

// runAdapter runs a bot with no commands on the account cfg describes,
// and returns its adapter and a reader of its log.
func runAdapter(t *testing.T, ctx context.Context, cfg *config.Matrix) (*matrix.Adapter, func() string) {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := matrix.New(cfg, matrix.Options{})
	bot := airut.New(airut.Config{Logger: log})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- bot.Run(runCtx, a) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
	return a, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// waitCrossSigning waits until the running adapter reports its device
// signed as want.
func waitCrossSigning(t *testing.T, ctx context.Context, a *matrix.Adapter, want bool) matrix.CrossSigningState {
	t.Helper()
	for {
		st, err := a.CrossSigning(ctx)
		if err == nil && st.DeviceSigned == want {
			return st
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the adapter never reported DeviceSigned %v: %+v, %v", want, st, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
