package discord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

// maxText is the most a Discord message holds. Discord counts
// characters; the adapter counts UTF-16 code units, which is never fewer.
const maxText = 2000

// interactionLife is how long Discord accepts answers to an interaction
// through its token, less a margin for a request in flight. After it,
// public replies go to the channel and private ones to a direct message.
const interactionLife = 15*time.Minute - 30*time.Second

// noMentions lets a message ping nobody, whatever its text says, so a
// reply that quotes a person or the model cannot ping a role or
// @everyone.
var noMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

// responder answers one invocation or event. With an interaction, it
// answers through the interaction's token while that lasts: the first
// public reply fills the deferred response and the rest follow it up.
// Without one, it posts in channel.
type responder struct {
	s       *session
	ic      *discordgo.Interaction
	channel string
	guild   string
	// thread is set when channel is a thread.
	thread string
	caller string
	// target is the message a reply answers when it names none: the
	// message or reacted message an event is about.
	target string
	// deadline is when the interaction's token stops being accepted.
	deadline time.Time

	// orig serializes every request that changes the deferred response,
	// and guards state.
	orig  sync.Mutex
	state responseState

	// edits serializes Edit.
	edits sync.Mutex

	mu   sync.Mutex
	sent map[string]*sentMessage
}

// responseState is what an interaction's deferred response holds.
type responseState int

const (
	// placeholder: Discord's "thinking" placeholder. While it shows,
	// Discord takes the next follow-up as an edit of it, ignoring the
	// follow-up's ephemeral flag.
	placeholder responseState = iota
	// privateNote: privateNoteText, put there so a private reply can be
	// a follow-up of its own. A public reply may still replace it.
	privateNote
	// filled: a public reply.
	filled
	// removed: deleted when the command finished without filling it.
	removed
)

// privateNoteText replaces the placeholder before a private reply, which
// Discord would otherwise show in the placeholder's place, publicly.
const privateNoteText = "Answered privately."

// sentMessage records a message the responder posted, as the parts it
// was split into, so an edit can rewrite every part.
type sentMessage struct {
	private bool
	parts   []part
}

// part is one Discord message.
type part struct {
	id      string
	channel string
	// original is the interaction's deferred response, and hook any
	// message sent through the interaction's token.
	original, hook bool
}

func (s *session) interactionResponder(ic *discordgo.Interaction) *responder {
	r := s.channelResponder(ic.ChannelID, ic.GuildID, "")
	r.ic = ic
	r.deadline = s.now().Add(interactionLife)
	switch {
	case ic.Member != nil && ic.Member.User != nil:
		r.caller = ic.Member.User.ID
	case ic.User != nil:
		r.caller = ic.User.ID
	}
	return r
}

func (s *session) channelResponder(channel, guild, caller string) *responder {
	r := &responder{s: s, channel: channel, guild: guild, caller: caller, sent: map[string]*sentMessage{}}
	if ch, err := s.dg.State.Channel(channel); err == nil && ch.IsThread() {
		r.thread = channel
	}
	return r
}

// deferResponse acknowledges the interaction, which shows the person a
// "thinking" placeholder until the first public reply fills it.
func (r *responder) deferResponse(ctx context.Context) error {
	return r.s.dg.InteractionRespond(r.ic, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}, discordgo.WithContext(ctx))
}

// finish removes the deferred response when no public reply filled it,
// such as when a command only replied privately.
func (r *responder) finish(ctx context.Context) {
	r.orig.Lock()
	defer r.orig.Unlock()
	if r.state == filled || r.state == removed || !r.live() {
		return
	}
	r.state = removed
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.s.dg.InteractionResponseDelete(r.ic, discordgo.WithContext(ctx)); err != nil {
		r.s.log.Warn("could not remove a command's placeholder", "interaction", r.ic.ID, "error_kind", errKind(err))
	}
}

// live reports whether the interaction's token is still accepted.
func (r *responder) live() bool { return r.ic != nil && r.s.now().Before(r.deadline) }

// fill puts a public reply in the deferred response if no public reply
// has, and reports whether it took the response. The response counts as
// filled only once Discord accepts the edit, so a reply that fails
// leaves it for the next.
func (r *responder) fill(ctx context.Context, edit *discordgo.WebhookEdit) (*discordgo.Message, bool, error) {
	r.orig.Lock()
	defer r.orig.Unlock()
	if r.state != placeholder && r.state != privateNote {
		return nil, false, nil
	}
	msg, err := r.s.dg.InteractionResponseEdit(r.ic, edit, discordgo.WithContext(ctx))
	if err != nil {
		return nil, true, err
	}
	r.state = filled
	return msg, true, nil
}

