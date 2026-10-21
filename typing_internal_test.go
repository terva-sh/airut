package airut

import (
	"context"
	"testing"
	"time"
)

// stalled is a Responder whose typing requests hang until their context
// ends, as on a platform that has stopped answering.
type stalled struct {
	Responder
	entered chan struct{}
}

func (s *stalled) Typing(ctx context.Context) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestKeepaliveStopsWhileARequestHangs(t *testing.T) {
	r := &stalled{entered: make(chan struct{}, 1)}
	// The chat's context lasts as long as the bot, past the turn.
	k := startKeepalive(context.Background(), r, 0, time.Millisecond)
	<-r.entered
	done := make(chan struct{})
	go func() { k.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop waited on a hanging typing request")
	}
}

func TestKeepalivePausesWithinTheWait(t *testing.T) {
	defer func(old time.Duration) { typingWait = old }(typingWait)
	typingWait = 20 * time.Millisecond
	r := &stalled{entered: make(chan struct{}, 1)}
	k := startKeepalive(context.Background(), r, 0, time.Hour)
	defer k.stop()
	<-r.entered
	done := make(chan struct{})
	go func() { k.pause(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a question waited on a hanging typing request past its bound")
	}
}
