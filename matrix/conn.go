package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"terva.sh/airut"
)

// ErrKeyGone is returned by Open for an encrypted attachment whose key
// the adapter no longer holds. It keeps the keys of the most recent
// encrypted attachments it saw, live or through History; reading the
// message again through History restores one.
var ErrKeyGone = errors.New("matrix: the key for this encrypted attachment is no longer held")

// The bounds on one page of history.
const (
	defaultPage = 50
	maxPage     = 100
)

func (s *session) Platform() airut.Platform { return airut.Matrix }

// Send posts m to ch. A reply to a message in a thread stays in that
// thread.
func (s *session) Send(ctx context.Context, ch airut.Channel, m airut.Message) (airut.MessageRef, error) {
	if m.Private {
		return airut.MessageRef{}, errors.New("matrix: Conn.Send has nobody to send privately to; reply from a Ctx instead")
	}
	reply := id.EventID(m.ReplyTo.ID)
	thread, _ := s.threads.get(reply)
	return s.send(ctx, id.RoomID(ch.ID), thread, reply, m)
}

// Open downloads an attachment through the homeserver's authenticated
// media endpoint, and decrypts it when it was encrypted. A decrypted
// file's hash is checked only once it has been read to the end, so a
// read that stops early has not been verified.
func (s *session) Open(ctx context.Context, a airut.Attachment) (io.ReadCloser, error) {
	content, ok := s.files.get(a.URL)
	if !ok {
		if strings.HasSuffix(a.URL, encryptedMark) {
			return nil, ErrKeyGone
		}
		if _, err := id.ParseContentURI(a.URL); err != nil {
			return nil, errors.New("matrix: not a Matrix attachment")
		}
		content = &event.MessageEventContent{URL: id.ContentURIString(a.URL)}
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := s.c.DownloadMedia(ctx, content, 0, pw)
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// History returns messages in ch older than cursor, newest first. Those
// that were commands, edits, redacted, or the bot's own are left out, and
// so are encrypted ones it cannot decrypt, so a page can hold fewer than
// limit messages, or none, and still have a Next.
func (s *session) History(ctx context.Context, ch airut.Channel, cursor string, limit int, opts ...airut.HistoryOption) (airut.HistoryPage, error) {
	withBot := airut.ReadHistoryOptions(opts).IncludeBot
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	if err := s.c.Connect(ctx); err != nil {
		return airut.HistoryPage{}, err
	}
	room := id.RoomID(ch.ID)
	filter := &mautrix.FilterPart{Types: []event.Type{event.EventMessage, event.EventEncrypted}}
	resp, err := s.c.Messages(ctx, room, cursor, "", mautrix.DirectionBackward, filter, limit)
	if err != nil {
		return airut.HistoryPage{}, err
	}
	page := airut.HistoryPage{Next: resp.End}
	var undecryptable int
	for _, evt := range resp.Chunk {
		evt.RoomID = room
		if err := evt.Content.ParseRaw(evt.Type); err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
			continue
		}
		if evt.Type == event.EventEncrypted {
			dec, err := s.c.Crypto.Decrypt(ctx, evt)
			if err != nil {
				undecryptable++
				continue
			}
			evt = dec
		}
		own := evt.Sender == s.c.UserID
		if evt.Type != event.EventMessage || own && !withBot {
			continue
		}
		in, ok := s.parse(evt)
		if !ok {
			continue
		}
		if own {
			// The bot's answer as it ended, not the first fragment its
			// streaming posted.
			page.Messages = append(page.Messages, s.asMessage(ctx, room, in, true))
			continue
		}
		direct := s.isDirect(ctx, room, evt.Sender)
		if _, _, cmd := in.command(s.h.Prefix(), s.me, s.known, direct); cmd {
			continue
		}
		page.Messages = append(page.Messages, in.messageEvent(s.user(ctx, room, evt.Sender), s.me, direct))
	}
	if undecryptable > 0 {
		s.log.Info("history held messages it could not decrypt", "room", string(room), "count", undecryptable)
	}
	return page, nil
}

// Member reports whether u is joined to ch, as the bot's state store
// knows it. The bot learns a room's members only by being in it.
func (s *session) Member(ctx context.Context, ch airut.Channel, u airut.User) (bool, error) {
	m, err := s.c.StateStore.TryGetMember(ctx, id.RoomID(ch.ID), id.UserID(u.ID))
	if err != nil {
		return false, err
	}
	return m != nil && m.Membership == event.MembershipJoin, nil
}

// React sends an m.reaction to the message ref in ch and returns the
// reaction's event id. mautrix never encrypts a reaction, so its
// relation is readable even in an encrypted room. A homeserver refuses
// a second identical reaction from the same sender, so when the send
// fails React looks for the bot's own reaction with this key and
// returns that.
func (s *session) React(ctx context.Context, ch airut.Channel, ref airut.MessageRef, emoji string) (airut.MessageRef, error) {
	room, target := id.RoomID(ch.ID), id.EventID(ref.ID)
	resp, err := s.c.SendReaction(ctx, room, target, emoji)
	if err == nil {
		return airut.MessageRef{ID: string(resp.EventID)}, nil
	}
	if own, ok := s.ownReaction(ctx, room, target, emoji); ok {
		return airut.MessageRef{ID: string(own)}, nil
	}
	return airut.MessageRef{}, err
}

// relationPage is how many reactions ownReaction reads a request. A
// test lowers it to make the lookup page.
var relationPage = 100

// ownReaction finds the bot's live reaction with key on target. It reads
// every page of target's reactions, until ctx ends, because the bot's
// may be the oldest of many. It stops early if the homeserver hands back
// a page token it already gave.
func (s *session) ownReaction(ctx context.Context, room id.RoomID, target id.EventID, key string) (id.EventID, bool) {
	req := &mautrix.ReqGetRelations{RelationType: event.RelAnnotation, EventType: event.EventReaction, Limit: relationPage}
	seen := map[string]bool{}
	for ctx.Err() == nil {
		resp, err := s.c.GetRelations(ctx, room, target, req)
		if err != nil {
			return "", false
		}
		for _, evt := range resp.Chunk {
			if evt.Sender != s.c.UserID {
				continue
			}
			if err := evt.Content.ParseRaw(evt.Type); err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
				continue
			}
			if r := evt.Content.AsReaction(); r.RelatesTo.Key == key {
				return evt.ID, true
			}
		}
		if resp.NextBatch == "" || seen[resp.NextBatch] {
			break
		}
		seen[resp.NextBatch] = true
		req.From = resp.NextBatch
	}
	return "", false
}

