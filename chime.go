package airut

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// ChimeOptions configures a listener that lets the bot speak up in a
// conversation nobody asked it into. Chime asks the bot's Judge, and
// runs Handler only on a yes.
type ChimeOptions struct {
	// Channels are where the listener listens. It never judges a message
	// in a direct channel, or one that addresses the bot, because those
	// are the bot's own handlers' to answer.
	Channels []Channel
	// Persona names the judge's instructions, as Judgement.Persona does.
	Persona string
	// Prompt is the question the judge answers about each batch. It is
	// the app's own text, never chat.
	Prompt string
	// ContextMessages is how many of the channel's earlier messages the
	// judge reads before a batch, at least. The window keeps its start
	// while it grows to twice this, then drops to this in one step, so
	// successive judgements share their opening and a model server can
	// reuse its prompt cache. It defaults to 20, and is at most
	// MaxRoomMessages.
	ContextMessages int
	// Settle is how long a channel must be quiet before its new messages
	// are judged together, and MaxWait how long the first of them waits
	// at most while new ones keep coming. They default to 3 and 15
	// seconds.
	Settle  time.Duration
	MaxWait time.Duration
	// MaxAge is how old a message may be when it arrives and still be
	// judged, by MessageEvent.Sent, so a bot catching up after downtime
	// does not answer what it missed. It defaults to two minutes.
	MaxAge time.Duration
	// MaxPerHour caps how often Handler runs in one channel in any hour.
	// It must be positive. A channel at its cap is not judged.
	MaxPerHour int
	// Cooldown is how long after Handler returns that a channel is not
	// judged. Nothing in the channel is judged while it runs.
	Cooldown time.Duration
	// StayOut keeps the bot out of exchanges between other people: a
	// message that replies to someone other than its sender and the bot
	// is never answered.
	StayOut bool
	// Misses are the reactions that mark a reply Handler sent as a miss,
	// such as "👎". airut_chime_misses_total counts each message marked
	// once.
	Misses []string
	// Handler runs on a yes. Its Ctx stands on the message the judge
	// chose, so Reply answers it, and its caller is that message's
	// sender. An agent turn it starts is charged to the channel, never to
	// the sender: it needs the channel in the Gate's Access.Channels for
	// the platform, and takes one of the Gate's PerChannel turns but none
	// of PerUser. A failure is logged by kind and never answered.
	Handler func(*Ctx, Chimed) error
}

// Chimed is what a Chime handler is given.
type Chimed struct {
	// Target is the message the judge chose to answer.
	Target MessageEvent
	// Reason is the judge's own words. It is model text drawn from chat,
	// so a handler may show it but must never log it or follow it.
	Reason string
}

// The defaults of ChimeOptions.
const (
	defaultChimeContext = 20
	defaultChimeSettle  = 3 * time.Second
	defaultChimeMaxWait = 15 * time.Second
	defaultChimeMaxAge  = 2 * time.Minute
	// chimeSentMemory is how many of Handler's replies a listener
	// remembers, to count a miss on one.
	chimeSentMemory = 256
)

