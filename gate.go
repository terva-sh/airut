package airut

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
)

// Gate guards agent turns: who may start one, how often, and what a turn
// that sets no budget may spend. The core checks it before a turn reaches
// the Agent, so it holds on every platform. The zero Gate refuses every
// turn, because an agent command is a spend vector that anyone who can
// reach the command can trigger.
type Gate struct {
	// Access lists who may start an agent turn, by platform. A caller is
	// allowed when their user id, or the id of one of their roles, is
	// listed for their platform, or when they are a member of a listed
	// channel. A platform with no entry, or an empty one, allows nobody.
	// A turn a Chime handler starts is the channel's, and is allowed when
	// the channel itself is listed.
	Access map[Platform]Access
	// PerUser and PerChannel cap the turns one caller, and one channel,
	// may start in any Window. Zero means no cap of that kind. Turns are
	// counted in memory, so a restart forgets them. A Chime turn counts
	// against PerChannel only.
	PerUser    int
	PerChannel int
	Window     time.Duration
	// Owners, if set, is the rate of a caller Owner admits: their turns
	// are checked against its caps in place of PerUser and PerChannel, in
	// the same Window. They are still counted in the caller's and the
	// channel's shared counts, so members see the room's real usage and
	// are refused once it is full. nil leaves owners at the members'
	// rate, and the gate then never asks Owner. A Chime interruption is
	// the channel's, and never takes it; a follow-up takes its PerUser.
	// An owner check that fails is logged by kind and the turn takes the
	// members' rate.
	Owners *GroupRate
	// Wait lets a turn the rate refuses wait for its slot instead of
	// failing. The turn reacts with an hourglass on the message that
	// started it, or replies once with the wait when it cannot react,
	// then starts as soon as the gate admits it and removes the
	// reaction. It waits at most one Window from its first refusal, and
	// past that fails with the refusal it would have had. It ends with
	// the invocation's context. Each caller may have one turn waiting; a
	// second the rate refuses is refused at once. A turn the access list
	// refuses never waits, nor does one a Chime handler starts, which
	// nobody asked for. Off, a refused turn fails at once.
	Wait bool
	// Budget fills each zero field of a Turn's budget. The Agent stops a
	// turn at the budget it receives.
	Budget Budget
}

// GroupRate is the rate of a group of callers with caps of their own,
// such as Gate.Owners. PerUser and PerChannel take the Gate's place for
// the group's turns, counted in the Gate's Window. Zero means no cap of
// that kind.
type GroupRate struct {
	PerUser    int
	PerChannel int
}

// Access lists platform user and role ids, and the ids of channels
// whose members are allowed.
type Access struct {
	Users []string
	Roles []string
	// Channels admit their members, as MemberOf does. The user and role
	// lists are checked first, so membership is read only for a caller
	// neither admits.
	Channels []string
}

// Refusal is the error an agent turn the Gate refused returns. Its text
// is meant for the caller, and the core answers with it in the chat.
type Refusal struct {
	// Reason is RefusedAccess or RefusedRate.
	Reason string
	// Wait is how long until the turn could start, as the message names
	// it, for a rate refusal; 0 for an access refusal.
	Wait time.Duration
	msg  string
	// retry, on a rate refusal, is the message before the wait, so that
	// the refusal can be worded for another wait.
	retry string
}

// The refusal reasons.
const (
	RefusedAccess = "access"
	RefusedRate   = "rate"
)

func (r *Refusal) Error() string { return r.msg }

// rateRefusal is a rate refusal whose message is retry followed by wait.
func rateRefusal(retry string, wait time.Duration) *Refusal {
	return &Refusal{Reason: RefusedRate, Wait: wait, msg: retry + after(wait) + ".", retry: retry}
}

