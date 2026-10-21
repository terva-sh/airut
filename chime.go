package airut

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	// MaxAge is how old a message may be, by MessageEvent.Sent, and still
	// be judged: when it arrives, so a bot catching up after downtime does
	// not answer what it missed, and when a deferred batch is judged. It
	// defaults to two minutes.
	MaxAge time.Duration
	// MaxPerHour caps the interruptions in one cold channel in any hour,
	// and must be positive. Cooldown is the gap after an interruption
	// before the next can be judged. Messages that arrive while a channel
	// is at its cap or in its gap wait, and are judged together when it
	// ends, each still within MaxAge. Neither applies to an engaged
	// channel.
	MaxPerHour int
	Cooldown   time.Duration
	// EngagedFor is how long a channel stays engaged after the bot speaks
	// in it, restarted by each message the bot sends there through a Ctx:
	// a Chime answer, a mention turn, a command reply. What an app sends
	// through a Conn directly does not engage it, and neither does people
	// talking among themselves. Zero leaves every channel cold.
	//
	// An engaged channel is judged with FollowUpPersona and FollowUpPrompt,
	// without Cooldown or MaxPerHour, and a turn Handler starts there is a
	// follow-up: it is charged to the sender, as a mention is, and needs
	// the Gate access a mention would. They default to Persona and Prompt.
	EngagedFor      time.Duration
	FollowUpPersona string
	FollowUpPrompt  string
	// BotQuote is how many bytes of each of the bot's own messages the
	// judge reads, marked FromBot. It defaults to 300, and is at most
	// MaxQuote.
	BotQuote int
	// ContextAge and ContextBytes bound the context besides
	// ContextMessages. A window whose oldest message is older than
	// ContextAge drops every message older than half of it, and one past
	// ContextBytes of text drops its oldest to half of that, so, as with
	// ContextMessages, the context's opening changes only on a drop. Zero
	// bounds nothing.
	ContextAge   time.Duration
	ContextBytes int
	// ContextTokens caps each judgement's whole request below the judge's
	// own cap, when the Judge is a JudgeLimit; Judgement.InputTokens
	// carries it. Zero leaves the judge's.
	ContextTokens int
	// StayOut keeps the bot out of exchanges between other people: a
	// message that replies to someone other than its sender and the bot
	// is never answered.
	StayOut bool
	// Misses are the reactions that mark a reply Handler sent as a miss,
	// such as "👎". airut_chime_misses_total counts each message marked
	// once.
	Misses []string
	// Settings, when set, declares the listener's limits as settings under
	// it, as PREFIX.cooldown, PREFIX.max_per_hour, PREFIX.engaged_for, and
	// PREFIX.max_age, with these options as their defaults. MaxPerHour is
	// also max_per_hour's ceiling, so chat can lower it and never raise it.
	// With a JudgeLimit that has a cap, PREFIX.context_tokens is declared
	// too, with that cap as its ceiling. The listener reads them at use
	// time.
	Settings string
	// OnPicked runs when the judge says yes and the moment has not
	// passed, just before Handler, with Handler's Ctx. OnLimited runs when
	// a message in an engaged channel cannot be answered because a limit
	// stopped it: Config.JudgeRate, or the sender's PerUser, which Chime
	// asks the Gate about before Handler runs. A cold channel calls
	// neither on a limit. Both run on the channel's worker, so they must
	// return quickly; an error or a panic is logged by kind.
	OnPicked  func(c *Ctx, target MessageEvent) error
	OnLimited func(c *Ctx, ev MessageEvent) error
	// Handler runs on a yes. Its Ctx stands on the message the judge
	// chose, so Reply answers it, and its caller is that message's
	// sender. In a cold channel an agent turn it starts is an interruption,
	// charged to the channel and never to the sender: it needs the channel
	// in the Gate's Access.Channels for the platform, and takes one of the
	// Gate's PerChannel turns but none of PerUser. In an engaged channel it
	// is a follow-up, charged to the sender's PerUser and never to
	// PerChannel. A failure is logged by kind and never answered.
	Handler func(*Ctx, Chimed) error
}

// Chimed is what a Chime handler is given.
type Chimed struct {
	// Target is the message the judge chose to answer.
	Target MessageEvent
	// Reason is the judge's own words. It is model text drawn from chat,
	// so a handler may show it but must never log it or follow it.
	Reason string
	// FollowUp reports that the channel was engaged, so the answer
	// continues a conversation with the bot rather than interrupting one.
	FollowUp bool
}