// Chime registers a listener that speaks up unprompted. In each of its
// channels it gathers the messages that arrive until the channel has
// been quiet for Settle, or MaxWait has passed, and asks the judge
// about them together, one judgement at a time per channel. Messages
// that arrive meanwhile wait for the next. The judge reads the
// channel's earlier messages as context; the bot's own messages are
// never delivered, so they are not among them.
//
// A yes runs Handler, unless the moment has passed. Chime drops a yes
// when a message that arrived after the batch closed replies to the
// target, or comes from someone else and replies to nothing, since
// either may already be the answer; when StayOut is set and the target
// replies to another person; and when the channel is at MaxPerHour.
//
// Chime counts airut_chime_total by persona and outcome, and logs ids
// and outcomes: never the chat, the prompt, or the reason. It panics on
// options that cannot work, on a bot with no Judge, and once the bot is
// running.
func (b *Bot) Chime(o ChimeOptions) {
	switch {
	case b.cfg.Judge == nil:
		panic("airut: Chime needs Config.Judge")
	case o.Handler == nil:
		panic("airut: Chime needs a Handler")
	case len(o.Channels) == 0:
		panic("airut: Chime needs at least one channel")
	case o.MaxPerHour <= 0:
		panic("airut: Chime needs a positive MaxPerHour")
	case o.ContextMessages < 0 || o.ContextMessages > MaxRoomMessages:
		panic(fmt.Sprintf("airut: Chime's ContextMessages must be between 0 and %d", MaxRoomMessages))
	case o.Settle < 0 || o.MaxWait < 0 || o.MaxAge < 0 || o.Cooldown < 0:
		panic("airut: Chime's durations cannot be negative")
	}
	if o.ContextMessages == 0 {
		o.ContextMessages = defaultChimeContext
	}
	if o.Settle == 0 {
		o.Settle = defaultChimeSettle
	}
	if o.MaxWait == 0 {
		o.MaxWait = defaultChimeMaxWait
	}
	if o.MaxAge == 0 {
		o.MaxAge = defaultChimeMaxAge
	}
	k := &chime{
		b: b, o: o, in: map[string]bool{}, rooms: map[string]*chimeRoom{}, sent: map[string]bool{},
		spoken: &limiter{now: time.Now, seen: map[string][]time.Time{}}, now: time.Now,
	}
	for _, ch := range o.Channels {
		k.in[roomKey(ch)] = true
	}
	b.on("Chime", func(c *Ctx, ev Event) error { k.message(c, ev.(MessageEvent)); return nil }, &b.onMessage)
	if len(o.Misses) > 0 {
		b.on("Chime", func(c *Ctx, ev Event) error { k.reaction(ev.(ReactionEvent)); return nil }, &b.onReaction)
	}
}

// chime is one Chime listener.
type chime struct {
	b      *Bot
	o      ChimeOptions
	in     map[string]bool // the channels, by roomKey
	spoken *limiter        // Handler's runs, under MaxPerHour
	now    func() time.Time

	mu    sync.Mutex
	rooms map[string]*chimeRoom
	// sent remembers the replies Handler sent, by platform and id, the
	// oldest first in order.
	sent  map[string]bool
	order []string
}

// chimeRoom is the listener's state in one channel.
type chimeRoom struct {
	// window is the context the judge reads: the channel's messages
	// before the batch, oldest first.
	window []Quoted
	// senders are the senders of the messages in window and pending, by
	// message id, to tell a reply to a person without a fetch.
	senders map[string]string
	pending []chimeMsg
	// opened and last are when the first and the last pending message
	// arrived.
	opened, last time.Time
	// busy is set while a worker runs for the room.
	busy bool
	// quiet is when the cooldown ends.
	quiet time.Time
}

// chimeMsg is a message waiting to be judged.
type chimeMsg struct {
	ctx context.Context
	ev  MessageEvent
	r   Responder
	// judge is false for a message the judge reads but may not choose,
	// such as one that addresses the bot.
	judge bool
}

func roomKey(ch Channel) string { return string(ch.Platform) + "/channel/" + ch.ID }

// message queues ev in its room, and starts the room's worker if none
// runs. The bot's own messages, commands, and stale messages are never
// queued.
func (k *chime) message(c *Ctx, ev MessageEvent) {
	key := roomKey(ev.Channel)
	if !k.in[key] || ev.Direct || ev.FromBot {
		return
	}
	if _, _, ok := SplitCommand(ev.Text, k.b.cfg.Prefix); ok {
		return
	}
	now := k.now()
	if !ev.Sent.IsZero() && now.Sub(ev.Sent) > k.o.MaxAge {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	room := k.rooms[key]
	if room == nil {
		room = &chimeRoom{senders: map[string]string{}}
		k.rooms[key] = room
	}
	m := chimeMsg{ctx: c.ctx, ev: ev, r: c.resp, judge: !ev.Mentioned}
	if k.o.StayOut && m.judge && ev.ReplyTo.ID != "" {
		if s, ok := room.senders[ev.ReplyTo.ID]; ok && s != ev.Sender.ID {
			m.judge = false
		}
	}
	room.senders[ev.Message.ID] = ev.Sender.ID
	if len(room.pending) == 0 {
		room.opened = now
	}
	room.pending = append(room.pending, m)
	room.last = now
	if !room.busy {
		room.busy = true
		// The delivery that called add holds the bot's inflight count, so
		// this Add cannot race Run's Wait.
		k.b.inflight.Add(1)
		go k.work(room)
	}
}

// work judges room's pending messages until none are left. It runs
// while the room is busy, and holds one of the bot's inflight counts.
func (k *chime) work(room *chimeRoom) {
	defer k.b.inflight.Done()
	for {
		k.mu.Lock()
		if len(room.pending) == 0 {
			room.busy = false
			k.mu.Unlock()
			return
		}
		due := room.last.Add(k.o.Settle)
		if latest := room.opened.Add(k.o.MaxWait); latest.Before(due) {
			due = latest
		}
		ctx := room.pending[len(room.pending)-1].ctx
		k.mu.Unlock()
		if wait := due.Sub(k.now()); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
				continue
			case <-ctx.Done():
				t.Stop()
				k.mu.Lock()
				room.pending, room.busy = nil, false
				k.mu.Unlock()
				return
			}
		}
		k.round(room)
	}
}

