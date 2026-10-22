package airut

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Owner allows a caller who owns the bot on their platform: one whose
// user id Config.Owners lists for that platform, or one claimed on the
// bot's host with airut claim. It is the AccessCheck to give the
// built-ins that only owners should reach, as ProfileEditors:
// airut.Owner. None of them defaults to it, so listing owners never
// turns a command on by itself.
func Owner(c *Ctx) (bool, error) {
	return c.bot.owners.is(c.Context(), c.inv.Caller)
}

var _ AccessCheck = Owner

// IsOwner reports whether u owns the bot on u's platform: whether
// Config.Owners lists u's id for it, or u is claimed there with airut
// claim. It answers as Owner does for a caller, so code outside a
// command, such as a Matrix invite check, can ask it of any user. A
// claim is honoured within a minute of being made, and a claims read
// that fails keeps the claims last read, so an id they do not hold is
// refused. Treat an error as a refusal.
func (b *Bot) IsOwner(ctx context.Context, u User) (bool, error) {
	return b.owners.is(ctx, u)
}

// HasOwner reports whether the bot has an owner on platform p, from
// Config.Owners or a claim.
func (b *Bot) HasOwner(ctx context.Context, p Platform) (bool, error) {
	return b.owners.has(ctx, p)
}

// OwnerClaims lists the owners claimed on the bot's host with airut
// claim, which keeps them in the bot's session store.
// terva.sh/airut/agent's Agent is one. The bot reads claims from
// Config.Agent when it is one, and from nowhere else, because the
// command that records a claim reaches only the store a config file
// names. ClaimedOwners must be safe for concurrent use.
type OwnerClaims interface {
	// ClaimedOwners returns every claimed owner.
	ClaimedOwners(ctx context.Context) ([]Claim, error)
}

// Claim is an owner claimed with airut claim.
type Claim struct {
	Platform Platform
	ID       string
	// At is when the owner was claimed.
	At time.Time
}

// ByPlatform keys m by Platform, as a config file's table, such as
// [bot] owners, names its platforms: Owners: airut.ByPlatform(cfg.Bot.Owners).
func ByPlatform[T any](m map[string]T) map[Platform]T {
	if m == nil {
		return nil
	}
	out := make(map[Platform]T, len(m))
	for k, v := range m {
		out[Platform(k)] = v
	}
	return out
}

// claimLine tells someone how an unclaimed bot gets an owner.
const claimLine = "no owner: claim it with airut claim on the host"

// noOwnerWarning is logged at every start of a bot with no owner.
const noOwnerWarning = "no owner configured; set [bot] owners or claim the bot with airut claim"

// The sources an owner comes from, as status shows them.
const (
	ownerFromConfig = "config"
	ownerClaimed    = "claimed"
)

// claimRereadEvery is the least time between two reads of the claims.
// A claim made while the bot runs takes effect within it.
const claimRereadEvery = time.Minute

// ownerEntry is one owner and where the bot learned of them.
type ownerEntry struct {
	Platform Platform
	ID       string
	// Source is where the owner comes from: "config" for Config.Owners,
	// "claimed" for a claim.
	Source string
	// At is when the owner was recorded; zero for one from the config.
	At time.Time
}

// owners is where the bot's owners come from: Config.Owners, and the
// claims the Agent keeps when it is an OwnerClaims. Every check reads
// them through it.
//
// The claims are cached. A check the config does not answer reads them
// again when the last read is claimRereadEvery old, so a new claim is
// honoured, and a removed one dropped, within that time, and a busy bot
// reads them at most once a minute. A read that fails keeps the claims
// last read, and is logged once until a read succeeds again.
type owners struct {
	config map[Platform][]string
	claims OwnerClaims
	now    func() time.Time
	log    *slog.Logger

	mu sync.Mutex
	// reading is open while a read runs, and closed when it ends, so
	// concurrent checks share one read and a waiter can give up on it.
	reading chan struct{}
	// claimed holds the claims last read, by platform and then id.
	claimed map[Platform]map[string]time.Time
	// tried is whether a read has finished, and readAt when.
	tried   bool
	readAt  time.Time
	failing bool
}