// settle replaces the placeholder with privateNoteText, so the next
// follow-up is a message of its own and keeps its ephemeral flag.
func (r *responder) settle(ctx context.Context) error {
	r.orig.Lock()
	defer r.orig.Unlock()
	if r.state != placeholder {
		return nil
	}
	note := privateNoteText
	if _, err := r.s.dg.InteractionResponseEdit(r.ic, &discordgo.WebhookEdit{Content: &note, AllowedMentions: noMentions}, discordgo.WithContext(ctx)); err != nil {
		return err
	}
	r.state = privateNote
	return nil
}

func (r *responder) Reply(ctx context.Context, m airut.Message) (airut.MessageRef, error) {
	ref, _, err := r.send(ctx, m, nil)
	return ref, err
}

// send posts m, with components on its last part, and returns the parts
// it posted.
func (r *responder) send(ctx context.Context, m airut.Message, components []discordgo.MessageComponent) (airut.MessageRef, []part, error) {
	if m.Text == "" && len(m.Files) == 0 {
		return airut.MessageRef{}, nil, errors.New("discord: a message needs text or a file")
	}
	hook := r.live() && (m.Private || m.ReplyTo.ID == "")
	channel := r.channel
	if m.Private && !hook {
		dm, err := r.direct(ctx)
		if err != nil {
			return airut.MessageRef{}, nil, err
		}
		channel = dm
	}
	texts := split(strings.ToValidUTF8(m.Text, "\uFFFD"))
	rec := &sentMessage{private: m.Private}
	// failed returns err, with the first part of what was sent before
	// it, if any.
	failed := func(err error) (airut.MessageRef, []part, error) {
		if len(rec.parts) == 0 {
			return airut.MessageRef{}, nil, err
		}
		r.remember(rec)
		return airut.MessageRef{ID: rec.parts[0].id}, rec.parts, err
	}
	for i, text := range texts {
		var files []*discordgo.File
		var comps []discordgo.MessageComponent
		if i == len(texts)-1 {
			files, comps = discordFiles(m.Files), components
		}
		var p part
		var err error
		if hook && !r.live() {
			// The token ran out partway through: the rest goes where
			// a reply goes after it, keeping a private one private.
			hook = false
			if m.Private {
				if channel, err = r.direct(ctx); err != nil {
					return failed(err)
				}
			}
		}
		if hook {
			p, err = r.postHook(ctx, text, files, comps, m.Private)
		} else {
			reply := ""
			if i == 0 && !m.Private {
				reply = m.ReplyTo.ID
				if reply == "" {
					reply = r.target
				}
			}
			p, err = r.postChannel(ctx, channel, text, files, comps, reply, nonce(m.Key, i))
		}
		if err != nil {
			return failed(err)
		}
		rec.parts = append(rec.parts, p)
	}
	r.remember(rec)
	return airut.MessageRef{ID: rec.parts[0].id}, rec.parts, nil
}

func (r *responder) postHook(ctx context.Context, text string, files []*discordgo.File, components []discordgo.MessageComponent, private bool) (part, error) {
	opt := discordgo.WithContext(ctx)
	if !private {
		edit := &discordgo.WebhookEdit{Content: &text, Files: files, AllowedMentions: noMentions}
		if components != nil {
			edit.Components = &components
		}
		msg, took, err := r.fill(ctx, edit)
		if err != nil {
			return part{}, err
		}
		if took {
			return part{id: msg.ID, channel: r.channel, original: true, hook: true}, nil
		}
	} else if err := r.settle(ctx); err != nil {
		return part{}, err
	}
	p := &discordgo.WebhookParams{Content: text, Files: files, Components: components, AllowedMentions: noMentions}
	if private {
		p.Flags = discordgo.MessageFlagsEphemeral
	}
	msg, err := r.s.dg.FollowupMessageCreate(r.ic, true, p, opt)
	if err != nil {
		return part{}, err
	}
	return part{id: msg.ID, channel: r.channel, hook: true}, nil
}

