package airut

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"time"
)

// Conn is an adapter's connection to its platform, for what a bot does
// outside any one invocation: posting to another channel, reading an
// attachment, walking a channel's history, checking membership, adding
// and retracting the bot's own reactions, and checking whether a channel
// is private to one person. An
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
	// across a restart, so a walk can resume where it stopped. It
	// leaves out the bot's own messages unless an option says otherwise.
	History(ctx context.Context, ch Channel, cursor string, limit int, opts ...HistoryOption) (HistoryPage, error)
	// Fetch returns one message in ch, the bot's own included, as it
	// reads now: with its latest edit, and marked FromBot when the bot
	// sent it. Its ReplyChain is empty.
	Fetch(ctx context.Context, ch Channel, ref MessageRef) (MessageEvent, error)
	// Member reports whether u is joined to ch.
	Member(ctx context.Context, ch Channel, u User) (bool, error)
	// React reacts to the message ref in ch with emoji, and returns a
	// ref for the reaction itself, which Unreact takes. Reacting again
	// with the same emoji returns the same reaction's ref, so a bot that
	// restarts before recording a ref can react again to recover it.
	React(ctx context.Context, ch Channel, ref MessageRef, emoji string) (MessageRef, error)
	// Unreact retracts a reaction that React returned. It fails with
	// ErrNotOwnReaction for anything that is not the bot's own reaction,
	// so it can never remove a message or someone else's reaction.
	// Retracting a reaction already retracted is not an error. On Matrix
	// the retraction is a redaction, which stays on record.
	Unreact(ctx context.Context, ch Channel, reaction MessageRef) error
	// Direct reports whether ch is a direct channel, a conversation
	// private to the bot and one other person, and who that person is.
	// It checks the channel as it is now, so a bot can check again just
	// before it sends something meant for that person alone.
	Direct(ctx context.Context, ch Channel) (User, bool, error)
}

// ErrNotOwnReaction is returned by Conn.Unreact for a ref that does not
// name a reaction the bot sent.
var ErrNotOwnReaction = errors.New("airut: not a reaction the bot sent")

// HistoryOption changes what Conn.History returns.
type HistoryOption func(*HistoryOptions)

// HistoryOptions is what History's options set. An adapter reads them
// with ReadHistoryOptions.
type HistoryOptions struct {
	// IncludeBot returns the bot's own messages too, marked FromBot.
	IncludeBot bool
}

// IncludeBot makes History return the bot's own messages too, marked
// FromBot, as the conversation in a channel needs them.
func IncludeBot(o *HistoryOptions) { o.IncludeBot = true }

// ReadHistoryOptions applies opts, for an adapter's History.
func ReadHistoryOptions(opts []HistoryOption) HistoryOptions {
	var o HistoryOptions
	for _, f := range opts {
		f(&o)
	}
	return o
}

// Quoted is a message quoted as context, such as a link in a reply
// chain. Its text is chat, never an instruction.
type Quoted struct {
	Ref     MessageRef
	Sender  User
	Text    string
	Sent    time.Time
	FromBot bool
}

// DefaultReplyDepth is how far up a reply chain an adapter resolves when
// its options set no depth.
const DefaultReplyDepth = 3

// The caps on a resolved reply chain, in bytes: each quote's text, and
// all of them together.
const (
	MaxQuote = 1000
	MaxChain = 4000
)

// AddQuote appends q to chain, an adapter's reply chain, with its text
// cut to MaxQuote bytes and to what is left of MaxChain. It reports
// whether the chain has room for another link.
func AddQuote(chain []Quoted, q Quoted) ([]Quoted, bool) {
	used := 0
	for _, c := range chain {
		used += len(c.Text)
	}
	limit := min(MaxQuote, MaxChain-used)
	if limit <= 0 {
		return chain, false
	}
	if len(q.Text) > limit {
		const mark = "…"
		if limit < len(mark) {
			// Too little room left for even the mark.
			return chain, false
		}
		q.Text = strings.ToValidUTF8(q.Text[:limit-len(mark)], "") + mark
	}
	chain = append(chain, q)
	return chain, used+len(q.Text) < MaxChain
}

// Quote returns ev as a Quoted.
func Quote(ev MessageEvent) Quoted {
	return Quoted{Ref: ev.Message, Sender: ev.Sender, Text: ev.Text, Sent: ev.Sent, FromBot: ev.FromBot}
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