// round judges the pending messages as one batch, and runs Handler on a
// yes the moment has not passed.
func (k *chime) round(room *chimeRoom) {
	k.mu.Lock()
	batch := room.pending
	room.pending = nil
	earlier := slices.Clone(room.window)
	room.remember(batch, k.o.ContextMessages)
	cooling := k.now().Before(room.quiet)
	k.mu.Unlock()

	last := batch[len(batch)-1]
	key := roomKey(last.ev.Channel)
	switch {
	case !slices.ContainsFunc(batch, func(m chimeMsg) bool { return m.judge }):
		return
	case cooling:
		k.outcome(last.ev, "cooldown")
		return
	case k.spoken.atCap(key, k.o.MaxPerHour, time.Hour):
		k.outcome(last.ev, "limited")
		return
	}
	quotes := make([]Quoted, len(batch))
	for i, m := range batch {
		quotes[i] = chimeQuote(m.ev)
	}
	inv, _ := eventInvocation(last.ev, last.r)
	c := &Ctx{ctx: last.ctx, bot: k.b, inv: inv, resp: last.r}
	v, _ := c.Judge(Judgement{Persona: k.o.Persona, Prompt: k.o.Prompt, Context: earlier, Batch: quotes})
	if v.Decision != Yes {
		k.outcome(last.ev, "declined")
		return
	}
	i := slices.IndexFunc(batch, func(m chimeMsg) bool { return m.ev.Message == v.Target })
	if i < 0 || !batch[i].judge {
		k.outcome(last.ev, "not_judged")
		return
	}
	target := batch[i]
	if k.o.StayOut && k.repliesToPerson(target, room) {
		k.outcome(target.ev, "stayed_out")
		return
	}
	// After the StayOut check, which may wait on a fetch, so a message
	// that arrived meanwhile counts.
	k.mu.Lock()
	newer := slices.Clone(room.pending)
	k.mu.Unlock()
	if overtaken(target.ev, newer) {
		k.outcome(target.ev, "stale")
		return
	}
	// The cap was checked before judging, and only this room's rounds,
	// which run one at a time, count against it, so the room is still
	// under it.
	limit := Invocation{Caller: User{Platform: target.ev.Channel.Platform}, Channel: target.ev.Channel}
	_ = k.spoken.take(Gate{PerChannel: k.o.MaxPerHour, Window: time.Hour}, limit)
	k.speak(room, target, v.Reason)
}

// speak runs Handler on target. The room's worker runs it, so the room
// is not judged meanwhile, and then cools down for Cooldown.
func (k *chime) speak(room *chimeRoom, target chimeMsg, reason string) {
	defer func() {
		k.mu.Lock()
		room.quiet = k.now().Add(k.o.Cooldown)
		k.mu.Unlock()
	}()
	inv, _ := eventInvocation(target.ev, target.r)
	c := &Ctx{ctx: target.ctx, bot: k.b, inv: inv, resp: target.r, chime: k}
	err := runChime(k.o.Handler, c, Chimed{Target: target.ev, Reason: reason})
	if err == nil {
		k.outcome(target.ev, "spoke")
		return
	}
	k.outcome(target.ev, "failed")
	var pe panicError
	if errors.As(err, &pe) {
		k.b.cfg.Logger.Error("chime handler panicked", "event", target.ev.ID, "persona", k.o.Persona, "stack", string(pe.stack))
	} else if !errors.Is(err, context.Canceled) {
		k.b.cfg.Logger.Info("chime handler failed", "event", target.ev.ID, "persona", k.o.Persona, "error_kind", errorKind(err))
	}
}

