package matrix

import (
	"context"
	"errors"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"terva.sh/airut"
)

// editPage is how many of a message's edits newestEdit reads a request.
// A test lowers it to make the lookup page.
var editPage = 100

// Fetch reads one message from ch, decrypted, as it reads now: with the
// newest edit its sender made, and marked FromBot when the bot sent it.
func (s *session) Fetch(ctx context.Context, ch airut.Channel, ref airut.MessageRef) (airut.MessageEvent, error) {
	if err := s.c.Connect(ctx); err != nil {
		return airut.MessageEvent{}, err
	}
	return s.fetch(ctx, id.RoomID(ch.ID), id.EventID(ref.ID))
}

func (s *session) fetch(ctx context.Context, room id.RoomID, ev id.EventID) (airut.MessageEvent, error) {
	evt, err := s.readEvent(ctx, room, ev)
	if err != nil {
		return airut.MessageEvent{}, err
	}
	in, ok := readMessage(evt)
	if !ok {
		return airut.MessageEvent{}, errors.New("matrix: the event is not a message")
	}
	return s.asMessage(ctx, room, in, true), nil
}

// asMessage describes a message read from history or fetched, with the
// newest edit applied when edited is set, as it must be for the bot's
// own streamed answers, whose first event holds only the start.
func (s *session) asMessage(ctx context.Context, room id.RoomID, in inbound, edited bool) airut.MessageEvent {
	direct := s.isDirect(ctx, room, in.evt.Sender)
	ev := in.messageEvent(s.user(ctx, room, in.evt.Sender), s.me, direct)
	ev.FromBot = in.evt.Sender == s.c.UserID
	if ev.FromBot {
		ev.Mentioned, ev.Request = false, ""
	}
	if edited {
		if c, ok := s.newestEdit(ctx, room, in.evt); ok {
			ev.Text = messageText(c)
		}
	}
	return ev
}

// readEvent gets an event and decrypts it.
func (s *session) readEvent(ctx context.Context, room id.RoomID, ev id.EventID) (*event.Event, error) {
	evt, err := s.c.GetEvent(ctx, room, ev)
	if err != nil {
		return nil, err
	}
	evt.RoomID = room
	if err := evt.Content.ParseRaw(evt.Type); err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
		return nil, err
	}
	if evt.Type == event.EventEncrypted {
		if evt, err = s.c.Crypto.Decrypt(ctx, evt); err != nil {
			return nil, err
		}
	}
	return evt, nil
}

// newestEdit returns the new content of the newest edit of evt that its
// own sender made, if there is one. An edit by anyone else does not
// count, as clients ignore it too, so it reads every page of evt's edits,
// newest first, until it finds one or ctx ends: other people's edits
// cannot push the sender's out of reach. It stops early if the
// homeserver hands back a page token it already gave.
func (s *session) newestEdit(ctx context.Context, room id.RoomID, evt *event.Event) (*event.MessageEventContent, bool) {
	req := &mautrix.ReqGetRelations{RelationType: event.RelReplace, Dir: mautrix.DirectionBackward, Limit: editPage}
	seen := map[string]bool{}
	for ctx.Err() == nil {
		resp, err := s.c.GetRelations(ctx, room, evt.ID, req)
		if err != nil {
			s.log.Info("could not read a message's edits", "event", string(evt.ID), "error_kind", errKind(err))
			return nil, false
		}
		for _, e := range resp.Chunk {
			if c, ok := s.edit(ctx, room, evt, e); ok {
				return c, true
			}
		}
		if resp.NextBatch == "" || seen[resp.NextBatch] {
			break
		}
		seen[resp.NextBatch] = true
		req.From = resp.NextBatch
	}
	return nil, false
}

// edit returns the new content e gives evt, when e is an edit of evt by
// evt's own sender.
func (s *session) edit(ctx context.Context, room id.RoomID, evt, e *event.Event) (*event.MessageEventContent, bool) {
	if e.Sender != evt.Sender {
		return nil, false
	}
	e.RoomID = room
	if err := e.Content.ParseRaw(e.Type); err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
		return nil, false
	}
	if e.Type == event.EventEncrypted {
		dec, err := s.c.Crypto.Decrypt(ctx, e)
		if err != nil {
			return nil, false
		}
		e = dec
	}
	c := e.Content.AsMessage()
	if c == nil || c.NewContent == nil || c.RelatesTo.GetReplaceID() != evt.ID {
		return nil, false
	}
	return c.NewContent, true
}

// chain resolves what in replies to, nearest first, to the session's
// depth and within airut's caps. A link the bot cannot read or decrypt
// ends the chain.
func (s *session) chain(ctx context.Context, room id.RoomID, in inbound) []airut.Quoted {
	var out []airut.Quoted
	next := in.reply
	for more := true; more && next != "" && len(out) < s.replyDepth; {
		evt, err := s.readEvent(ctx, room, next)
		if err != nil {
			s.log.Info("a reply chain ended at a message the bot could not read", "event", string(next), "error_kind", errKind(err))
			break
		}
		link, ok := readMessage(evt)
		if !ok {
			break
		}
		out, more = airut.AddQuote(out, airut.Quote(s.asMessage(ctx, room, link, true)))
		next = link.reply
	}
	return out
}
