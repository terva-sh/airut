package airut

import "time"

// SetGateClock replaces the clock the bot's rate limit reads. Call it
// before Run.
func SetGateClock(b *Bot, now func() time.Time) { b.gate.rate.now = now }
