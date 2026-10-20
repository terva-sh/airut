package matrix

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut"
)

// The bounds on one message. A Matrix event is at most 65535 bytes. In
// an encrypted room the whole event, serialized, is encrypted and
// base64-encoded, which adds a third, so the content is held to
// maxContent serialized bytes, leaving room for the envelope. Text is
// first cut at maxText bytes, and a piece whose rendered content is
// still too large, because escaping or the HTML copy grew it, is cut
// again.
const (
	maxText    = 24000
	maxContent = 40000
)

// responder answers one invocation or event: in room, inside thread when
// it is set, as a reply to target.
type responder struct {
	s      *session
	room   id.RoomID
	thread id.EventID
	target id.EventID
	caller id.UserID
}

func (s *session) responder(room id.RoomID, thread, target id.EventID, caller id.UserID) *responder {
	return &responder{s: s, room: room, thread: thread, target: target, caller: caller}
}

func (r *responder) Reply(ctx context.Context, m airut.Message) (airut.MessageRef, error) {
	if m.Private {
		room, err := r.s.direct(ctx, r.caller)
		if err != nil {
			return airut.MessageRef{}, err
		}
		return r.s.send(ctx, room, "", "", m)
	}
	target := r.target
	if m.ReplyTo.ID != "" {
		target = id.EventID(m.ReplyTo.ID)
	}
	return r.s.send(ctx, r.room, r.thread, target, m)
}

// Edit replaces a message in the room it was sent to, which for a
// private reply is not the invocation's room. A message the bot has no
// record of sending is edited only once the homeserver confirms it is in
// the invocation's room, so the new text never lands where the old text
// was not.
func (r *responder) Edit(ctx context.Context, ref airut.MessageRef, m airut.Message) error {
	ev := id.EventID(ref.ID)
	if err := r.s.c.Connect(ctx); err != nil {
		return err
	}
	room, ok := r.s.rooms.get(ev)
	if !ok {
		if _, err := r.s.c.GetEvent(ctx, r.room, ev); err != nil {
			return errors.New("matrix: the message to edit is not one the bot knows in this room")
		}
		room = r.room
	}
	_, err := r.s.c.SendMessageEvent(ctx, room, event.EventMessage, editContent(ev, m.Text))
	return err
}

// React reacts to a message in the room the bot sent it to, or else in
// the invocation's room.
func (r *responder) React(ctx context.Context, ref airut.MessageRef, emoji string) error {
	ev := id.EventID(ref.ID)
	room, ok := r.s.rooms.get(ev)
	if !ok {
		room = r.room
	}
	_, err := r.s.c.SendReaction(ctx, room, ev, emoji)
	return err
}