// ChimeState is a Chime listener's state in one channel, for a status
// line.
type ChimeState struct {
	// EngagedUntil is when the channel goes cold. A time that has passed
	// means it is cold.
	EngagedUntil time.Time
	// QuietUntil is when the gap after the last interruption ends.
	QuietUntil time.Time
	// InterruptionsLeft is how many more interruptions MaxPerHour allows
	// in the hour that ends now.
	InterruptionsLeft int
	// Pending is how many messages wait to be judged.
	Pending int
}

// The defaults of ChimeOptions.
const (
	defaultChimeContext  = 20
	defaultChimeSettle   = 3 * time.Second
	defaultChimeMaxWait  = 15 * time.Second
	defaultChimeMaxAge   = 2 * time.Minute
	defaultChimeBotQuote = 300
	// chimeSentMemory is how many of Handler's replies a listener
	// remembers, to count a miss on one.
	chimeSentMemory = 256
	// chimeBot marks a message of the bot's own among a room's senders.
	chimeBot = "\x00bot"
)

// Chime registers a listener that speaks up unprompted. In each of its
// channels it gathers the messages that arrive until the channel has
// been quiet for Settle, or MaxWait has passed, and asks the judge
// about them together, one judgement at a time per channel. Messages
// that arrive meanwhile wait for the next, and none is dropped unjudged
// except by MaxAge. The judge reads the channel's earlier messages as
// context, among them the bot's own that it sent through a Ctx.
//
// A channel is cold until the bot speaks in it, and engaged for
// EngagedFor after. A cold channel's answers are interruptions, under
// MaxPerHour and Cooldown; an engaged channel's are follow-ups, under
// the sender's own limits.
//
// A yes runs Handler, unless the moment has passed. Chime drops a yes
// when a message that arrived after the batch closed replies to the
// target, or comes from someone else and replies to nothing, since
// either may already be the answer; and when StayOut is set and the
// target replies to another person.
//
// Chime counts airut_chime_total by persona, outcome, and whether the
// channel was engaged, and logs ids and outcomes: never the chat, the
// prompt, or the reason. It panics on options that cannot work, on a bot
// with no Judge, on a channel another listener has, and once the bot is
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
	case o.BotQuote < 0 || o.BotQuote > MaxQuote:
		panic(fmt.Sprintf("airut: Chime's BotQuote must be between 0 and %d", MaxQuote))
	case o.ContextBytes < 0 || o.ContextTokens < 0:
		panic("airut: Chime's ContextBytes and ContextTokens cannot be negative")
	case o.Settle < 0 || o.MaxWait < 0 || o.MaxAge < 0 || o.Cooldown < 0 || o.EngagedFor < 0 || o.ContextAge < 0:
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
	if o.BotQuote == 0 {
		o.BotQuote = defaultChimeBotQuote
	}
	if o.FollowUpPersona == "" {
		o.FollowUpPersona = o.Persona
	}
	if o.FollowUpPrompt == "" {
		o.FollowUpPrompt = o.Prompt
	}
	k := &chime{
		b: b, o: o, in: map[string]bool{}, rooms: map[string]*chimeRoom{}, sent: map[string]bool{},
		spoken: &limiter{now: time.Now, seen: map[string][]time.Time{}}, now: time.Now,
	}
	if b.chimes == nil {
		b.chimes = map[string]*chime{}
	}
	for _, ch := range o.Channels {
		key := roomKey(ch)
		if b.chimes[key] != nil {
			panic("airut: two Chime listeners share channel " + ch.ID)
		}
		k.in[key] = true
		b.chimes[key] = k
	}
	if o.Settings != "" {
		k.knobs = k.declare(o.Settings)
	}
	b.on("Chime", func(c *Ctx, ev Event) error { k.message(c, ev.(MessageEvent)); return nil }, &b.onMessage)
	if len(o.Misses) > 0 {
		b.on("Chime", func(c *Ctx, ev Event) error { k.reaction(ev.(ReactionEvent)); return nil }, &b.onReaction)
	}
}

