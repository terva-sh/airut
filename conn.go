package airut

import (
	"context"
	"errors"
	"io"
	"slices"
)

// Conn is an adapter's connection to its platform, for what a bot does
// outside any one invocation: posting to another channel, reading an
// attachment, walking a channel's history, and checking membership. An
// adapter announces its Conn through Host.Ready once it is connected.
// Its methods must be safe for concurrent use.
type Conn interface {
	Platform() Platform
	// Send posts m to ch. Message.ReplyTo makes it a reply.
	Send(ctx context.Context, ch Channel, m Message) (MessageRef, error)
	// Open returns an attachment's bytes, downloaded with the platform's
	// own authentication. The caller closes it and bounds what it reads.
	Open(ctx context.Context, a Attachment) (io.ReadCloser, error)
	// History returns up to limit messages older than cursor, newest
	// first, as the events they were. An empty cursor starts from the
	// newest message. Next is the cursor for the following page, and is
	// empty at the start of what the bot may read. Cursors stay valid
	// across a restart, so a walk can resume where it stopped.
	History(ctx context.Context, ch Channel, cursor string, limit int) (HistoryPage, error)
	// Member reports whether u is joined to ch.
	Member(ctx context.Context, ch Channel, u User) (bool, error)
}

// HistoryPage is one page of a channel's history.
type HistoryPage struct {
	Messages []MessageEvent
	Next     string
}

// ErrNoConn is returned when the platform's adapter has not announced a
// Conn: it is not connected yet, or it offers none.
var ErrNoConn = errors.New("airut: the platform is not connected")

// OnReady registers a handler that runs each time an adapter announces
// its Conn, such as after a reconnect. It is where a bot resumes work
// that outlives an invocation. Each call runs on a goroutine of its own
// with the bot's context, and Run waits for it before returning.
// OnReady panics once the bot is running.
func (b *Bot) OnReady(h func(ctx context.Context, c Conn)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		panic("airut: OnReady called after Run")
	}
	b.onReady = append(b.onReady, h)
}

// Conn returns the Conn the adapter for p announced.
func (b *Bot) Conn(p Platform) (Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.conns[p]; c != nil {
		return c, nil
	}
	return nil, ErrNoConn
}

// Conn returns the Conn of the platform the invocation or event came
// from. A message sent through it is posted even when an agent turn
// called the command, so a tool's author decides whether a tool may.
func (c *Ctx) Conn() (Conn, error) { return c.bot.Conn(c.Platform()) }

// ready records conn and runs the OnReady handlers.
func (b *Bot) ready(ctx context.Context, conn Conn) {
	if conn == nil {
		return
	}
	b.mu.Lock()
	if b.conns == nil {
		b.conns = map[Platform]Conn{}
	}
	b.conns[conn.Platform()] = conn
	handlers := slices.Clone(b.onReady)
	b.mu.Unlock()
	log := b.cfg.Logger.With("platform", string(conn.Platform()))
	for i, h := range handlers {
		b.inflight.Go(func() {
			defer func() {
				if e := recovered(recover()); e != nil {
					log.Error("ready handler panicked", "handler", i, "stack", string(e.(panicError).stack))
				}
			}()
			h(ctx, conn)
		})
	}
}