// channelMessage is a message as Discord's create-message endpoint takes
// it. It is declared here because discordgo's MessageSend has no nonce.
type channelMessage struct {
	Content         string                            `json:"content,omitempty"`
	Components      []discordgo.MessageComponent      `json:"components,omitempty"`
	AllowedMentions *discordgo.MessageAllowedMentions `json:"allowed_mentions"`
	Reference       *discordgo.MessageReference       `json:"message_reference,omitempty"`
	Nonce           string                            `json:"nonce,omitempty"`
	EnforceNonce    bool                              `json:"enforce_nonce,omitempty"`
}

// postChannel posts one message to channel, as a reply to the message
// reply names when it is set. A nonce makes Discord answer a repeat of
// the post, for a few minutes, with the message it first created.
func (r *responder) postChannel(ctx context.Context, channel, text string, files []*discordgo.File, components []discordgo.MessageComponent, reply, nonce string) (part, error) {
	data := channelMessage{Content: text, Components: components, AllowedMentions: noMentions, Nonce: nonce, EnforceNonce: nonce != ""}
	if reply != "" {
		no := false
		data.Reference = &discordgo.MessageReference{MessageID: reply, ChannelID: channel, FailIfNotExists: &no}
	}
	endpoint := discordgo.EndpointChannelMessages(channel)
	opt := discordgo.WithContext(ctx)
	var body []byte
	var err error
	if len(files) > 0 {
		var ct string
		var b []byte
		if ct, b, err = discordgo.MultipartBodyWithJSON(data, files); err != nil {
			return part{}, err
		}
		body, err = r.s.dg.RequestRaw("POST", endpoint, ct, b, endpoint, 0, opt)
	} else {
		body, err = r.s.dg.RequestWithBucketID("POST", endpoint, data, endpoint, opt)
	}
	if err != nil {
		return part{}, err
	}
	var msg discordgo.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return part{}, fmt.Errorf("discord: reading the posted message: %w", err)
	}
	return part{id: msg.ID, channel: channel}, nil
}

// nonce derives the nonce for part i of a message keyed key: 25
// characters, the most Discord takes, of a hash of both, so the key's
// text never reaches Discord.
func nonce(key string, i int) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key + "\x00" + strconv.Itoa(i)))
	return hex.EncodeToString(sum[:])[:25]
}

// direct returns the direct-message channel with the caller.
func (r *responder) direct(ctx context.Context) (string, error) {
	if r.caller == "" {
		return "", errors.New("discord: a private reply needs someone to send it to")
	}
	ch, err := r.s.dg.UserChannelCreate(r.caller, discordgo.WithContext(ctx))
	if err != nil {
		return "", err
	}
	return ch.ID, nil
}

// The most messages a responder remembers for editing. A command or an
// event posts a handful; this only bounds a handler that never stops.
const keptMessages = 256

func (r *responder) remember(m *sentMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sent) >= keptMessages {
		for id := range r.sent {
			delete(r.sent, id)
			break
		}
	}
	r.sent[m.parts[0].id] = m
}

// lookup returns the record of a message the responder posted, and a
// copy of its parts taken under the lock that guards them.
func (r *responder) lookup(id string) (*sentMessage, []part) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.sent[id]
	if m == nil {
		return nil, nil
	}
	return m, append([]part(nil), m.parts...)
}

// Edit rewrites a message. Text that has grown past Discord's limit, as
// a streamed answer does, spills into further messages after the first,
// and an edit that shrinks it removes the parts it no longer needs. A
// message the responder did not send is edited in the invocation's
// channel, as one part.
func (r *responder) Edit(ctx context.Context, ref airut.MessageRef, m airut.Message) error {
	if m.Text == "" {
		return errors.New("discord: an edit needs text")
	}
	// One edit at a time, so two cannot each track parts the other
	// posted or removed.
	r.edits.Lock()
	defer r.edits.Unlock()
	rec, parts := r.lookup(ref.ID)
	if rec == nil {
		rec = &sentMessage{parts: []part{{id: ref.ID, channel: r.channel}}}
		parts = append([]part(nil), rec.parts...)
		r.remember(rec)
	}
	texts := split(strings.ToValidUTF8(m.Text, "\uFFFD"))
	for i, text := range texts {
		if i < len(parts) {
			if err := r.editPart(ctx, parts[i], text, nil); err != nil {
				return err
			}
			continue
		}
		var p part
		var err error
		switch {
		case parts[0].hook && r.live():
			p, err = r.postHook(ctx, text, nil, nil, rec.private)
		case parts[0].hook && rec.private:
			// The ephemeral message's token ran out mid-edit. Its
			// channel is the invocation's, which everyone sees, so
			// the rest goes to a direct message.
			var dm string
			if dm, err = r.direct(ctx); err == nil {
				p, err = r.postChannel(ctx, dm, text, nil, nil, "", "")
			}
		default:
			p, err = r.postChannel(ctx, parts[0].channel, text, nil, nil, "", "")
		}
		if err != nil {
			return err
		}
		// Record each new part as it is posted, so a failure later in
		// the loop leaves none untracked.
		parts = append(parts, p)
		r.setParts(rec, parts)
	}
	kept := parts[:len(texts):len(texts)]
	for _, p := range parts[len(texts):] {
		if err := r.removePart(ctx, p); err != nil {
			// Keep it, so a later edit reuses or removes it.
			kept = append(kept, p)
			r.s.log.Warn("could not remove part of an edited message", "message", p.id, "error_kind", errKind(err))
		}
	}
	r.setParts(rec, kept)
	return nil
}