// ChimeState reports the state of the Chime listener in ch, and false
// when no listener has the channel.
func (b *Bot) ChimeState(ch Channel) (ChimeState, bool) {
	k := b.chimes[roomKey(ch)]
	if k == nil {
		return ChimeState{}, false
	}
	key := roomKey(ch)
	st := ChimeState{InterruptionsLeft: k.spoken.left(key, k.maxPerHour(), time.Hour)}
	k.mu.Lock()
	defer k.mu.Unlock()
	if room := k.rooms[key]; room != nil {
		st.EngagedUntil, st.QuietUntil, st.Pending = room.engaged, room.quiet, len(room.pending)
	}
	return st, true
}

// chime is one Chime listener.
type chime struct {
	b      *Bot
	o      ChimeOptions
	in     map[string]bool // the channels, by roomKey
	spoken *limiter        // interruptions, under MaxPerHour
	knobs  *chimeSettings  // the limits as settings, when Settings is set
	now    func() time.Time

	mu    sync.Mutex
	rooms map[string]*chimeRoom
	// sent remembers the replies Handler sent, by platform and id, the
	// oldest first in order.
	sent  map[string]bool
	order []string
}

// chimeSettings are a listener's limits as settings.
type chimeSettings struct {
	cooldown, engagedFor, maxAge *Setting[time.Duration]
	maxPerHour, contextTokens    *Setting[int]
}

// declare declares the listener's limits as settings under prefix.
func (k *chime) declare(prefix string) *chimeSettings {
	o := k.o
	var tokens *Setting[int]
	if jl, ok := k.b.cfg.Judge.(JudgeLimit); ok && jl.InputTokens() > 0 {
		ceiling := jl.InputTokens()
		def := ceiling
		if o.ContextTokens > 0 {
			def = min(o.ContextTokens, ceiling)
		}
		tokens = k.b.DeclareInt(IntSetting{Name: prefix + ".context_tokens",
			Description: "The most tokens one judgement's request may take, below the judge's own cap",
			Default:     def, Min: 1, Max: ceiling})
	}
	return &chimeSettings{contextTokens: tokens,
		cooldown: k.b.DeclareDuration(DurationSetting{Name: prefix + ".cooldown",
			Description: "The gap after the bot interrupts a conversation before it may again",
			Default:     o.Cooldown, Max: max(o.Cooldown, 24*time.Hour)}),
		maxPerHour: k.b.DeclareInt(IntSetting{Name: prefix + ".max_per_hour",
			Description: "How often the bot may interrupt a conversation in one channel in any hour",
			Default:     o.MaxPerHour, Min: 1, Max: o.MaxPerHour}),
		engagedFor: k.b.DeclareDuration(DurationSetting{Name: prefix + ".engaged_for",
			Description: "How long a channel the bot spoke in stays engaged, so it answers follow-ups",
			Default:     o.EngagedFor, Max: max(o.EngagedFor, time.Hour)}),
		maxAge: k.b.DeclareDuration(DurationSetting{Name: prefix + ".max_age",
			Description: "How old a message may be and still be judged",
			Default:     o.MaxAge, Min: time.Second, Max: max(o.MaxAge, time.Hour)}),
	}
}

func (k *chime) cooldown() time.Duration {
	if k.knobs != nil {
		return k.knobs.cooldown.Get()
	}
	return k.o.Cooldown
}

func (k *chime) maxPerHour() int {
	if k.knobs != nil {
		return k.knobs.maxPerHour.Get()
	}
	return k.o.MaxPerHour
}

func (k *chime) engagedFor() time.Duration {
	if k.knobs != nil {
		return k.knobs.engagedFor.Get()
	}
	return k.o.EngagedFor
}

func (k *chime) contextTokens() int {
	if k.knobs != nil && k.knobs.contextTokens != nil {
		return k.knobs.contextTokens.Get()
	}
	return k.o.ContextTokens
}

func (k *chime) maxAge() time.Duration {
	if k.knobs != nil {
		return k.knobs.maxAge.Get()
	}
	return k.o.MaxAge
}

// chimeRoom is the listener's state in one channel.
type chimeRoom struct {
	// window is the context the judge reads: the channel's messages
	// before the batch, oldest first.
	window []Quoted
	// senders are the senders of the messages in window and pending, by
	// message id, to tell a reply to a person without a fetch. The bot's
	// own are chimeBot.
	senders map[string]string
	pending []chimeMsg
	// opened and last are when the first and the last pending message
	// arrived.
	opened, last time.Time
	// busy is set while a worker runs for the room, and wake tells it
	// that something changed while it waits.
	busy bool
	wake chan struct{}
	// quiet is when the gap after the last interruption ends, and engaged
	// when the room goes cold.
	quiet, engaged time.Time
	// deferred is set once the pending batch has been held for a limit.
	deferred bool
}