// newOwners copies cfg. It panics on an empty id, which no caller has.
// claims may be nil, for config owners only.
func newOwners(cfg map[Platform][]string, claims OwnerClaims, log *slog.Logger) *owners {
	o := &owners{config: make(map[Platform][]string, len(cfg)), claims: claims, now: time.Now, log: log}
	for p, ids := range cfg {
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				panic(fmt.Sprintf("airut: Config.Owners[%q] lists an empty id", p))
			}
		}
		if len(ids) > 0 {
			o.config[p] = slices.Clone(ids)
		}
	}
	return o
}

// refresh reads the claims again when they are due. One read runs at a
// time. A check that cached reports a hit does not wait for a read
// another check started; any other waits for it until ctx ends, and then
// answers from the cache. A read the caller's context cut short does not
// count, so the next check reads again.
func (o *owners) refresh(ctx context.Context, cached func() bool) {
	if o.claims == nil {
		return
	}
	for {
		o.mu.Lock()
		if o.tried && o.now().Sub(o.readAt) < claimRereadEvery {
			o.mu.Unlock()
			return
		}
		wait := o.reading
		if wait == nil {
			break
		}
		o.mu.Unlock()
		if cached() {
			return
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return
		}
	}
	done := make(chan struct{})
	o.reading = done
	o.mu.Unlock()
	o.read(ctx, done)
}

// read reads the claims into the cache, and then closes done, even when
// ClaimedOwners panics, so no waiter is left waiting.
func (o *owners) read(ctx context.Context, done chan struct{}) {
	defer func() {
		o.mu.Lock()
		o.reading = nil
		close(done)
		o.mu.Unlock()
	}()
	list, err := o.claims.ClaimedOwners(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil && ctx.Err() != nil {
		return
	}
	o.tried, o.readAt = true, o.now()
	if err != nil {
		if !o.failing {
			o.failing = true
			o.log.Warn("owner claims unreadable; keeping the claims last read", "error_kind", errorKind(err))
		}
		return
	}
	if o.failing {
		o.failing = false
		o.log.Info("owner claims readable again")
	}
	claimed := map[Platform]map[string]time.Time{}
	for _, c := range list {
		if strings.TrimSpace(c.ID) == "" {
			continue
		}
		if claimed[c.Platform] == nil {
			claimed[c.Platform] = map[string]time.Time{}
		}
		claimed[c.Platform][c.ID] = c.At
	}
	o.claimed = claimed
}

func (o *owners) isClaimed(u User) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.claimed[u.Platform][u.ID]
	return ok
}

func (o *owners) hasClaim(p Platform) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.claimed[p]) > 0
}

// is reports whether u owns the bot on u's platform.
func (o *owners) is(ctx context.Context, u User) (bool, error) {
	if slices.Contains(o.config[u.Platform], u.ID) {
		return true, nil
	}
	o.refresh(ctx, func() bool { return o.isClaimed(u) })
	return o.isClaimed(u), nil
}

// has reports whether the bot has an owner on p.
func (o *owners) has(ctx context.Context, p Platform) (bool, error) {
	if len(o.config[p]) > 0 {
		return true, nil
	}
	o.refresh(ctx, func() bool { return o.hasClaim(p) })
	return o.hasClaim(p), nil
}

// all lists every owner, by platform and then id, an owner from the
// config before the same one claimed.
func (o *owners) all(ctx context.Context) ([]ownerEntry, error) {
	o.refresh(ctx, func() bool { return false })
	var out []ownerEntry
	for p := range o.config {
		for _, id := range o.config[p] {
			out = append(out, ownerEntry{Platform: p, ID: id, Source: ownerFromConfig})
		}
	}
	o.mu.Lock()
	for p, ids := range o.claimed {
		for id, at := range ids {
			out = append(out, ownerEntry{Platform: p, ID: id, Source: ownerClaimed, At: at})
		}
	}
	o.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Source == ownerFromConfig && out[j].Source != ownerFromConfig
	})
	return out, nil
}

