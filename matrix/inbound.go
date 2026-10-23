package matrix

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"terva.sh/airut"
)

// names are the ways a message can address the bot at its start.
type names struct{ id, local, display string }

// stripMention returns body without a leading mention of the bot, and
// reports whether there was one. A mention is the bot's user id, display
// name, or localpart, in any case, followed by the end of the message, a
// colon, a comma, or a space: what a client writes for a pill is the
// display name and a colon.
func (n names) stripMention(body string) (string, bool) {
	for _, name := range []string{n.id, n.display, n.local} {
		if name == "" || len(body) < len(name) || !strings.EqualFold(body[:len(name)], name) {
			continue
		}
		rest := body[len(name):]
		if rest != "" && !strings.ContainsAny(rest[:1], ":, \t\n") {
			continue
		}
		return strings.TrimLeft(rest, ":, \t\n"), true
	}
	return body, false
}

// trigger returns the command a message's text calls. A mention of the
// bot followed by a word calls that word only when it is a registered
// command, so "Baphomet: what is this?" is chat for the event handlers
// rather than a call to a command named what. The prefix calls whatever
// follows it, and the core answers a name it does not know. With an
// empty prefix only a mention triggers, so that ordinary chat is never
// read as a command. In a direct channel every message addresses the
// bot, so a line reads as if it opened with a mention.
func trigger(text, prefix string, me names, known func(string) bool, direct bool) (name, rest string, ok bool) {
	text = strings.TrimSpace(text)
	r, mentioned := me.stripMention(text)
	if mentioned || direct {
		if prefix != "" {
			if name, rest, ok := airut.SplitCommand(r, prefix); ok {
				return name, rest, true
			}
		}
		if name, rest, ok := airut.SplitCommand(r, ""); ok && known(name) {
			return name, rest, true
		}
		return "", "", false
	}
	if prefix == "" {
		return "", "", false
	}
	return airut.SplitCommand(text, prefix)
}

// inbound is a Matrix message, read into what airut needs.
type inbound struct {
	evt     *event.Event
	content *event.MessageEventContent
	text    string
	files   []airut.Attachment
	thread  id.EventID
	reply   id.EventID
}

// parse reads a message event. It reports false for what is not a
// message a bot acts on: an edit, which would otherwise run a command a
// second time, and a redacted message, which has no content left. It
// records the message's thread, and the keys of an encrypted attachment,
// for later reactions and downloads.
func (s *session) parse(evt *event.Event) (inbound, bool) {
	in, ok := readMessage(evt)
	if !ok {
		return in, false
	}
	if in.thread != "" {
		s.threads.put(evt.ID, in.thread)
	}
	if in.content.File != nil {
		s.files.put(attachmentURL(in.content), in.content)
	}
	return in, true
}

// readMessage reads a message the bot may act on. Edits, redacted
// messages, and in-room verification requests are not: a request's body
// is a fallback telling clients without in-room verification to use
// another kind, and a handler or an agent turn would answer it as chat.
func readMessage(evt *event.Event) (inbound, bool) {
	c := evt.Content.AsMessage()
	if c == nil || c.MsgType == "" || c.MsgType == event.MsgVerificationRequest || c.RelatesTo.GetReplaceID() != "" {
		return inbound{}, false
	}
	in := inbound{
		evt:     evt,
		content: c,
		text:    messageText(c),
		thread:  c.RelatesTo.GetThreadParent(),
		reply:   c.RelatesTo.GetNonFallbackReplyTo(),
	}
	if a, ok := attachment(c); ok {
		in.files = []airut.Attachment{a}
	}
	return in, true
}

// command reports the command the message calls, if it calls one.
// Notices are never commands: bots send them, and answering one is how
// two bots talk to each other forever.
func (in inbound) command(prefix string, me names, known func(string) bool, direct bool) (name, rest string, ok bool) {
	if in.content.MsgType == event.MsgNotice {
		return "", "", false
	}
	return trigger(in.text, prefix, me, known, direct)
}

func (in inbound) invocation(sender airut.User, name, rest string) airut.Invocation {
	return airut.Invocation{
		ID:      string(in.evt.ID),
		Command: name,
		Text:    rest,
		Files:   in.files,
		Caller:  sender,
		Channel: channel(in.evt.RoomID),
		Thread:  string(in.thread),
		Message: airut.MessageRef{ID: string(in.evt.ID)},
		ReplyTo: airut.MessageRef{ID: string(in.reply)},
		Native:  in.evt,
	}
}

func (in inbound) messageEvent(sender airut.User, me names, direct bool) airut.MessageEvent {
	request, mentioned := in.addressed(me, direct)
	return airut.MessageEvent{
		ID:        string(in.evt.ID),
		Message:   airut.MessageRef{ID: string(in.evt.ID)},
		Sender:    sender,
		Channel:   channel(in.evt.RoomID),
		Thread:    string(in.thread),
		Direct:    direct,
		Text:      in.text,
		Mentioned: mentioned,
		Request:   request,
		Files:     in.files,
		ReplyTo:   airut.MessageRef{ID: string(in.reply)},
		Sent:      sent(in.evt),
		Native:    in.evt,
	}
}

// sent is when the homeserver says evt was sent, or zero when it gives
// no time.
func sent(evt *event.Event) time.Time {
	if evt.Timestamp <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(evt.Timestamp)
}

// addressed reports whether the message addresses the bot, and what it
// asks: the text after an opening mention, or the whole text when only
// m.mentions names the bot. Only leading space is skipped to find the
// mention, so the rest of the text reaches the handler as it was sent.
// A room mention alone does not address the bot, so "@room" never
// starts a turn. In a direct channel every message addresses the bot.
func (in inbound) addressed(me names, direct bool) (string, bool) {
	if rest, ok := me.stripMention(strings.TrimLeftFunc(in.text, unicode.IsSpace)); ok {
		return rest, true
	}
	if m := in.content.Mentions; direct || (m != nil && me.id != "" && m.Has(id.UserID(me.id))) {
		return in.text, true
	}
	return "", false
}

// messageText is what a message says: a text message's body without the
// quoted reply some clients still prepend, or a file's caption.
func messageText(c *event.MessageEventContent) string {
	if isMedia(c.MsgType) {
		return c.GetCaption()
	}
	return event.TrimReplyFallbackText(c.Body)
}

func isMedia(t event.MessageType) bool {
	switch t {
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		return true
	}
	return false
}

// encryptedMark ends the URL of an encrypted attachment. Its bytes are
// unreadable without the key the message carried, so Open refuses one
// whose key the adapter no longer holds rather than return ciphertext.
const encryptedMark = "#encrypted"

func attachmentURL(c *event.MessageEventContent) string {
	if c.File != nil {
		return string(c.File.URL) + encryptedMark
	}
	return string(c.URL)
}

func attachment(c *event.MessageEventContent) (airut.Attachment, bool) {
	if !isMedia(c.MsgType) || (c.URL == "" && (c.File == nil || c.File.URL == "")) {
		return airut.Attachment{}, false
	}
	a := airut.Attachment{Name: c.GetFileName(), URL: attachmentURL(c)}
	if c.Info != nil {
		a.ContentType = c.Info.MimeType
		a.Size = int64(c.Info.Size)
	}
	return a, true
}

// errKind names what kind of error err is, for a log line: a Matrix
// error code where the server sent one, never the error's text.
func errKind(err error) string {
	var he mautrix.HTTPError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &he) && he.RespError != nil && he.RespError.ErrCode != "":
		return he.RespError.ErrCode
	case errors.As(err, &he):
		return "http"
	}
	return "other"
}