// chimeMsg is a message waiting to be judged.
type chimeMsg struct {
	ctx context.Context
	ev  MessageEvent
	r   Responder
	// judge is false for a message the judge reads but may not choose,
	// such as one that addresses the bot or one the bot sent.
	judge bool
}

func roomKey(ch Channel) string { return string(ch.Platform) + "/channel/" + ch.ID }

// room returns the room for key, made if new. k.mu must be held.
func (k *chime) room(key string) *chimeRoom {
	room := k.rooms[key]
	if room == nil {
		room = &chimeRoom{senders: map[string]string{}, wake: make(chan struct{}, 1)}
		k.rooms[key] = room
	}
	return room
}

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
	if !ev.Sent.IsZero() && now.Sub(ev.Sent) > k.maxAge() {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	room := k.room(key)
	m := chimeMsg{ctx: c.ctx, ev: ev, r: c.resp, judge: !ev.Mentioned}
	if k.o.StayOut && m.judge && ev.ReplyTo.ID != "" {
		if s, ok := room.senders[ev.ReplyTo.ID]; ok && s != ev.Sender.ID && s != chimeBot {
			m.judge = false
		}
	}
	room.senders[ev.Message.ID] = ev.Sender.ID
	k.queue(room, m, now)
}

// queue adds m to room's pending messages and makes sure a worker runs.
// k.mu must be held.
func (k *chime) queue(room *chimeRoom, m chimeMsg, now time.Time) {
	if len(room.pending) == 0 {
		room.opened = now
	}
	room.pending = append(room.pending, m)
	room.last = now
	// A room deferred for a long gap could collect without bound, so what
	// falls off the front becomes context, unjudged.
	if over := len(room.pending) - MaxRoomMessages; over > 0 {
		room.remember(room.pending[:over], k.o, now)
		room.pending = slices.Clone(room.pending[over:])
	}
	if !room.busy {
		room.busy = true
		// The delivery or the send that called queue holds the bot's
		// inflight count, so this Add cannot race Run's Wait.
		k.b.inflight.Add(1)
		go k.work(room)
		return
	}
	room.poke()
}

// poke tells a waiting worker that something changed, such as the room
// becoming engaged, so it looks again at what it holds.
func (room *chimeRoom) poke() {
	select {
	case room.wake <- struct{}{}:
	default:
	}
}

// said records a message the bot sent in a channel of the listener,
// through a Ctx: it engages the room, and the judge reads the message
// as the bot's.
func (k *chime) said(c *Ctx, ref MessageRef, text string) {
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	room := k.room(roomKey(c.inv.Channel))
	if d := k.engagedFor(); d > 0 {
		room.engaged = now.Add(d)
		// A held batch need wait no longer, whether or not this send has
		// text to add.
		room.poke()
	}
	// A message with no text never joins the window, so nothing would
	// ever forget its sender. A reply to it is fetched, and found the
	// bot's, instead.
	if strings.TrimSpace(text) == "" {
		return
	}
	if ref.ID != "" {
		room.senders[ref.ID] = chimeBot
	}
	ev := MessageEvent{Message: ref, Channel: c.inv.Channel, Text: text, Sent: now, FromBot: true}
	if len(room.pending) == 0 && !room.busy {
		room.remember([]chimeMsg{{ev: ev}}, k.o, now)
		return
	}
	k.queue(room, chimeMsg{ctx: c.ctx, ev: ev, r: c.resp}, now)
}

