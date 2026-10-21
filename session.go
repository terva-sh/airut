package airut

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// SessionLinker is an Agent that remembers which session produced each
// of the bot's messages, and when each session was last active. When
// Config.Agent implements it:
//
//   - A reply to one of the bot's messages resumes the session that
//     produced it, whatever the turn's scope, when the person replying
//     may read it. Everyone in a channel may read the conversation the
//     channel or one of its threads shares. Only its own person may read
//     a person's conversation, and then only in its own channel or, for
//     a PerUser one, outside every direct channel. Otherwise the turn's
//     own scope decides, and the turn still sees what the reply replies
//     to as quoted chat.
//   - The Conversation scope picks a session as its doc says.
//   - StreamTo links the reply it posts, and TurnRun.Link links a message
//     the app posted itself.
//
// Links expire with their sessions, and forgetting a session forgets its
// links.
type SessionLinker interface {
	// LinkSession records that the session named key produced the bot's
	// message ref in ch.
	LinkSession(ctx context.Context, ref MessageRef, ch Channel, key string) error
	// LinkedSession returns the session that produced the bot's message
	// ref in ch, if a link is recorded and the session is still kept.
	LinkedSession(ctx context.Context, ref MessageRef, ch Channel) (key string, ok bool, err error)
	// FollowSession records that person spoke, now, in the session named
	// key. person is an id-only name for one person in one channel.
	FollowSession(ctx context.Context, person, key string) error
	// FollowedSession returns the session person last spoke in, as
	// FollowSession recorded it, if that was within within and the
	// session is still kept. A zero within takes the agent's default.
	FollowedSession(ctx context.Context, person string, within time.Duration) (key string, ok bool, err error)
}

// sessionKey resolves the session turn t remembers, and says which rule
// chose it when a SessionLinker took part: "reply", "thread", "follow",
// or "new".
func (c *Ctx) sessionKey(t Turn) (key, how string, err error) {
	base := c.SessionKey(t.Session)
	l, ok := c.bot.cfg.Agent.(SessionLinker)
	if base == "" || !ok {
		return base, "", nil
	}
	if ref := c.inv.ReplyTo; ref.ID != "" {
		linked, ok, err := l.LinkedSession(c.ctx, ref, c.inv.Channel)
		if err != nil {
			return "", "", err
		}
		if ok && c.mayResume(linked) {
			return linked, "reply", nil
		}
	}
	// A direct channel holds one conversation, whatever the scope.
	if t.Session != Conversation || c.inv.Direct {
		return base, "", nil
	}
	if c.inv.Thread != "" {
		return c.SessionKey(PerThread), "thread", nil
	}
	followed, ok, err := l.FollowedSession(c.ctx, base, t.History.FollowWithin)
	if err != nil {
		return "", "", err
	}
	if ok {
		return followed, "follow", nil
	}
	// Each of a person's own conversations in a channel is a generation
	// of their PerUserChannel key, named by when it began.
	return base + "/" + strconv.FormatInt(time.Now().UnixMilli(), 10), "new", nil
}

// follow records, after a Conversation turn outside a direct channel,
// that its caller spoke in the session named key, so that their next
// message in the channel can go on in it: their own conversation, a
// shared one they replied into, or a thread they spoke in.
func (c *Ctx) follow(t Turn, key string) {
	l, ok := c.bot.cfg.Agent.(SessionLinker)
	if !ok || t.Session != Conversation || c.inv.Direct || key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 10*time.Second)
	defer cancel()
	if err := l.FollowSession(ctx, c.SessionKey(PerUserChannel), key); err != nil {
		c.bot.cfg.Logger.Warn("could not record whom an agent turn followed", "invocation", c.inv.ID, "error_kind", errorKind(err))
	}
}

// mayResume reports whether the caller may resume the session named key
// from here, by the rule SessionLinker gives.
func (c *Ctx) mayResume(key string) bool {
	p, ch, me := string(c.inv.Caller.Platform), c.inv.Channel.ID, c.userKey()
	mine := me + "/channel/" + ch
	if key == mine || strings.HasPrefix(key, mine+"/") {
		return true
	}
	if c.inv.Direct {
		return false
	}
	return key == me || key == p+"/channel/"+ch || strings.HasPrefix(key, p+"/thread/"+ch+"/")
}