// Ask puts the question as rihma's reaction widget: one seeded reaction
// per choice, answerable only by a.Who. The widget is closed with the
// answer, or with a note that time ran out.
func (r *responder) Ask(ctx context.Context, a airut.Ask) (string, error) {
	ask := rihma.Ask{
		ID:      newID(),
		Room:    r.room,
		Text:    a.Question,
		Expires: a.Timeout,
	}
	for _, c := range a.Choices {
		ask.Options = append(ask.Options, rihma.AskOption{Key: c, Label: c})
	}
	if a.Who.ID != "" {
		ask.RestrictTo = []id.UserID{id.UserID(a.Who.ID)}
	}
	if r.thread != "" {
		ask.RelatesTo = (&event.RelatesTo{}).SetThread(r.thread, "")
	}
	asks := r.s.c.Asks()
	ans, err := asks.Wait(ctx, ask)
	outcome := ""
	switch {
	case err == nil:
		outcome = ans.Key
	case errors.Is(err, rihma.ErrAskExpired):
		outcome = "No answer in time"
		err = airut.ErrAskTimeout
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if cerr := asks.Close(cctx, ask.ID, outcome); cerr != nil {
		r.s.log.Warn("could not close a question", "room", string(r.room), "error_kind", errKind(cerr))
	}
	if err != nil {
		return "", err
	}
	return ans.Key, nil
}

func (r *responder) Typing(ctx context.Context) error {
	_, err := r.s.c.UserTyping(ctx, r.room, true, 30*time.Second)
	return err
}

// Thread returns a responder inside the thread the invocation is in, or
// in a new one rooted at the message that triggered it. Matrix threads
// have no names, so name is not shown.
func (r *responder) Thread(_ context.Context, _ string) (airut.Responder, string, error) {
	root := r.thread
	if root == "" {
		root = r.target
	}
	if root == "" {
		return nil, "", errors.New("matrix: a thread needs a message to start from")
	}
	return r.s.responder(r.room, root, r.target, r.caller), string(root), nil
}

// Resolve reads a user from "@user:server" or a matrix.to or matrix: link
// to one, and a room from "!room:server", "#alias:server", or a link.
// Matrix has no roles.
func (r *responder) Resolve(ctx context.Context, kind airut.Kind, token string) (any, error) {
	return r.s.resolve(ctx, r.room, kind, token)
}

func (s *session) resolve(ctx context.Context, room id.RoomID, kind airut.Kind, token string) (any, error) {
	tok, ok := identifier(kind, token)
	switch {
	case !ok:
		return nil, fmt.Errorf("matrix: not a %s", kind)
	case kind == airut.KindUser:
		return s.user(ctx, room, id.UserID(tok)), nil
	case strings.HasPrefix(tok, "#"):
		resp, err := s.c.ResolveAlias(ctx, id.RoomAlias(tok))
		if err != nil {
			return nil, err
		}
		return airut.Channel{Platform: airut.Matrix, ID: string(resp.RoomID), Name: tok}, nil
	}
	return channel(id.RoomID(tok)), nil
}

// identifier reads token as a Matrix identifier of the kind asked for: a
// user id for a user, and a room id or alias for a channel. A matrix.to
// or matrix: link to one is read as the identifier it names.
func identifier(kind airut.Kind, token string) (string, bool) {
	tok := strings.TrimSpace(token)
	if uri, err := id.ParseMatrixURIOrMatrixToURL(tok); err == nil {
		tok = uri.PrimaryIdentifier()
	}
	switch kind {
	case airut.KindUser:
		_, _, err := id.UserID(tok).ParseAndValidateRelaxed()
		return tok, err == nil
	case airut.KindChannel:
		for _, sigil := range []string{"!", "#"} {
			if rest, ok := strings.CutPrefix(tok, sigil); ok {
				local, server, found := strings.Cut(rest, ":")
				return tok, found && local != "" && server != "" && !strings.ContainsAny(tok, " \t\n")
			}
		}
	}
	return "", false
}

// send posts m into room, inside thread when it is set and as a reply to
// replyTo when that is. Text longer than one event allows goes out as
// several, and the reference names the first. Each event, text or file,
// gets its own transaction id from m.Key, numbered in the order sent.
func (s *session) send(ctx context.Context, room id.RoomID, thread, replyTo id.EventID, m airut.Message) (airut.MessageRef, error) {
	if err := s.c.Connect(ctx); err != nil {
		return airut.MessageRef{}, err
	}
	rel := relation(thread, replyTo)
	var first id.EventID
	part := 0
	for _, content := range messages(m.Text) {
		content.RelatesTo = rel
		resp, err := s.c.SendMessageEvent(ctx, room, event.EventMessage, content, mautrix.ReqSendEvent{TransactionID: txnID(m.Key, part)})
		part++
		if err != nil {
			return airut.MessageRef{ID: string(first)}, err
		}
		s.sent(room, resp.EventID, thread)
		if first == "" {
			first = resp.EventID
		}
	}
	for _, f := range m.Files {
		ev, err := s.c.SendMedia(ctx, room, rihma.Media{
			Data: f.Data, Name: f.Name, MimeType: f.ContentType, MsgType: mediaType(f.ContentType), RelatesTo: rel,
			TransactionID: txnID(m.Key, part),
		})
		part++
		if err != nil {
			return airut.MessageRef{ID: string(first)}, err
		}
		s.sent(room, ev, thread)
		if first == "" {
			first = ev
		}
	}
	return airut.MessageRef{ID: string(first)}, nil
}

// sent records where a message the bot posted went: its room, for a
// later edit or reaction, and its thread. Sync never shows the bot its
// own messages, so without this a reaction to one, such as a moderator's
// ✅ on a review, would not know its thread.
func (s *session) sent(room id.RoomID, ev, thread id.EventID) {
	s.rooms.put(ev, room)
	if thread != "" {
		s.threads.put(ev, thread)
	}
}

// relation places a message inside thread, as a reply to replyTo.
func relation(thread, replyTo id.EventID) *event.RelatesTo {
	switch {
	case thread != "" && replyTo != "":
		return (&event.RelatesTo{}).SetThread(thread, "").SetReplyTo(replyTo)
	case thread != "":
		return (&event.RelatesTo{}).SetThread(thread, thread)
	case replyTo != "":
		return (&event.RelatesTo{}).SetReplyTo(replyTo)
	}
	return nil
}

// messages renders text as one message, or as several when one would
// exceed maxContent.
func messages(text string) []*event.MessageEventContent {
	var out []*event.MessageEventContent
	var add func(piece string)
	add = func(piece string) {
		c := render(piece)
		if contentSize(c) <= maxContent || utf8.RuneCountInString(piece) <= 1 {
			out = append(out, c)
			return
		}
		for _, p := range split(piece, len(piece)/2) {
			add(p)
		}
	}
	for _, piece := range split(text, maxText) {
		add(piece)
	}
	return out
}

// editContent is the edit of original to text, shortened until it fits
// maxContent. An edit carries the text twice, as the fallback and the
// new content, so it cannot be split as a new message is.
func editContent(original id.EventID, text string) *event.MessageEventContent {
	text = truncate(text, maxText)
	for {
		c := render(text)
		c.SetEdit(original)
		// Text this short fits however it escapes; the floor only
		// guarantees the loop ends.
		if contentSize(c) <= maxContent || len(text) <= 64 {
			return c
		}
		text = truncate(text, len(text)/2)
	}
}

func contentSize(c *event.MessageEventContent) int {
	b, err := json.Marshal(c)
	if err != nil {
		return maxContent + 1
	}
	return len(b)
}

// render turns text into a message, Markdown rendered and any HTML in it
// shown as text. Its mentions are empty, so that text naming a user or
// "@room", which an agent's reply may hold, pings nobody.
func render(text string) *event.MessageEventContent {
	content := format.RenderMarkdown(text, true, false)
	content.MsgType = event.MsgText
	content.Body = text
	content.Mentions = &event.Mentions{}
	return &content
}

func mediaType(contentType string) event.MessageType {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(contentType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(contentType, "audio/"):
		return event.MsgAudio
	}
	return event.MsgFile
}

// split cuts text into pieces of at most max bytes, at a line break when
// one falls in the second half of a piece, and never inside a character.
// Empty text is no pieces.
func split(text string, max int) []string {
	var out []string
	for len(text) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			// max is smaller than the first character; keep it whole.
			_, cut = utf8.DecodeRuneInString(text)
		}
		if nl := strings.LastIndexByte(text[:cut], '\n'); nl >= max/2 {
			cut = nl + 1
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// truncate shortens text to at most max bytes, marking the cut.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const mark = "…"
	cut := max(limit-len(mark), 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + mark
}

// direct returns the room for private messages with u: one recorded in
// the account's m.direct that is still private to the two of them, or a
// new one. A room is reused only while privateRoom holds for it, so a
// reply never reaches a third member or goes out unencrypted.
func (s *session) direct(ctx context.Context, u id.UserID) (id.RoomID, error) {
	s.dmMu.Lock()
	defer s.dmMu.Unlock()
	if room, ok := s.dms[u]; ok && s.privateRoom(ctx, room, u) {
		return room, nil
	}
	direct := event.DirectChatsEventContent{}
	if err := s.c.GetAccountData(ctx, event.AccountDataDirectChats.Type, &direct); err != nil && !errors.Is(err, mautrix.MNotFound) {
		return "", err
	}
	for _, room := range direct[u] {
		if s.privateRoom(ctx, room, u) {
			s.dms[u] = room
			return room, nil
		}
	}
	// The initial state names the encryption and the join rule, which
	// CreateRoom records in the state store with the invite, so the room
	// passes privateRoom at once and the first message's keys are shared
	// with the invitee.
	resp, err := s.c.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Preset:   "trusted_private_chat",
		IsDirect: true,
		Invite:   []id.UserID{u},
		InitialState: []*event.Event{
			{Type: event.StateEncryption, Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}}},
			{Type: event.StateJoinRules, Content: event.Content{Parsed: &event.JoinRulesEventContent{JoinRule: event.JoinRuleInvite}}},
		},
	})
	if err != nil {
		return "", err
	}
	direct[u] = append(direct[u], resp.RoomID)
	if err := s.c.SetAccountData(ctx, event.AccountDataDirectChats.Type, direct); err != nil {
		s.log.Warn("could not record a direct room", "room", string(resp.RoomID), "error_kind", errKind(err))
	}
	s.dms[u] = resp.RoomID
	return resp.RoomID, nil
}