// edited replaces the text of a message the bot sent and the room
// still holds, as a streamed reply is edited while it grows.
func (k *chime) edited(c *Ctx, ref MessageRef, text string) {
	if ref.ID == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	room := k.rooms[roomKey(c.inv.Channel)]
	if room == nil {
		return
	}
	for i := range room.pending {
		if m := &room.pending[i]; m.ev.FromBot && m.ev.Message == ref {
			m.ev.Text = text
			return
		}
	}
	for i := range room.window {
		if q := &room.window[i]; q.FromBot && q.Ref == ref {
			q.Text = cutQuote(text, k.o.BotQuote)
			return
		}
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
		now := k.now()
		k.age(room, now)
		due := room.last.Add(k.o.Settle)
		if latest := room.opened.Add(k.o.MaxWait); latest.Before(due) {
			due = latest
		}
		if hold := k.held(room, now); hold.After(due) {
			due = hold
			if !room.deferred {
				room.deferred = true
				k.outcome(room.pending[len(room.pending)-1].ev, "deferred", false)
			}
			// Wake when the newest message would age out, so a long hold
			// does not keep what can no longer be judged.
			if expires := k.expires(room); !expires.IsZero() && expires.Before(due) {
				due = expires
			}
		}
		ctx := room.pending[len(room.pending)-1].ctx
		k.mu.Unlock()
		if wait := due.Sub(now); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-room.wake:
				t.Stop()
			case <-ctx.Done():
				t.Stop()
				k.mu.Lock()
				room.pending, room.busy = nil, false
				k.mu.Unlock()
				return
			}
			continue
		}
		k.round(room)
	}
}

// age marks the pending messages older than MaxAge as no longer to be
// judged, and counts aged_out once for those it marks. k.mu must be
// held.
func (k *chime) age(room *chimeRoom, now time.Time) {
	aged := false
	for i := range room.pending {
		if m := &room.pending[i]; m.judge && !m.ev.Sent.IsZero() && now.Sub(m.ev.Sent) > k.maxAge() {
			m.judge, aged = false, true
		}
	}
	if aged {
		k.outcome(room.pending[len(room.pending)-1].ev, "aged_out", now.Before(room.engaged))
	}
}

// expires is just after the newest pending message to be judged ages
// out, or zero when none will. k.mu must be held.
func (k *chime) expires(room *chimeRoom) time.Time {
	var at time.Time
	for _, m := range room.pending {
		if m.judge && !m.ev.Sent.IsZero() {
			at = m.ev.Sent.Add(k.maxAge() + time.Millisecond)
		}
	}
	return at
}

// held is when a cold room's limits let its pending messages be judged:
// the end of its gap, or of the hour that holds MaxPerHour
// interruptions. It is zero for an engaged room, and for a batch the
// judge could not choose from anyway. k.mu must be held.
func (k *chime) held(room *chimeRoom, now time.Time) time.Time {
	if now.Before(room.engaged) || !slices.ContainsFunc(room.pending, func(m chimeMsg) bool { return m.judge }) {
		return time.Time{}
	}
	until := room.quiet
	key := roomKey(room.pending[0].ev.Channel)
	if wait := k.spoken.wait(key, k.maxPerHour(), time.Hour); wait > 0 && now.Add(wait).After(until) {
		until = now.Add(wait)
	}
	return until
}

