package airut

import (
	"context"
	"time"
)

// SetGateClock replaces the clock the bot's rate limit reads. Call it
// before Run.
func SetGateClock(b *Bot, now func() time.Time) { b.gate.rate.now = now }

// SetOwnerClock replaces the clock the bot's owner claims are re-read
// by. Call it before Run.
func SetOwnerClock(b *Bot, now func() time.Time) { b.owners.now = now }

// SetGateOwnerCheck replaces the owner check the gate asks before it
// gives a caller Gate.Owners' caps. Call it before Run.
func SetGateOwnerCheck(b *Bot, check AccessCheck) { b.gate.isOwner = check }

// SetGateSleep replaces how a turn waiting for its rate slot sleeps.
// Call it before Run.
func SetGateSleep(b *Bot, sleep func(ctx context.Context, d time.Duration) error) {
	b.gate.sleep = sleep
}