func runChime(h func(*Ctx, Chimed) error, c *Ctx, e Chimed) (err error) {
	defer func() {
		if e := recovered(recover()); e != nil {
			err = e
		}
	}()
	return h(c, e)
}

// outcome counts and logs how a round ended.
func (k *chime) outcome(ev MessageEvent, o string) {
	k.b.metrics.Add("airut_chime_total", 1, "persona", k.o.Persona, "outcome", o)
	k.b.cfg.Logger.Debug("chime", "event", ev.ID, "persona", k.o.Persona, "outcome", o)
}

// remember moves batch into the window. The window keeps its start
// while it grows to twice n, then keeps only its last n, so successive
// judgements open with the same messages.
func (room *chimeRoom) remember(batch []chimeMsg, n int) {
	for _, m := range batch {
		room.window = append(room.window, chimeQuote(m.ev))
	}
	if len(room.window) > 2*n {
		for _, q := range room.window[:len(room.window)-n] {
			delete(room.senders, q.Ref.ID)
		}
		room.window = slices.Clone(room.window[len(room.window)-n:])
	}
}

// chimeQuote quotes a message for the judge, with its text cut to
// MaxQuote bytes.
func chimeQuote(ev MessageEvent) Quoted {
	q := Quote(ev)
	if len(q.Text) > MaxQuote {
		const mark = "…"
		q.Text = strings.ToValidUTF8(q.Text[:MaxQuote-len(mark)], "") + mark
	}
	return q
}

// overtaken reports whether a message that arrived after target's batch
// closed may already answer it: one that replies to it, or one from
// someone else that replies to nothing.
func overtaken(target MessageEvent, newer []chimeMsg) bool {
	return slices.ContainsFunc(newer, func(m chimeMsg) bool {
		return m.ev.ReplyTo == target.Message ||
			m.ev.ReplyTo.ID == "" && m.ev.Sender.ID != target.Sender.ID
	})
}

// repliesToPerson reports whether target replies to someone other than
// its sender and the bot. A message the listener has seen came from a
// person, since the bot's own are never delivered; any other is
// fetched, and one that cannot be read counts as a person's.
func (k *chime) repliesToPerson(target chimeMsg, room *chimeRoom) bool {
	to := target.ev.ReplyTo
	if to.ID == "" {
		return false
	}
	k.mu.Lock()
	sender, seen := room.senders[to.ID]
	k.mu.Unlock()
	if seen {
		return sender != target.ev.Sender.ID
	}
	conn, err := k.b.Conn(target.ev.Channel.Platform)
	if err != nil {
		return true
	}
	m, err := conn.Fetch(target.ctx, target.ev.Channel, to)
	if err != nil {
		k.b.cfg.Logger.Info("chime could not read a replied message", "event", target.ev.ID, "error_kind", errorKind(err))
		return true
	}
	return !m.FromBot && m.Sender.ID != target.ev.Sender.ID
}

// sentBy records a reply Handler sent, to count a miss on it. A
// message id is unique on its platform, and a reply in a thread may be
// in another channel, so the platform and the id name it.
func (k *chime) sentBy(p Platform, ref MessageRef) {
	if ref.ID == "" {
		return
	}
	id := string(p) + "/" + ref.ID
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.sent[id] {
		return
	}
	k.sent[id] = true
	k.order = append(k.order, id)
	if len(k.order) > chimeSentMemory {
		delete(k.sent, k.order[0])
		k.order = k.order[1:]
	}
}

// reaction counts a miss: a reaction among Misses on a reply Handler
// sent, once for each reply.
func (k *chime) reaction(ev ReactionEvent) {
	if !slices.Contains(k.o.Misses, ev.Emoji) {
		return
	}
	id := string(ev.Channel.Platform) + "/" + ev.Target.ID
	k.mu.Lock()
	hit := k.sent[id]
	delete(k.sent, id)
	k.mu.Unlock()
	if hit {
		k.b.metrics.Add("airut_chime_misses_total", 1, "persona", k.o.Persona)
		k.b.cfg.Logger.Debug("chime miss", "event", ev.ID, "persona", k.o.Persona)
	}
}