// round judges the pending messages as one batch, and runs Handler on a
// yes the moment has not passed.
func (k *chime) round(room *chimeRoom) {
	k.mu.Lock()
	now := k.now()
	batch := room.pending
	room.pending, room.deferred = nil, false
	engaged := now.Before(room.engaged)
	// What the bot said before anyone spoke again, such as Handler's
	// answer, is context for the batch rather than part of it. remember
	// also bounds the window, even when there is nothing to add, so one
	// an edit of the bot's grew is bounded before the judge reads it.
	lead := 0
	for lead < len(batch) && batch[lead].ev.FromBot {
		lead++
	}
	room.remember(batch[:lead], k.o, now)
	batch = batch[lead:]
	room.trimAge(now, k.o.ContextAge)
	quotes := make([]Quoted, len(batch))
	for i, m := range batch {
		quotes[i] = k.quote(m.ev)
	}
	persona, prompt := k.o.Persona, k.o.Prompt
	if engaged {
		persona, prompt = k.o.FollowUpPersona, k.o.FollowUpPrompt
	}
	j := Judgement{Persona: persona, Prompt: prompt, Batch: quotes, InputTokens: k.contextTokens()}
	i := slices.IndexFunc(batch, func(m chimeMsg) bool { return m.judge })
	if jl, ok := k.b.cfg.Judge.(JudgeLimit); ok && i >= 0 {
		for j.Context = room.window; len(room.window) > 0 && !jl.Fits(j); j.Context = room.window {
			room.drop((len(room.window) + 1) / 2)
		}
	}
	j.Context = slices.Clone(room.window)
	room.remember(batch, k.o, now)
	k.mu.Unlock()

	if i < 0 {
		return
	}
	// The judgement stands on the last message a person sent, since the
	// bot's own came through another Ctx.
	last := batch[i]
	for _, m := range batch[i:] {
		if !m.ev.FromBot {
			last = m
		}
	}
	inv, _ := eventInvocation(last.ev, last.r)
	c := &Ctx{ctx: last.ctx, bot: k.b, inv: inv, resp: last.r}
	v, err := c.Judge(j)
	if errors.Is(err, ErrJudgeRate) {
		k.outcome(last.ev, "limited", engaged)
		if engaged {
			k.hook("OnLimited", k.o.OnLimited, c, last.ev)
		}
		return
	}
	if v.Decision != Yes {
		k.outcome(last.ev, "declined", engaged)
		return
	}
	i = slices.IndexFunc(batch, func(m chimeMsg) bool { return m.ev.Message == v.Target })
	if i < 0 || !batch[i].judge {
		k.outcome(last.ev, "not_judged", engaged)
		return
	}
	target := batch[i]
	k.b.cfg.Logger.Debug("chime", "event", target.ev.ID, "persona", persona, "outcome", "picked", "engaged", engaged)
	if k.o.StayOut && k.repliesToPerson(target, room) {
		k.outcome(target.ev, "stayed_out", engaged)
		return
	}
	// After the StayOut check, which may wait on a fetch, so a message
	// that arrived meanwhile counts.
	k.mu.Lock()
	newer := slices.Clone(room.pending)
	k.mu.Unlock()
	if overtaken(target.ev, newer) {
		k.outcome(target.ev, "stale", engaged)
		return
	}
	inv, _ = eventInvocation(target.ev, target.r)
	tc := &Ctx{ctx: target.ctx, bot: k.b, inv: inv, resp: target.r, chime: k, followUp: engaged}
	if engaged && k.b.cfg.Agent != nil {
		switch k.b.gate.admits(tc) {
		case RefusedAccess:
			k.outcome(target.ev, "refused", engaged)
			return
		case RefusedRate:
			k.outcome(target.ev, "limited", engaged)
			k.hook("OnLimited", k.o.OnLimited, tc, target.ev)
			return
		}
	}
	if !engaged {
		// The cap was checked before judging, and only this room's
		// rounds, which run one at a time, count against it, so the room
		// is still under it.
		limit := Invocation{Caller: User{Platform: target.ev.Channel.Platform}, Channel: target.ev.Channel}
		_ = k.spoken.take(Gate{PerChannel: k.maxPerHour(), Window: time.Hour}, limit)
	}
	k.hook("OnPicked", k.o.OnPicked, tc, target.ev)
	k.speak(room, tc, Chimed{Target: target.ev, Reason: v.Reason, FollowUp: engaged})
}

// speak runs Handler on c's target. The room's worker runs it, so the
// room is not judged meanwhile; what arrives waits for the next round.
// An interruption then holds the room for Cooldown, which the bot's
// answer, by engaging the room, lifts while it lasts.
func (k *chime) speak(room *chimeRoom, c *Ctx, e Chimed) {
	if !e.FollowUp {
		defer func() {
			k.mu.Lock()
			room.quiet = k.now().Add(k.cooldown())
			k.mu.Unlock()
		}()
	}
	err := runChime(k.o.Handler, c, e)
	if err == nil {
		k.outcome(e.Target, "spoke", e.FollowUp)
		return
	}
	k.outcome(e.Target, "failed", e.FollowUp)
	k.logFailure("chime handler", e.Target, err)
}

// hook runs one of the listener's hooks, if set, and logs its failure.
func (k *chime) hook(name string, h func(*Ctx, MessageEvent) error, c *Ctx, ev MessageEvent) {
	if h == nil {
		return
	}
	err := runChime(func(c *Ctx, e Chimed) error { return h(c, e.Target) }, c, Chimed{Target: ev})
	if err != nil {
		k.logFailure("chime "+name, ev, err)
	}
}

