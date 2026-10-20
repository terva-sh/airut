package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

// The bounds on one page of history: Discord returns at most 100
// messages a request.
const (
	defaultPage = 50
	maxPage     = 100
)

// attachmentHosts are the hosts Discord serves attachments from. Open
// fetches from no other, so an Attachment cannot point it elsewhere.
var attachmentHosts = []string{"cdn.discordapp.com", "media.discordapp.net"}

// Discord's error codes for a user who is not in the guild, and for a
// user who does not exist.
const (
	unknownMember = 10007
	unknownUser   = 10013
)

func (s *session) Platform() airut.Platform { return airut.Discord }

// Send posts m to ch. A Key makes Discord answer a repeat of the post
// with the message it first created, for a few minutes.
func (s *session) Send(ctx context.Context, ch airut.Channel, m airut.Message) (airut.MessageRef, error) {
	if m.Private {
		return airut.MessageRef{}, errors.New("discord: Conn.Send has nobody to send privately to; reply from a Ctx instead")
	}
	guild := ""
	if c, err := s.dg.State.Channel(ch.ID); err == nil {
		guild = c.GuildID
	}
	return s.channelResponder(ch.ID, guild, "").Reply(ctx, m)
}

// Open downloads an attachment from Discord's content servers. Their
// links are signed and need no token, so the bot's token is never sent.
func (s *session) Open(ctx context.Context, a airut.Attachment) (io.ReadCloser, error) {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || !slices.Contains(attachmentHosts, u.Hostname()) {
		return nil, errors.New("discord: not a Discord attachment")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// discordgo's client, with no time limit of its own, since the body
	// takes as long as the caller reads it, and no redirect off
	// Discord's content servers.
	c := *s.dg.Client
	c.Timeout = 0
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || r.URL.Scheme != "https" || !slices.Contains(attachmentHosts, r.URL.Hostname()) {
			return errors.New("discord: an attachment redirected off Discord's content servers")
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord: downloading an attachment: %w", errKindOnly(err))
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("discord: downloading an attachment: status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// History returns messages in ch older than cursor, newest first. The
// bot's own messages, and messages other than plain ones and replies,
// such as a member joining, are left out, so a page can hold fewer than
// limit messages and still have a Next. The cursor is a message id.
func (s *session) History(ctx context.Context, ch airut.Channel, cursor string, limit int) (airut.HistoryPage, error) {
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	msgs, err := s.dg.ChannelMessages(ch.ID, limit, cursor, "", "", discordgo.WithContext(ctx))
	if err != nil {
		return airut.HistoryPage{}, err
	}
	var page airut.HistoryPage
	if len(msgs) == limit {
		page.Next = msgs[len(msgs)-1].ID
	}
	for _, m := range msgs {
		if m.GuildID == "" {
			m.GuildID = s.guildOf(ch.ID)
		}
		if s.delivers(m) {
			page.Messages = append(page.Messages, s.messageEvent(m))
		}
	}
	return page, nil
}

// Member reports whether u is in ch: in its guild, for a guild channel,
// or among its recipients, for a direct message. Membership of a guild
// says nothing of whether u may read a channel in it.
func (s *session) Member(ctx context.Context, ch airut.Channel, u airut.User) (bool, error) {
	opt := discordgo.WithContext(ctx)
	c, err := s.dg.State.Channel(ch.ID)
	if err != nil {
		if c, err = s.dg.Channel(ch.ID, opt); err != nil {
			return false, err
		}
	}
	if c.GuildID == "" {
		return slices.ContainsFunc(c.Recipients, func(r *discordgo.User) bool { return r.ID == u.ID }), nil
	}
	_, err = s.dg.GuildMember(c.GuildID, u.ID, opt)
	var re *discordgo.RESTError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &re) && re.Message != nil && (re.Message.Code == unknownMember || re.Message.Code == unknownUser):
		return false, nil
	}
	return false, err
}

// guildOf returns the guild of a channel in discordgo's cache, or "".
func (s *session) guildOf(channel string) string {
	if c, err := s.dg.State.Channel(channel); err == nil {
		return c.GuildID
	}
	return ""
}

// React adds the bot's reaction to the message ref in ch. Discord gives
// a reaction no id of its own, so the ref returned is the message id
// and the emoji, joined by a slash. Adding a reaction the bot already
// added succeeds, and returns the same ref.
func (s *session) React(ctx context.Context, ch airut.Channel, ref airut.MessageRef, emoji string) (airut.MessageRef, error) {
	e := reactionEmoji(emoji)
	if !snowflake.MatchString(ch.ID) || !snowflake.MatchString(ref.ID) || !pathSafe(e) {
		return airut.MessageRef{}, errors.New("discord: not a channel, message, and emoji Discord can react with")
	}
	if err := s.dg.MessageReactionAdd(ch.ID, ref.ID, e, discordgo.WithContext(ctx)); err != nil {
		return airut.MessageRef{}, err
	}
	return airut.MessageRef{ID: ref.ID + "/" + e}, nil
}

// Unreact removes the bot's own reaction through the endpoint that
// removes only the bot's, so no ref can reach anyone else's reaction. A
// ref React did not make is refused before any request, and so is one
// whose parts could change the request's path.
func (s *session) Unreact(ctx context.Context, ch airut.Channel, reaction airut.MessageRef) error {
	msg, e, ok := strings.Cut(reaction.ID, "/")
	if !ok || !snowflake.MatchString(ch.ID) || !snowflake.MatchString(msg) || !pathSafe(e) {
		return airut.ErrNotOwnReaction
	}
	return s.dg.MessageReactionRemove(ch.ID, msg, e, "@me", discordgo.WithContext(ctx))
}

// pathSafe reports whether an emoji, as reactionEmoji gives it, can go
// into a request path as one segment: a custom emoji's name:id, or text
// with none of the characters that end or escape a segment. A '#', as in
// the keycap emoji, is allowed because discordgo's reaction calls escape
// it, which TestConnReactions pins: unescaped, it would end the path
// before the @me that limits a removal to the bot's own reaction.
func pathSafe(e string) bool {
	return e != "" && !strings.ContainsAny(e, "/\\?%. \t\r\n")
}