// privateRoom reports whether room is fit for a private reply to u, as
// the state store knows it: encrypted, joinable only by invitation, and
// with nobody joined or invited but the bot, joined, and u.
func (s *session) privateRoom(ctx context.Context, room id.RoomID, u id.UserID) bool {
	st := s.c.StateStore
	if enc, err := st.IsEncrypted(ctx, room); err != nil || !enc {
		return false
	}
	if jr, err := st.GetJoinRules(ctx, room); err != nil || jr == nil || jr.JoinRule != event.JoinRuleInvite {
		return false
	}
	members, err := st.GetAllMembers(ctx, room)
	return err == nil && onlyThem(members, s.c.UserID, u)
}

// onlyThem reports whether bot is joined and u joined or invited, and
// nobody else is either.
func onlyThem(members map[id.UserID]*event.MemberEventContent, bot, u id.UserID) bool {
	var botIn, uIn bool
	for who, m := range members {
		if m == nil || (m.Membership != event.MembershipJoin && m.Membership != event.MembershipInvite) {
			continue
		}
		switch who {
		case bot:
			botIn = m.Membership == event.MembershipJoin
		case u:
			uIn = true
		default:
			return false
		}
	}
	return botIn && uIn
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// txnID is the transaction id of part i of a message with key, or empty,
// which lets mautrix make a fresh one, for a message without a key. The
// homeserver answers a repeated transaction id from the same device with
// the event it already sent. The key is hashed so that a key of any
// length or alphabet makes a valid id.
func txnID(key string, i int) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("airut.%x.%d", sum[:16], i)
}