func (k *chime) logFailure(what string, ev MessageEvent, err error) {
	var pe panicError
	if errors.As(err, &pe) {
		k.b.cfg.Logger.Error(what+" panicked", "event", ev.ID, "persona", k.o.Persona, "stack", string(pe.stack))
	} else if !errors.Is(err, context.Canceled) {
		k.b.cfg.Logger.Info(what+" failed", "event", ev.ID, "persona", k.o.Persona, "error_kind", errorKind(err))
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
func (k *chime) outcome(ev MessageEvent, o string, engaged bool) {
	e := strconv.FormatBool(engaged)
	k.b.metrics.Add("airut_chime_total", 1, "persona", k.o.Persona, "outcome", o, "engaged", e)
	k.b.cfg.Logger.Debug("chime", "event", ev.ID, "persona", k.o.Persona, "outcome", o, "engaged", engaged)
}

// quote quotes a message for the judge, the bot's own cut to BotQuote.
func (k *chime) quote(ev MessageEvent) Quoted {
	if ev.FromBot {
		q := Quote(ev)
		q.Text = cutQuote(q.Text, k.o.BotQuote)
		return q
	}
	return chimeQuote(ev)
}

// remember moves batch into the window. The window keeps its start
// while it grows to twice ContextMessages, or past ContextBytes, then
// keeps only its last ContextMessages, within half of ContextBytes, so
// successive judgements open with the same messages.
func (room *chimeRoom) remember(batch []chimeMsg, o ChimeOptions, now time.Time) {
	for _, m := range batch {
		q := chimeQuote(m.ev)
		if m.ev.FromBot {
			q = Quote(m.ev)
			q.Text = cutQuote(q.Text, o.BotQuote)
		}
		room.window = append(room.window, q)
	}
	n := o.ContextMessages
	if len(room.window) > 2*n {
		room.drop(len(room.window) - n)
	}
	room.trimBytes(o.ContextBytes)
}

// trimBytes drops the window's oldest messages to half of limit bytes
// of text, once it holds more than limit.
func (room *chimeRoom) trimBytes(limit int) {
	if limit <= 0 || windowBytes(room.window) <= limit {
		return
	}
	size, i := windowBytes(room.window), 0
	for ; i < len(room.window) && size > limit/2; i++ {
		size -= len(room.window[i].Text)
	}
	room.drop(i)
}

// trimAge drops the window's messages older than half of age, once its
// oldest is older than age.
func (room *chimeRoom) trimAge(now time.Time, age time.Duration) {
	if age <= 0 || len(room.window) == 0 || now.Sub(room.window[0].Sent) <= age {
		return
	}
	i := 0
	for i < len(room.window) && now.Sub(room.window[i].Sent) > age/2 {
		i++
	}
	room.drop(i)
}

// drop removes the window's first n messages.
func (room *chimeRoom) drop(n int) {
	for _, q := range room.window[:n] {
		delete(room.senders, q.Ref.ID)
	}
	room.window = slices.Clone(room.window[n:])
}

func windowBytes(w []Quoted) int {
	n := 0
	for _, q := range w {
		n += len(q.Text)
	}
	return n
}

// chimeQuote quotes a message for the judge, with its text cut to
// MaxQuote bytes.
func chimeQuote(ev MessageEvent) Quoted {
	q := Quote(ev)
	q.Text = cutQuote(q.Text, MaxQuote)
	return q
}

// cutQuote cuts text to n bytes, marking the cut.
func cutQuote(text string, n int) string {
	if len(text) <= n {
		return text
	}
	const mark = "…"
	return strings.ToValidUTF8(text[:max(n-len(mark), 0)], "") + mark
}

// overtaken reports whether a message that arrived after target's batch
// closed may already answer it: one that replies to it, or one from
// someone else, the bot among them, that replies to nothing.
func overtaken(target MessageEvent, newer []chimeMsg) bool {
	return slices.ContainsFunc(newer, func(m chimeMsg) bool {
		return m.ev.ReplyTo == target.Message ||
			m.ev.ReplyTo.ID == "" && m.ev.Sender.ID != target.Sender.ID
	})
}

// repliesToPerson reports whether target replies to someone other than
// its sender and the bot. A message the listener has seen is known;
// any other is fetched, and one that cannot be read counts as a
// person's.
func (k *chime) repliesToPerson(target chimeMsg, room *chimeRoom) bool {
	to := target.ev.ReplyTo
	if to.ID == "" {
		return false
	}
	k.mu.Lock()
	sender, seen := room.senders[to.ID]
	k.mu.Unlock()
	if seen {
		return sender != target.ev.Sender.ID && sender != chimeBot
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