// Unreact redacts a reaction the bot sent. It fetches the event first
// and redacts only an m.reaction whose sender is the bot. An encrypted
// event is refused unread, because the bot's reactions are never
// encrypted.
func (s *session) Unreact(ctx context.Context, ch airut.Channel, reaction airut.MessageRef) error {
	room, ev := id.RoomID(ch.ID), id.EventID(reaction.ID)
	if ev == "" {
		return airut.ErrNotOwnReaction
	}
	evt, err := s.c.GetEvent(ctx, room, ev)
	if err != nil {
		return err
	}
	if evt.Type != event.EventReaction || evt.Sender != s.c.UserID {
		return airut.ErrNotOwnReaction
	}
	if evt.Unsigned.RedactedBecause != nil {
		return nil
	}
	_, err = s.c.RedactEvent(ctx, room, ev)
	return err
}

// Direct reports whether ch is a direct room: named in the account's
// m.direct, encrypted, joinable only by invitation, and with nobody
// joined or invited but the bot and one other person, whom it returns.
// It reads the state store as it is now.
func (s *session) Direct(ctx context.Context, ch airut.Channel) (airut.User, bool, error) {
	room := id.RoomID(ch.ID)
	members, err := s.c.StateStore.GetAllMembers(ctx, room)
	if err != nil {
		return airut.User{}, false, err
	}
	var other id.UserID
	for who, m := range members {
		if who == s.c.UserID || m == nil || (m.Membership != event.MembershipJoin && m.Membership != event.MembershipInvite) {
			continue
		}
		if other != "" {
			return airut.User{}, false, nil
		}
		other = who
	}
	if other == "" || !s.isDirect(ctx, room, other) {
		return airut.User{}, false, nil
	}
	return s.user(ctx, room, other), true, nil
}

// Profile reads the bot's global display name and avatar. Matrix has no
// description, so About is always empty.
func (s *session) Profile(ctx context.Context) (airut.Profile, error) {
	if err := s.c.Connect(ctx); err != nil {
		return airut.Profile{}, err
	}
	resp, err := s.c.GetProfile(ctx, s.c.UserID)
	if err != nil {
		return airut.Profile{}, err
	}
	p := airut.Profile{Name: resp.DisplayName}
	if !resp.AvatarURL.IsEmpty() {
		p.Avatar = resp.AvatarURL.String()
	}
	return p, nil
}

// SetProfile changes the bot's global display name and avatar, which
// the homeserver carries into every room the bot is in. A new picture is
// uploaded as plain media, since a profile is public. A change that sets
// About fails with airut.ErrUnsupported before anything changes, and a
// name the bot already has is not set again.
func (s *session) SetProfile(ctx context.Context, p airut.ProfileChange) error {
	if err := p.Check(); err != nil {
		return err
	}
	if p.About != nil {
		return fmt.Errorf("matrix: a profile description: %w", airut.ErrUnsupported)
	}
	if err := s.c.Connect(ctx); err != nil {
		return err
	}
	if p.Name != nil {
		cur, err := s.c.GetOwnDisplayName(ctx)
		if err != nil || cur.DisplayName != *p.Name {
			if err := s.c.SetDisplayName(ctx, *p.Name); err != nil {
				return err
			}
		}
	}
	if p.Avatar != nil {
		up, err := s.c.UploadBytes(ctx, p.Avatar.Data, p.Avatar.Type)
		if err != nil {
			return err
		}
		if err := s.c.SetAvatarURL(ctx, up.ContentURI); err != nil {
			return err
		}
	}
	return nil
}