// allows reports whether the caller of c is on the access list, or a
// member of one of its channels. A membership check that fails is logged
// by kind whatever the answer, so a channel whose list cannot be read
// shows even when another channel admits the caller. When no channel
// admits the caller, the first failure is returned, and refuses.
func (a Access) allows(c *Ctx) (bool, error) {
	inv := c.inv
	if slices.Contains(a.Users, inv.Caller.ID) ||
		slices.ContainsFunc(inv.Roles, func(r Role) bool { return slices.Contains(a.Roles, r.ID) }) {
		return true, nil
	}
	var first error
	for _, id := range a.Channels {
		ok, err := MemberOf(Channel{Platform: inv.Caller.Platform, ID: id})(c)
		if err != nil {
			c.bot.cfg.Logger.Info("channel membership check failed", "invocation", inv.ID, "channel", id, "error_kind", errorKind(err))
			if first == nil {
				first = err
			}
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, first
}

// errRefusedAccess is the refusal of a turn the access list refused.
func errRefusedAccess() *Refusal {
	return &Refusal{Reason: RefusedAccess, msg: "You are not allowed to start an agent turn on this bot."}
}

// check applies the access list and the rate limit to a turn about to
// start for c. A turn it allows is counted against both limits. A
// membership check that fails refuses the turn unless another channel
// admits the caller; allows logs it.
//
// A turn a Chime handler starts in a cold channel is the channel's,
// since nobody asked for it: the channel must be listed in
// Access.Channels, and the turn takes one of PerChannel's turns and none
// of the sender's. One it starts as a follow-up is the sender's, as a
// mention's is, and takes none of PerChannel's.
//
// A caller Owner admits is checked against Owners' caps, when the Gate
// sets them, and counted in the same shared counts as anyone else.
func (g *gate) check(c *Ctx) error {
	if c.followUp {
		if ok, _ := g.cfg.Access[c.inv.Caller.Platform].allows(c); !ok {
			return errRefusedAccess()
		}
		mine := g.limits().userOnly()
		return g.rate.charge(g.callerCaps(c, mine).userOnly(), mine, c.inv)
	}
	if c.chime != nil {
		ch := c.inv.Channel
		if !slices.Contains(g.cfg.Access[ch.Platform].Channels, ch.ID) {
			return &Refusal{Reason: RefusedAccess, msg: "This channel is not allowed to start an agent turn by itself."}
		}
		room := g.limits().channelOnly()
		return g.rate.charge(room, room, c.inv)
	}
	if ok, _ := g.cfg.Access[c.inv.Caller.Platform].allows(c); !ok {
		return errRefusedAccess()
	}
	lim := g.limits()
	return g.rate.charge(g.callerCaps(c, lim), lim, c.inv)
}

// userOnly returns g with no cap on channels, its Owners' included, for
// a turn that takes none of the channel's.
func (g Gate) userOnly() Gate {
	g.PerChannel = 0
	if g.Owners != nil {
		g.Owners = &GroupRate{PerUser: g.Owners.PerUser}
	}
	return g
}

// channelOnly returns g with no cap on callers, its Owners' included,
// for a turn that is the channel's.
func (g Gate) channelOnly() Gate {
	g.PerUser = 0
	if g.Owners != nil {
		g.Owners = &GroupRate{PerChannel: g.Owners.PerChannel}
	}
	return g
}

// callerCaps returns lim with the caps the caller of c is checked
// against: Owners' when lim sets them and Owner admits the caller,
// otherwise lim's own. An owner check that fails is logged by kind and
// leaves the members' caps, so a failure never lifts a cap.
func (g *gate) callerCaps(c *Ctx, lim Gate) Gate {
	if lim.Owners == nil {
		return lim
	}
	owner, err := g.isOwner(c)
	if err != nil {
		c.bot.cfg.Logger.Info("owner check failed", "invocation", c.inv.ID, "error_kind", errorKind(err))
		return lim
	}
	if owner {
		lim.PerUser, lim.PerChannel = lim.Owners.PerUser, lim.Owners.PerChannel
	}
	return lim
}

// admits reports why a follow-up turn for c would be refused now,
// without counting one: RefusedAccess, RefusedRate, or "" when it would
// start. An owner is held to Owners' PerUser, as check holds them.
func (g *gate) admits(c *Ctx) string {
	if ok, _ := g.cfg.Access[c.inv.Caller.Platform].allows(c); !ok {
		return RefusedAccess
	}
	caps := g.callerCaps(c, g.limits())
	user := string(c.inv.Caller.Platform) + "/user/" + c.inv.Caller.ID
	if g.rate.wait(user, caps.PerUser, caps.Window) > 0 {
		return RefusedRate
	}
	return ""
}

// budget fills the zero fields of b from the gate's default.
func (g *gate) budget(b Budget) Budget {
	def := g.limits().Budget
	if b.Turns == 0 {
		b.Turns = def.Turns
	}
	if b.Tokens == 0 {
		b.Tokens = def.Tokens
	}
	return b
}

// gate is a Bot's running Gate.
type gate struct {
	cfg  Gate
	rate limiter
	// knobs are the Gate's limits as settings, on a bot with an Agent.
	knobs *gateSettings
	// isOwner is Owner, which tests replace to make it fail.
	isOwner AccessCheck
	// waiters holds the caller keys with a turn waiting for its slot.
	waiters waiters
	// sleep waits d or until ctx ends; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// limits returns the Gate with the limits in force.
func (g *gate) limits() Gate {
	cfg := g.cfg
	if k := g.knobs; k != nil {
		cfg.PerUser, cfg.PerChannel, cfg.Window = k.perUser.Get(), k.perChannel.Get(), k.window.Get()
		cfg.Budget = Budget{Turns: k.turns.Get(), Tokens: k.tokens.Get()}
		if k.ownersPerUser != nil {
			cfg.Owners = &GroupRate{PerUser: k.ownersPerUser.Get(), PerChannel: k.ownersPerChannel.Get()}
		}
	}
	return cfg
}

// gateSettings are the Gate's rate and budget as settings. Config.Gate
// is their config layer and their ceiling: a chat override may tighten a
// limit, or loosen it back to the configured value, and no further, so
// that spend never exceeds what the deployed configuration allows. The
// access lists stay out of them, because a list editable from chat lets
// its editor grant themselves access.
type gateSettings struct {
	perUser, perChannel, turns, tokens *Setting[int]
	window                             *Setting[time.Duration]
	// ownersPerUser and ownersPerChannel are Gate.Owners' caps, declared
	// only when the Gate sets them.
	ownersPerUser, ownersPerChannel *Setting[int]
}

// ownersCapped reports whether either of the owners' caps is set.
func (k *gateSettings) ownersCapped() bool {
	return k.ownersPerUser != nil && (k.ownersPerUser.Get() > 0 || k.ownersPerChannel.Get() > 0)
}

// maxGateWindow bounds the agent.window setting.
const maxGateWindow = 7 * 24 * time.Hour

func (b *Bot) declareGateSettings(cfg Gate) *gateSettings {
	k := &gateSettings{}
	// fromConfig makes the configured value the setting's config layer.
	fromConfig := func(s *setting, v any) {
		s.gate = true
		val := &settingValue{v: v, layer: LayerConfig}
		s.lower.Store(val)
		s.cur.Store(val)
	}
	// limit declares a cap where 0 means none: an override of a positive
	// configured cap stays from 1 to it, and one of no cap may be any.
	limit := func(name, desc, none string, conf int, needsWindow bool) *Setting[int] {
		st := b.DeclareInt(IntSetting{Name: name, Description: desc, Max: math.MaxInt})
		s := st.s
		s.bounds = "a whole number, 0 for " + none
		if conf > 0 {
			s.bounds = fmt.Sprintf("a whole number from 1 to %d, the configured value", conf)
		}
		s.limit = func(v any) error {
			n := v.(int)
			if conf > 0 && (n < 1 || n > conf) {
				return Errorf("%s must be from 1 to %d: a setting may tighten the configured limit, not loosen it.", name, conf)
			}
			if needsWindow && n > 0 && k.window.Get() <= 0 {
				return Errorf("Set agent.window first: %s needs a window to count turns in.", name)
			}
			return nil
		}
		fromConfig(s, conf)
		return st
	}
	k.window = b.DeclareDuration(DurationSetting{Name: "agent.window",
		Description: "The window agent.per_user and agent.per_channel count turns in", Max: maxGateWindow})
	ws := k.window.s
	ws.bounds = fmt.Sprintf("a duration up to %s, positive while a cap is set", formatDuration(maxGateWindow))
	if cfg.Window > 0 {
		ws.bounds = fmt.Sprintf("a duration from %s, the configured value, to %s", formatDuration(cfg.Window), formatDuration(maxGateWindow))
	}
	ws.limit = func(v any) error {
		d := v.(time.Duration)
		switch {
		case d < cfg.Window:
			return Errorf("agent.window must be at least %s: a setting may tighten the configured limit, not loosen it.", formatDuration(cfg.Window))
		case d <= 0 && (k.perUser.Get() > 0 || k.perChannel.Get() > 0):
			return Errorf("agent.window must be positive while agent.per_user or agent.per_channel is set.")
		case d <= 0 && k.ownersCapped():
			return Errorf("agent.window must be positive while agent.for.owners.per_user or agent.for.owners.per_channel is set.")
		}
		return nil
	}
	fromConfig(ws, cfg.Window)
	k.perUser = limit("agent.per_user", "Agent turns one person may start in each window", "no cap", cfg.PerUser, true)
	k.perChannel = limit("agent.per_channel", "Agent turns one channel may start in each window", "no cap", cfg.PerChannel, true)
	if o := cfg.Owners; o != nil {
		k.ownersPerUser = limit("agent.for.owners.per_user", "Agent turns one owner may start in each window", "no cap", o.PerUser, true)
		k.ownersPerChannel = limit("agent.for.owners.per_channel",
			"Agent turns a channel may have started in each window when an owner starts one", "no cap", o.PerChannel, true)
	}
	k.turns = limit("agent.max_turns", "Model requests an agent turn may make when it sets no budget", "none", cfg.Budget.Turns, false)
	k.tokens = limit("agent.max_tokens", "Tokens an agent turn may spend when it sets no budget", "none", cfg.Budget.Tokens, false)
	return k
}

func newGate(cfg Gate) *gate {
	if (cfg.PerUser > 0 || cfg.PerChannel > 0) && cfg.Window <= 0 {
		panic("airut: Gate.Window must be positive when PerUser or PerChannel is set")
	}
	if cfg.PerUser < 0 || cfg.PerChannel < 0 || cfg.Budget.Turns < 0 || cfg.Budget.Tokens < 0 {
		panic("airut: Gate limits cannot be negative")
	}
	if o := cfg.Owners; o != nil {
		if (o.PerUser > 0 || o.PerChannel > 0) && cfg.Window <= 0 {
			panic("airut: Gate.Window must be positive when Owners sets a cap")
		}
		if o.PerUser < 0 || o.PerChannel < 0 {
			panic("airut: Gate limits cannot be negative")
		}
		// A copy, so the caller changing theirs changes nothing here.
		own := *o
		cfg.Owners = &own
	}
	return &gate{cfg: cfg, rate: limiter{now: time.Now, seen: map[string][]time.Time{}}, isOwner: Owner, sleep: sleep}
}

// limiter counts turn starts in a sliding window, per key.
type limiter struct {
	mu    sync.Mutex
	now   func() time.Time
	seen  map[string][]time.Time // start times inside the window, oldest first
	swept time.Time
}

// take counts a turn for inv's caller and channel, or refuses it when
// either is at its cap. A refused turn counts against neither.
func (l *limiter) take(cfg Gate, inv Invocation) error {
	return l.charge(cfg, cfg, inv)
}

// charge refuses a turn for inv when its caller or channel is at
// check's cap, and otherwise counts it. Which counts it is recorded in
// comes from the caps every group is configured with, count's own and
// count.Owners', whichever caps apply to this caller: it is counted for
// the caller when any group caps callers, and for the channel when any
// caps channels. So a member's turn shows in a count only an owners'
// cap reads, and an owner's in one only the members' cap reads. The two
// share check's Window. A refused turn counts against neither.
func (l *limiter) charge(check, count Gate, inv Invocation) error {
	countUser := check.PerUser > 0 || count.PerUser > 0 || count.Owners != nil && count.Owners.PerUser > 0
	countChannel := check.PerChannel > 0 || count.PerChannel > 0 || count.Owners != nil && count.Owners.PerChannel > 0
	if !countUser && !countChannel {
		return nil
	}
	window := check.Window
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now, window)
	p := string(inv.Caller.Platform)
	user, channel := p+"/user/"+inv.Caller.ID, p+"/channel/"+inv.Channel.ID
	// When both are full, the one that stays full longer is named, so the
	// wait the caller is given is one after which the turn can start.
	uwait, ufull := l.full(user, check.PerUser, now, window)
	cwait, cfull := l.full(channel, check.PerChannel, now, window)
	switch {
	case ufull && (!cfull || uwait >= cwait):
		return rateRefusal("You have started too many agent turns. Try again in ", uwait)
	case cfull:
		return rateRefusal("Too many agent turns have started in this channel. Try again in ", cwait)
	}
	if countUser {
		l.seen[user] = append(l.prune(user, now, window), now)
	}
	if countChannel {
		l.seen[channel] = append(l.prune(channel, now, window), now)
	}
	return nil
}

// atCap reports whether key has max starts inside window, without
// counting one.
func (l *limiter) atCap(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, full := l.full(key, max, l.now(), window)
	return full
}

// wait is how long until key has fewer than max starts inside window,
// or zero when it has already.
func (l *limiter) wait(key string, max int, window time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, _ := l.full(key, max, l.now(), window)
	return d
}

// left is how many more starts key may have inside window.
func (l *limiter) left(key string, max int, window time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.full(key, max, l.now(), window)
	// A cap lowered below what the window already holds leaves none.
	if n := max - len(l.seen[key]); n > 0 {
		return n
	}
	return 0
}

// full drops key's starts that left the window and reports whether
// max remain, with how long until enough leave that fewer than max do.
// That is not always the oldest: a group with a higher cap, as owners,
// can leave more than max in a count another group reads.
func (l *limiter) full(key string, max int, now time.Time, window time.Duration) (time.Duration, bool) {
	if max <= 0 {
		return 0, false
	}
	ts := l.prune(key, now, window)
	if len(ts) < max {
		return 0, false
	}
	return ts[len(ts)-max].Add(window).Sub(now), true
}

// prune drops key's starts that left the window and returns the rest.
// A turn counted under no cap of its own, as an uncapped owner's, is
// pruned when the next is counted, so its key never grows past one
// window's turns.
func (l *limiter) prune(key string, now time.Time, window time.Duration) []time.Time {
	ts := l.seen[key]
	i := 0
	for i < len(ts) && !ts[i].After(now.Add(-window)) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.seen, key)
		return nil
	}
	l.seen[key] = ts
	return ts
}

// sweep forgets keys whose every start has left the window, at most once
// a window, so callers who never return do not accumulate.
func (l *limiter) sweep(now time.Time, window time.Duration) {
	if now.Sub(l.swept) < window {
		return
	}
	l.swept = now
	for k, ts := range l.seen {
		if !ts[len(ts)-1].After(now.Add(-window)) {
			delete(l.seen, k)
		}
	}
}

// after renders a wait for the chat, rounded up to a whole second.
func after(d time.Duration) string {
	s := int((d + time.Second - 1) / time.Second)
	switch {
	case s < 60:
		return plural(s, "second")
	case s%60 == 0:
		return plural(s/60, "minute")
	}
	return plural(s/60, "minute") + " " + plural(s%60, "second")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
