package airut

import (
	"context"
	"sync"
	"time"
)

// TypingStopper is a Responder that can take its typing notice down
// before the notice lapses. An agent turn's keepalive uses it when the
// turn waits on a person and when the turn ends; with a Responder that
// lacks it, the notice lapses on its own.
type TypingStopper interface {
	StopTyping(ctx context.Context) error
}

// The keepalive's defaults. A Discord typing notice lasts about ten
// seconds, so it is renewed before then.
const (
	defaultTypingDelay = time.Second
	defaultTypingEvery = 8 * time.Second
)

// typingWait bounds each typing request, so a platform that stalls
// cannot hold up a question or the end of a turn for longer.
var typingWait = 5 * time.Second

// turnState is what the Ctxs of one running agent turn share: the
// streamer to flush, and the typing to pause, while a person is asked.
type turnState struct {
	// scope and key are the turn's session scope and the session it
	// resolved to, which SessionKey names for that scope.
	scope SessionScope
	key   string

	mu     sync.Mutex
	flush  func()
	typing *keepalive
	calls  map[any]int // calls of a tool that caps them per turn
}

// count counts a call of tool and reports whether it is within max.
func (s *turnState) count(tool any, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[any]int{}
	}
	if s.calls[tool] >= max {
		return false
	}
	s.calls[tool]++
	return true
}

// asking runs ask with the turn's reply complete and its typing paused.
func (s *turnState) asking(ask func() (string, error)) (string, error) {
	s.mu.Lock()
	flush := s.flush
	s.mu.Unlock()
	if flush != nil {
		flush()
	}
	if s.typing != nil {
		s.typing.pause()
		defer s.typing.resume()
	}
	return ask()
}

// keepalive shows a typing notice through a turn: first after delay,
// so a turn that ends at once shows none, then every interval, except
// while paused.
type keepalive struct {
	// ctx ends when the keepalive stops, which cancels a request still
	// under way; parent is the chat's, for the last take-down.
	ctx    context.Context
	cancel context.CancelFunc
	parent context.Context
	resp   Responder
	every  time.Duration

	// mu is held across each Typing and StopTyping call, so a renewal
	// already under way cannot land after a pause took the notice down.
	mu      sync.Mutex
	paused  int
	due     bool // the delay has passed
	showing bool // a notice was sent and not taken down
	wake    chan struct{}
	done    chan struct{}
	exited  chan struct{}
}

func startKeepalive(ctx context.Context, resp Responder, delay, every time.Duration) *keepalive {
	kctx, cancel := context.WithCancel(ctx)
	k := &keepalive{
		ctx: kctx, cancel: cancel, parent: ctx, resp: resp, every: every,
		wake: make(chan struct{}, 1), done: make(chan struct{}), exited: make(chan struct{}),
	}
	go k.loop(max(delay, 0))
	return k
}

func (k *keepalive) loop(delay time.Duration) {
	defer close(k.exited)
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-k.ctx.Done():
			return
		case <-k.wake:
			t.Stop()
		case <-t.C:
		}
		k.mu.Lock()
		k.due = true
		if k.paused == 0 {
			ctx, cancel := context.WithTimeout(k.ctx, typingWait)
			if k.resp.Typing(ctx) == nil {
				k.showing = true
			}
			cancel()
		}
		k.mu.Unlock()
		t.Reset(k.every)
	}
}

// pause stops renewing the notice and takes it down where the platform
// can.
func (k *keepalive) pause() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.paused++
	k.takeDown(k.ctx)
}

// resume renews the notice at once when the pause ends after the delay.
func (k *keepalive) resume() {
	k.mu.Lock()
	k.paused--
	again := k.paused == 0 && k.due
	k.mu.Unlock()
	if again {
		select {
		case k.wake <- struct{}{}:
		default:
		}
	}
}

// stop ends the keepalive, cancelling a request still under way, and
// takes the notice down.
func (k *keepalive) stop() {
	close(k.done)
	k.cancel()
	<-k.exited
	k.mu.Lock()
	defer k.mu.Unlock()
	k.takeDown(k.parent)
}

// takeDown takes the notice down within typingWait. It is called with mu
// held.
func (k *keepalive) takeDown(ctx context.Context) {
	if ts, ok := k.resp.(TypingStopper); ok && k.showing && ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(ctx, typingWait)
		_ = ts.StopTyping(ctx)
		cancel()
	}
	k.showing = false
}