func (r *responder) setParts(m *sentMessage, parts []part) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m.parts = append([]part(nil), parts...)
}

// editPart rewrites one message's text, and its components when
// components is not nil.
func (r *responder) editPart(ctx context.Context, p part, text string, components *[]discordgo.MessageComponent) error {
	opt := discordgo.WithContext(ctx)
	hook := &discordgo.WebhookEdit{Content: &text, Components: components, AllowedMentions: noMentions}
	var err error
	switch {
	case p.original && r.live():
		_, err = r.s.dg.InteractionResponseEdit(r.ic, hook, opt)
	case p.hook && r.live():
		_, err = r.s.dg.FollowupMessageEdit(r.ic, p.id, hook, opt)
	default:
		_, err = r.s.dg.ChannelMessageEditComplex(&discordgo.MessageEdit{
			ID: p.id, Channel: p.channel, Content: &text, Components: components, AllowedMentions: noMentions,
		}, opt)
	}
	return err
}

func (r *responder) removePart(ctx context.Context, p part) error {
	opt := discordgo.WithContext(ctx)
	switch {
	case p.original && r.live():
		return r.s.dg.InteractionResponseDelete(r.ic, opt)
	case p.hook && r.live():
		return r.s.dg.FollowupMessageDelete(r.ic, p.id, opt)
	}
	return r.s.dg.ChannelMessageDelete(p.channel, p.id, opt)
}

// React reacts to a message in the channel it was posted to. emoji is a
// Unicode emoji, or a custom one as "name:id" or "<:name:id>".
func (r *responder) React(ctx context.Context, ref airut.MessageRef, emoji string) error {
	channel := r.channel
	if _, parts := r.lookup(ref.ID); len(parts) > 0 {
		channel = parts[0].channel
	}
	return r.s.dg.MessageReactionAdd(channel, ref.ID, reactionEmoji(emoji), discordgo.WithContext(ctx))
}

var customEmoji = regexp.MustCompile(`^<a?:(\w+:\d+)>$`)

func reactionEmoji(e string) string {
	if m := customEmoji.FindStringSubmatch(e); m != nil {
		return m[1]
	}
	return e
}

func (r *responder) Typing(ctx context.Context) error {
	return r.s.dg.ChannelTyping(r.channel, discordgo.WithContext(ctx))
}

// threadArchive is how long a thread the bot opens stays open without
// activity, in minutes: a day.
const threadArchive = 1440

// Thread opens a thread named name: from the bot's first public reply
// when it has one, or else in the channel on its own. Inside a thread,
// it returns a responder for that thread, since Discord threads do not
// nest.
func (r *responder) Thread(ctx context.Context, name string) (airut.Responder, string, error) {
	if r.thread != "" {
		return r.s.channelResponder(r.thread, r.guild, r.caller), r.thread, nil
	}
	if r.guild == "" {
		return nil, "", errors.New("discord: a direct message cannot have threads")
	}
	opt := discordgo.WithContext(ctx)
	var ch *discordgo.Channel
	var err error
	if from := r.firstPublic(); from != "" {
		ch, err = r.s.dg.MessageThreadStart(r.channel, from, name, threadArchive, opt)
	} else {
		ch, err = r.s.dg.ThreadStart(r.channel, name, discordgo.ChannelTypeGuildPublicThread, threadArchive, opt)
	}
	if err != nil {
		return nil, "", err
	}
	t := r.s.channelResponder(ch.ID, r.guild, r.caller)
	t.thread = ch.ID
	return t, ch.ID, nil
}

