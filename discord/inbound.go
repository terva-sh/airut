package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

// onMessage delivers a message that is not the bot's own to the bot's
// OnMessage handlers, whose replies answer it.
func (s *session) onMessage(_ *discordgo.Session, mc *discordgo.MessageCreate) {
	if mc.Message == nil || !s.delivers(mc.Message) {
		return
	}
	ev := s.messageEvent(mc.Message)
	r := s.channelResponder(mc.ChannelID, mc.GuildID, mc.Author.ID)
	r.target = mc.ID
	s.spawn(func() {
		// Off the gateway's goroutine: each link past the first is a
		// request.
		if ev.Mentioned {
			ev.ReplyChain = s.chain(s.ctx, mc.Message)
		}
		s.h.Deliver(s.ctx, ev, r)
	})
}

// chain resolves what m replies to, nearest first, to the session's
// depth and within airut's caps. The gateway carries the first link;
// each one past it is fetched, and one the bot cannot read ends the
// chain.
func (s *session) chain(ctx context.Context, m *discordgo.Message) []airut.Quoted {
	var out []airut.Quoted
	for room := true; room && len(out) < s.replyDepth; {
		ref := m.MessageReference
		if m.Type != discordgo.MessageTypeReply || ref == nil || ref.MessageID == "" {
			break
		}
		channel := ref.ChannelID
		if channel == "" {
			channel = m.ChannelID
		}
		next := m.ReferencedMessage
		if next == nil || next.ID != ref.MessageID {
			var err error
			if next, err = s.dg.ChannelMessage(channel, ref.MessageID, discordgo.WithContext(ctx)); err != nil {
				s.log.Info("a reply chain ended at a message the bot could not read", "message", ref.MessageID, "error_kind", errKind(err))
				break
			}
		}
		if next.Author == nil {
			break
		}
		if next.GuildID == "" {
			next.GuildID = s.guildOf(channel)
		}
		out, room = airut.AddQuote(out, airut.Quote(s.messageEvent(next)))
		m = next
	}
	return out
}

// delivers reports whether m is a message the bot's handlers see: a
// plain message or a reply, from anyone but the bot.
func (s *session) delivers(m *discordgo.Message) bool {
	if m.Author == nil || m.Author.ID == s.self() {
		return false
	}
	return m.Type == discordgo.MessageTypeDefault || m.Type == discordgo.MessageTypeReply
}

// messageEvent describes m to the core.
func (s *session) messageEvent(m *discordgo.Message) airut.MessageEvent {
	ev := airut.MessageEvent{
		ID:      m.ID,
		Message: airut.MessageRef{ID: m.ID},
		Sender:  user(m.Author, m.Member),
		Channel: s.channel(m.ChannelID),
		Direct:  s.direct(m.ChannelID, m.GuildID),
		Text:    m.Content,
		Sent:    m.Timestamp,
		FromBot: m.Author.ID == s.self(),
		Native:  m,
	}
	if ch, err := s.dg.State.Channel(m.ChannelID); err == nil && ch.IsThread() {
		ev.Thread = ch.ID
	}
	if m.Member != nil {
		for _, id := range m.Member.Roles {
			ev.Roles = append(ev.Roles, s.role(m.GuildID, id))
		}
	}
	me := s.self()
	for _, u := range m.Mentions {
		if u != nil && u.ID == me {
			ev.Mentioned = true
		}
	}
	// In a direct message everything addresses the bot, except what the
	// bot says itself.
	ev.Mentioned = (ev.Mentioned || ev.Direct) && !ev.FromBot
	if ev.Mentioned {
		ev.Request = request(m.Content, me)
	}
	for _, a := range m.Attachments {
		ev.Files = append(ev.Files, airut.Attachment{Name: a.Filename, ContentType: a.ContentType, Size: int64(a.Size), URL: a.URL})
	}
	if m.Type == discordgo.MessageTypeReply && m.MessageReference != nil {
		ev.ReplyTo = airut.MessageRef{ID: m.MessageReference.MessageID}
	}
	return ev
}

// request is text without an opening mention of the bot, or text itself
// when the mention was elsewhere.
func request(text, me string) string {
	for _, m := range []string{"<@" + me + ">", "<@!" + me + ">"} {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(text), m); ok {
			return strings.TrimSpace(rest)
		}
	}
	return text
}

// onReaction delivers a reaction that is not the bot's own to the bot's
// OnReaction handlers, whose replies answer the reacted message.
func (s *session) onReaction(_ *discordgo.Session, ra *discordgo.MessageReactionAdd) {
	if ra.MessageReaction == nil || ra.UserID == s.self() {
		return
	}
	emoji := emojiName(ra.Emoji)
	ev := airut.ReactionEvent{
		ID:      reactionID(ra.MessageID, ra.UserID, emoji),
		Sender:  airut.User{Platform: airut.Discord, ID: ra.UserID},
		Channel: s.channel(ra.ChannelID),
		Direct:  s.direct(ra.ChannelID, ra.GuildID),
		Target:  airut.MessageRef{ID: ra.MessageID},
		Emoji:   emoji,
		Native:  ra,
	}
	if ra.Member != nil {
		if ra.Member.User != nil {
			ev.Sender = user(ra.Member.User, ra.Member)
		}
		for _, id := range ra.Member.Roles {
			ev.Roles = append(ev.Roles, s.role(ra.GuildID, id))
		}
	}
	if ch, err := s.dg.State.Channel(ra.ChannelID); err == nil && ch.IsThread() {
		ev.Thread = ch.ID
	}
	r := s.channelResponder(ra.ChannelID, ra.GuildID, ra.UserID)
	r.target = ra.MessageID
	s.spawn(func() { s.h.Deliver(s.ctx, ev, r) })
}

// reactionID identifies one person's reaction with one emoji on one
// message. The emoji enters it as a short hash, so the id stays ids and
// digits.
func reactionID(message, user, emoji string) string {
	sum := sha256.Sum256([]byte(emoji))
	return "reaction:" + message + ":" + user + ":" + hex.EncodeToString(sum[:4])
}

// emojiName is a Unicode emoji itself, and a custom one as "name:id",
// the form React takes.
func emojiName(e discordgo.Emoji) string {
	if e.ID == "" {
		return e.Name
	}
	return e.Name + ":" + e.ID
}

// self returns the bot's own user id, once the gateway has said it.
func (s *session) self() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.me
}

// direct reports whether a channel is a direct message: outside any
// guild, and of the one-to-one kind when the state knows it. A bot
// cannot be added to a group DM, so a guildless channel the state has
// not seen is taken as one-to-one.
func (s *session) direct(channel, guild string) bool {
	if guild != "" {
		return false
	}
	if ch, err := s.dg.State.Channel(channel); err == nil {
		return ch.GuildID == "" && ch.Type == discordgo.ChannelTypeDM
	}
	return true
}
