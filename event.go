package airut

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Event is something that happened on a platform other than a command:
// a MessageEvent or a ReactionEvent. An adapter hands one to
// Host.Deliver, and the core runs the handlers registered for its kind.
type Event interface{ event() }

// MessageEvent is a message that was not a command. Adapters never
// deliver the bot's own messages.
type MessageEvent struct {
	// ID identifies the event in logs. It never holds chat text.
	ID      string
	Message MessageRef
	Sender  User
	Roles   []Role
	Channel Channel
	// Thread identifies the thread the message is in, if any.
	Thread string
	// Direct reports that the message arrived in a direct channel: a
	// conversation private to the bot and the sender, as Conn.Direct
	// checks one. The adapter checks it for each message, so it is false
	// from the first message after a third person joins.
	Direct bool
	Text   string
	// Mentioned reports that the message addresses the bot: it opens
	// with a mention of the bot, by the rule the adapter uses for
	// commands, or the platform marks the bot as mentioned in it, or it
	// arrived in a direct channel, where everything addresses the bot. A
	// mention of everyone in the channel does not count.
	Mentioned bool
	// Request is what an addressed message asks of the bot: Text without
	// its opening mention, or Text itself when the mention was elsewhere.
	// It is empty when Mentioned is false.
	Request string
	// Files are the message's attachments, such as an image or a video.
	// Conn.Open reads their bytes.
	Files []Attachment
	// ReplyTo is the message this one replies to; its ID is empty when
	// it replies to none.
	ReplyTo MessageRef
	// ReplyChain is the message ReplyTo names, then the one that replies
	// to, and so on, nearest first. The adapter resolves it only for a
	// message that addresses the bot, to the depth it is configured
	// with, and stops at a message it cannot read. For any other message
	// it is empty, and Conn.Fetch follows ReplyTo instead.
	ReplyChain []Quoted
	// FromBot reports that the bot sent the message. Only Conn.Fetch,
	// and Conn.History with IncludeBot, return such a message.
	FromBot bool
	// Sent is when the platform says the message was sent. After
	// downtime a handler receives what it missed, so a bot that answers
	// chat can leave an old message unanswered. It is zero when the
	// platform gives no time.
	Sent time.Time
	// Native is the platform's own event, for the escape hatch.
	Native any
}

// ReactionEvent is a reaction added to a message. Adapters never deliver
// the bot's own reactions.
type ReactionEvent struct {
	// ID identifies the event in logs. It never holds chat text.
	ID      string
	Sender  User
	Roles   []Role
	Channel Channel
	// Thread identifies the thread the reacted message is in, if any.
	Thread string
	// Direct reports that the reaction was added in a direct channel.
	Direct bool
	// Target is the message reacted to. Its ID is the platform's own and
	// stays the same across restarts, so a bot can match a reaction to a
	// message it posted long before.
	Target MessageRef
	Emoji  string
	Native any
}

func (MessageEvent) event()  {}
func (ReactionEvent) event() {}

// OnMessage registers a handler for every message that is not a command.
// It may be called more than once; each handler sees every message, in
// the order they were registered. The handler's Ctx has the sender as
// its caller, and Reply answers the message. A handler that fails is
// logged by kind and never answered in the chat, so a handler speaks
// only when it chooses to. OnMessage panics once the bot is running.
func (b *Bot) OnMessage(h func(*Ctx, MessageEvent) error) {
	b.on("OnMessage", func(c *Ctx, ev Event) error { return h(c, ev.(MessageEvent)) }, &b.onMessage)
}

// OnReaction registers a handler for reactions, as OnMessage does for
// messages. The handler's Ctx treats the reacted message as the one it
// answers.
func (b *Bot) OnReaction(h func(*Ctx, ReactionEvent) error) {
	b.on("OnReaction", func(c *Ctx, ev Event) error { return h(c, ev.(ReactionEvent)) }, &b.onReaction)
}

type eventHandler func(*Ctx, Event) error

func (b *Bot) on(what string, h eventHandler, list *[]eventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		panic("airut: " + what + " called after Run")
	}
	*list = append(*list, h)
}

// eventInvocation is the invocation a handler's Ctx stands on.
func eventInvocation(ev Event, r Responder) (Invocation, string) {
	switch e := ev.(type) {
	case MessageEvent:
		return Invocation{
			ID: e.ID, Caller: e.Sender, Roles: e.Roles, Channel: e.Channel, Thread: e.Thread, Direct: e.Direct,
			Message: e.Message, ReplyTo: e.ReplyTo, ReplyChain: e.ReplyChain, Respond: r, Native: e.Native,
		}, "message"
	case ReactionEvent:
		return Invocation{
			ID: e.ID, Caller: e.Sender, Roles: e.Roles, Channel: e.Channel, Thread: e.Thread, Direct: e.Direct,
			Message: e.Target, Respond: r, Native: e.Native,
		}, "reaction"
	}
	return Invocation{}, ""
}

// normalize turns a pointer to an event into the event, so an adapter
// may deliver either. It reports false for a nil pointer.
func normalize(ev Event) (Event, bool) {
	switch e := ev.(type) {
	case *MessageEvent:
		if e == nil {
			return nil, false
		}
		return *e, true
	case *ReactionEvent:
		if e == nil {
			return nil, false
		}
		return *e, true
	case nil:
		return nil, false
	}
	return ev, true
}

// deliver runs ev's handlers in order. A handler's failure is logged and
// does not stop the next.
func (b *Bot) deliver(ctx context.Context, ev Event, r Responder) {
	b.inflight.Add(1)
	defer b.inflight.Done()
	ev, ok := normalize(ev)
	inv, kind := eventInvocation(ev, r)
	if !ok || kind == "" {
		b.cfg.Logger.Error("an adapter delivered an event the core does not know", "type", fmt.Sprintf("%T", ev))
		return
	}
	b.mu.Lock()
	var handlers []eventHandler
	switch kind {
	case "message":
		handlers = b.onMessage
	case "reaction":
		handlers = b.onReaction
	}
	b.mu.Unlock()
	if len(handlers) == 0 {
		return
	}
	log := b.cfg.Logger.With("event", inv.ID, "kind", kind, "platform", string(inv.Caller.Platform))
	start := time.Now()
	defer func() { b.metrics.Since("airut_event_seconds", start, "kind", kind) }()
	for i, h := range handlers {
		c := &Ctx{ctx: ctx, bot: b, inv: inv, resp: r}
		err := runHandler(h, c, ev)
		o := "ok"
		if err != nil {
			o = errorKind(err)
		}
		b.metrics.Add("airut_events_total", 1, "kind", kind, "outcome", o)
		if err == nil {
			continue
		}
		var pe panicError
		if errors.As(err, &pe) {
			log.Error("event handler panicked", "handler", i, "stack", string(pe.stack))
		} else if !errors.Is(err, context.Canceled) {
			log.Info("event handler failed", "handler", i, "error_kind", errorKind(err))
		}
	}
}

func runHandler(h eventHandler, c *Ctx, ev Event) (err error) {
	defer func() {
		if e := recovered(recover()); e != nil {
			err = e
		}
	}()
	return h(c, ev)
}