// warnUnowned logs noOwnerWarning when none of platforms has an owner.
// It names the platforms and the key, never an id.
func (b *Bot) warnUnowned(ctx context.Context, platforms []Platform) {
	for _, p := range platforms {
		has, err := b.owners.has(ctx, p)
		if err != nil {
			b.cfg.Logger.Info("owner check failed", "platform", string(p), "error_kind", errorKind(err))
			continue
		}
		if has {
			return
		}
	}
	names := make([]string, len(platforms))
	for i, p := range platforms {
		names[i] = string(p)
	}
	sort.Strings(names)
	b.cfg.Logger.Warn(noOwnerWarning, "platforms", strings.Join(names, ","))
}

// answerUnclaimed tells the sender of a direct message how the bot gets
// an owner, when it has none on their platform, once per sender for
// each Run. A reply that fails is tried again on their next message.
func (b *Bot) answerUnclaimed(ctx context.Context, ev MessageEvent, c *Ctx) {
	if !ev.Direct || ev.FromBot {
		return
	}
	p := ev.Sender.Platform
	log := b.cfg.Logger.With("event", ev.ID, "platform", string(p))
	has, err := b.owners.has(ctx, p)
	if err != nil {
		log.Info("owner check failed", "error_kind", errorKind(err))
		return
	}
	if has {
		return
	}
	key := string(p) + "\x00" + ev.Sender.ID
	b.mu.Lock()
	told := b.claimTold[key]
	if !told {
		if b.claimTold == nil {
			b.claimTold = map[string]bool{}
		}
		b.claimTold[key] = true
	}
	b.mu.Unlock()
	if told {
		return
	}
	if _, err := c.Reply("This bot has " + claimLine + "."); err != nil {
		log.Info("claim line not sent", "error_kind", errorKind(err))
		b.mu.Lock()
		delete(b.claimTold, key)
		b.mu.Unlock()
		return
	}
	log.Info("told a direct message's sender the bot has no owner")
}

// statusCommand is the built-in status. It is not a tool: what it shows
// an owner names every owner, which a turn has no need to see.
func statusCommand(b *Bot) *Command {
	c, err := newCommand(b, "status", "Show the bot's version, its platforms, and whether it has an owner", func(c *Ctx) error {
		_, err := c.ReplyPrivate(b.status(c))
		return err
	})
	if err != nil {
		panic("airut: the built-in status command: " + err.Error())
	}
	return c
}

// status is the status command's text for the caller of c.
func (b *Bot) status(c *Ctx) string {
	ctx := c.Context()
	var s strings.Builder
	if v := strings.TrimSpace(b.cfg.Version); v != "" {
		fmt.Fprintf(&s, "Running %s.\n", v)
	}
	b.mu.Lock()
	platforms := slices.Collect(maps.Keys(b.conns))
	b.mu.Unlock()
	if !slices.Contains(platforms, c.Platform()) {
		platforms = append(platforms, c.Platform())
	}
	slices.Sort(platforms)
	s.WriteString("Platforms:")
	for _, p := range platforms {
		switch has, err := b.owners.has(ctx, p); {
		case err != nil:
			b.cfg.Logger.Info("owner check failed", "invocation", c.inv.ID, "platform", string(p), "error_kind", errorKind(err))
			fmt.Fprintf(&s, "\n- %s: owner unknown", p)
		case has:
			fmt.Fprintf(&s, "\n- %s: has an owner", p)
		default:
			fmt.Fprintf(&s, "\n- %s: %s", p, claimLine)
		}
	}
	owner, err := Owner(c)
	if err != nil {
		b.cfg.Logger.Info("owner check failed", "invocation", c.inv.ID, "error_kind", errorKind(err))
	}
	if !owner || err != nil {
		return s.String()
	}
	all, err := b.owners.all(ctx)
	if err != nil {
		b.cfg.Logger.Info("owner list failed", "invocation", c.inv.ID, "error_kind", errorKind(err))
		return s.String()
	}
	s.WriteString("\nOwners:")
	for _, o := range all {
		fmt.Fprintf(&s, "\n- %s %s (%s", o.Platform, o.ID, o.Source)
		if !o.At.IsZero() {
			fmt.Fprintf(&s, ", %s", o.At.UTC().Format(time.RFC3339))
		}
		s.WriteString(")")
	}
	return s.String()
}

// claimsOf is a's claims, or nil when a keeps none.
func claimsOf(a Agent) OwnerClaims {
	c, _ := a.(OwnerClaims)
	return c
}
