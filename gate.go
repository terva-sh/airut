package airut

import (
	"fmt"
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
	Access map[Platform]Access
	// PerUser and PerChannel cap the turns one caller, and one channel,
	// may start in any Window. Zero means no cap of that kind. Turns are
	// counted in memory, so a restart forgets them.
	PerUser    int
	PerChannel int
	Window     time.Duration
	// Budget fills each zero field of a Turn's budget. The Agent stops a
	// turn at the budget it receives.
	Budget Budget
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
	msg    string
}

// The refusal reasons.
const (
	RefusedAccess = "access"
	RefusedRate   = "rate"
)

func (r *Refusal) Error() string { return r.msg }

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
func (g *gate) check(c *Ctx) error {
	if ok, _ := g.cfg.Access[c.inv.Caller.Platform].allows(c); !ok {
		return errRefusedAccess()
	}
	return g.rate.take(g.cfg, c.inv)
}

// budget fills the zero fields of b from the gate's default.
func (g *gate) budget(b Budget) Budget {
	if b.Turns == 0 {
		b.Turns = g.cfg.Budget.Turns
	}
	if b.Tokens == 0 {
		b.Tokens = g.cfg.Budget.Tokens
	}
	return b
}

// gate is a Bot's running Gate.
type gate struct {
	cfg  Gate
	rate limiter
}

func newGate(cfg Gate) *gate {
	if (cfg.PerUser > 0 || cfg.PerChannel > 0) && cfg.Window <= 0 {
		panic("airut: Gate.Window must be positive when PerUser or PerChannel is set")
	}
	if cfg.PerUser < 0 || cfg.PerChannel < 0 || cfg.Budget.Turns < 0 || cfg.Budget.Tokens < 0 {
		panic("airut: Gate limits cannot be negative")
	}
	return &gate{cfg: cfg, rate: limiter{now: time.Now, seen: map[string][]time.Time{}}}
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
	if cfg.PerUser <= 0 && cfg.PerChannel <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now, cfg.Window)
	p := string(inv.Caller.Platform)
	user, channel := p+"/user/"+inv.Caller.ID, p+"/channel/"+inv.Channel.ID
	// When both are full, the one that stays full longer is named, so the
	// wait the caller is given is one after which the turn can start.
	uwait, ufull := l.full(user, cfg.PerUser, now, cfg.Window)
	cwait, cfull := l.full(channel, cfg.PerChannel, now, cfg.Window)
	switch {
	case ufull && (!cfull || uwait >= cwait):
		return &Refusal{Reason: RefusedRate, msg: "You have started too many agent turns. Try again in " + after(uwait) + "."}
	case cfull:
		return &Refusal{Reason: RefusedRate, msg: "Too many agent turns have started in this channel. Try again in " + after(cwait) + "."}
	}
	if cfg.PerUser > 0 {
		l.seen[user] = append(l.seen[user], now)
	}
	if cfg.PerChannel > 0 {
		l.seen[channel] = append(l.seen[channel], now)
	}
	return nil
}

// full drops key's starts that left the window and reports whether
// max remain, with how long until the oldest leaves.
func (l *limiter) full(key string, max int, now time.Time, window time.Duration) (time.Duration, bool) {
	if max <= 0 {
		return 0, false
	}
	ts := l.seen[key]
	i := 0
	for i < len(ts) && !ts[i].After(now.Add(-window)) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.seen, key)
		return 0, false
	}
	l.seen[key] = ts
	if len(ts) < max {
		return 0, false
	}
	return ts[0].Add(window).Sub(now), true
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
