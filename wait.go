package airut

import (
	"context"
	"errors"
	"sync"
	"time"
)

// waitEmoji is the reaction a turn waiting for its slot shows on the
// message that started it: an hourglass with flowing sand.
const waitEmoji = "⏳"

// unreactTimeout bounds removing the waiting reaction, which runs on a
// context apart from the invocation's so that a cancelled wait still
// takes its reaction down.
const unreactTimeout = 10 * time.Second

// enter applies the gate to a turn about to start for c, as check does.
// With Gate.Wait on, a turn the rate refuses waits for its slot, unless
// a Chime handler started it: an interruption is the channel's, and a
// follow-up runs in the room's worker, where a wait would hold up the
// whole room.
func (g *gate) enter(c *Ctx) error {
	err := g.check(c)
	var rf *Refusal
	if err == nil || !g.cfg.Wait || c.chime != nil || !errors.As(err, &rf) || rf.Reason != RefusedRate {
		return err
	}
	return g.await(c, rf)
}

// The outcomes of a wait, as airut_agent_turn_waits_total counts them.
const (
	waitStarted   = "started"
	waitExpired   = "expired"
	waitCancelled = "cancelled"
	waitRefused   = "refused"
)

// await waits for the slot of the turn rf refused, and returns nil once
// the gate has admitted and counted it. It works in absolute times on
// the gate's clock: the deadline is one Window, the one in force at the
// first refusal, after that refusal, and each retry is due when the wait
// its refusal named is over, however long telling the caller took. A
// retry due at the deadline itself may start; past it, the turn ends
// with a rate refusal and is never admitted, as it does when a refusal
// on a retry names a wait that would end past the deadline. It returns
// the refusal at once when the caller already has a turn waiting, and
// the context's error when the invocation ends first.
func (g *gate) await(c *Ctx, rf *Refusal) error {
	m, log := c.bot.metrics, c.bot.cfg.Logger
	outcome := func(o string) { m.Add("airut_agent_turn_waits_total", 1, "outcome", o) }
	key := c.userKey()
	if !g.waiters.hold(key) {
		outcome(waitRefused)
		log.Info("agent turn not waiting", "invocation", c.inv.ID, "reason", "already_waiting")
		return rf
	}
	defer g.waiters.release(key)
	now := g.rate.now
	first := now()
	deadline, retryAt := first.Add(g.limits().Window), first.Add(rf.Wait)
	log.Info("agent turn waiting", "invocation", c.inv.ID, "platform", string(c.inv.Caller.Platform), "wait", rf.Wait)
	undo := g.notifyWait(c, retryAt)
	defer undo()
	expired := func(rf *Refusal) error {
		outcome(waitExpired)
		log.Info("agent turn wait expired", "invocation", c.inv.ID, "waited", now().Sub(first), "wait", rf.Wait)
		return rf
	}
	for {
		err := c.ctx.Err()
		if d := retryAt.Sub(now()); d > 0 && err == nil {
			err = g.sleep(c.ctx, d)
		}
		if err != nil {
			outcome(waitCancelled)
			log.Info("agent turn wait cancelled", "invocation", c.inv.ID, "waited", now().Sub(first))
			return err
		}
		if t := now(); t.After(deadline) {
			// Woken late, past the deadline: the slot the refusal waited
			// for has freed, but the turn may not take it. The refusal is
			// worded for the time left until it was due, at least a
			// second, as after rounds up.
			return expired(rateRefusal(rf.retry, max(retryAt.Sub(t), time.Second)))
		}
		err = g.check(c)
		if err == nil {
			outcome(waitStarted)
			log.Info("agent turn wait over", "invocation", c.inv.ID, "waited", now().Sub(first))
			return nil
		}
		if !errors.As(err, &rf) || rf.Reason != RefusedRate {
			outcome(waitRefused)
			log.Info("agent turn not waiting", "invocation", c.inv.ID, "reason", "refused_"+refusalReason(err))
			return err
		}
		if retryAt = now().Add(rf.Wait); retryAt.After(deadline) {
			return expired(rf)
		}
	}
}

// refusalReason is err's refusal reason, or its kind when it is no
// refusal.
func refusalReason(err error) string {
	var rf *Refusal
	if errors.As(err, &rf) {
		return rf.Reason
	}
	return errorKind(err)
}

// notifyWait tells the caller of c that their turn is waiting until
// retryAt: an hourglass reaction on the message that started it, or,
// when there is no message to react to or the reaction fails, one reply
// naming the time left until retryAt as it is sent. When none is left,
// the retry is due at once and nothing is said. It returns what takes
// the reaction down again.
func (g *gate) notifyWait(c *Ctx, retryAt time.Time) func() {
	log := c.bot.cfg.Logger
	if conn, err := c.bot.Conn(c.Platform()); err == nil && c.inv.Message.ID != "" {
		ch := c.inv.Channel
		ref, err := conn.React(c.ctx, ch, c.inv.Message, waitEmoji)
		if err == nil {
			return func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), unreactTimeout)
				defer cancel()
				if err := conn.Unreact(ctx, ch, ref); err != nil {
					log.Info("waiting reaction not removed", "invocation", c.inv.ID, "error_kind", errorKind(err))
				}
			}
		}
		log.Info("waiting reaction failed", "invocation", c.inv.ID, "error_kind", errorKind(err))
	}
	left := retryAt.Sub(g.rate.now())
	if left <= 0 {
		return func() {}
	}
	if _, err := c.Reply("I'll get to this in " + after(left) + "."); err != nil {
		log.Info("waiting reply failed", "invocation", c.inv.ID, "error_kind", errorKind(err))
	}
	return func() {}
}

// waiters is the set of callers with a turn waiting for its slot.
type waiters struct {
	mu   sync.Mutex
	keys map[string]bool
}

// hold marks key as waiting, and reports false when it already was.
func (w *waiters) hold(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.keys[key] {
		return false
	}
	if w.keys == nil {
		w.keys = map[string]bool{}
	}
	w.keys[key] = true
	return true
}

// release ends key's wait.
func (w *waiters) release(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.keys, key)
}

// sleep waits d, or until ctx ends, when it returns ctx's error.
func sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