// firstPublic returns the id of the deferred response once a public reply
// filled it.
func (r *responder) firstPublic() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.sent {
		if m.parts[0].original {
			return m.parts[0].id
		}
	}
	return ""
}

var (
	userMention    = regexp.MustCompile(`^<@!?(\d+)>$`)
	channelMention = regexp.MustCompile(`^<#(\d+)>$`)
	roleMention    = regexp.MustCompile(`^<@&(\d+)>$`)
	snowflake      = regexp.MustCompile(`^\d{1,20}$`)
)

// Resolve reads a user, channel, or role from its mention, such as
// "<@123>", "<#123>", or "<@&123>", or from its bare id.
func (r *responder) Resolve(ctx context.Context, kind airut.Kind, token string) (any, error) {
	return r.s.resolve(ctx, r.guild, kind, token)
}

func (s *session) resolve(ctx context.Context, guild string, kind airut.Kind, token string) (any, error) {
	opt := discordgo.WithContext(ctx)
	id, ok := mentioned(kind, token)
	if !ok {
		return nil, fmt.Errorf("discord: not a %s", kind)
	}
	switch kind {
	case airut.KindUser:
		if guild != "" {
			if m, err := s.dg.GuildMember(guild, id, opt); err == nil && m.User != nil {
				return user(m.User, m), nil
			}
		}
		u, err := s.dg.User(id, opt)
		if err != nil {
			return nil, err
		}
		return user(u, nil), nil
	case airut.KindChannel:
		ch, err := s.dg.State.Channel(id)
		if err != nil {
			if ch, err = s.dg.Channel(id, opt); err != nil {
				return nil, err
			}
		}
		return airut.Channel{Platform: airut.Discord, ID: ch.ID, Name: ch.Name}, nil
	case airut.KindRole:
		if guild == "" {
			return nil, errors.New("discord: roles exist only in a guild")
		}
		if ro, err := s.dg.State.Role(guild, id); err == nil {
			return airut.Role{Platform: airut.Discord, ID: ro.ID, Name: ro.Name}, nil
		}
		roles, err := s.dg.GuildRoles(guild, opt)
		if err != nil {
			return nil, err
		}
		for _, ro := range roles {
			if ro.ID == id {
				return airut.Role{Platform: airut.Discord, ID: ro.ID, Name: ro.Name}, nil
			}
		}
		return nil, errors.New("discord: no such role in this guild")
	}
	return nil, fmt.Errorf("discord: cannot resolve a %s", kind)
}

// mentioned reads the id from a mention of kind, or a bare id.
func mentioned(kind airut.Kind, token string) (string, bool) {
	re := map[airut.Kind]*regexp.Regexp{airut.KindUser: userMention, airut.KindChannel: channelMention, airut.KindRole: roleMention}[kind]
	if re == nil {
		return "", false
	}
	if m := re.FindStringSubmatch(token); m != nil {
		return m[1], true
	}
	return token, snowflake.MatchString(token)
}

func discordFiles(fs []airut.File) []*discordgo.File {
	var out []*discordgo.File
	for _, f := range fs {
		out = append(out, &discordgo.File{Name: f.Name, ContentType: f.ContentType, Reader: bytes.NewReader(f.Data)})
	}
	return out
}

// split cuts text into pieces Discord takes, at the last line break
// before the limit, or else the last space, or else at the limit. Empty
// text is one empty piece, which a message of files only carries. Each
// invalid byte counts as one unit and is never split.
func split(text string) []string {
	var out []string
	for units(text) > maxText {
		cut, n, lastLine, lastSpace := 0, 0, -1, -1
		for i, c := range text {
			n += utf16.RuneLen(c)
			if n > maxText {
				break
			}
			_, w := utf8.DecodeRuneInString(text[i:])
			cut = i + w
			switch c {
			case '\n':
				lastLine = cut
			case ' ':
				lastSpace = cut
			}
		}
		switch {
		case lastLine > 0:
			cut = lastLine
		case lastSpace > 0:
			cut = lastSpace
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	return append(out, text)
}

func units(s string) int {
	n := 0
	for _, c := range s {
		n += utf16.RuneLen(c)
	}
	return n
}
