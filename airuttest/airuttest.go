// Package airuttest runs an airut bot in process, with no network: a fake
// adapter that records everything the bot sends, and a scripted fake
// agent.
package airuttest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
)

// Platform is the platform the fake adapter reports by default.
const Platform airut.Platform = "test"

// Event is one thing the bot did through the fake adapter.
type Event struct {
	// Kind is "reply", "edit", "react", "ask", "typing", or "thread",
	// or "send" for a message posted through the adapter's Conn. A
	// reaction added through the Conn is also "react", with Channel and
	// Reaction set, and one retracted through it is "unreact".
	Kind    string
	Ref     string // the message replied, edited, or reacted to
	Text    string // the message text, the emoji, the question, or the thread name
	Private bool
	Files   []string
	Choices []string
	Thread  string // the thread the event happened in, if any
	// ReplyTo is the message a reply answers, when it set one.
	ReplyTo string
	// Channel is the channel a send, or a reaction made through the
	// Conn, went to.
	Channel string
	// Reaction is the ref Conn.React returned for the reaction added or
	// retracted.
	Reaction string
}

// Adapter is a fake platform. Slash and Text deliver invocations and
// return what the bot did in answer.
type Adapter struct {
	platform airut.Platform
	ready    chan struct{}

	mu     sync.Mutex
	host   airut.Host
	ctx    context.Context
	events []Event
	invSeq int
	refSeq int
	answer func(airut.Ask) (string, error)

	// What the adapter's Conn serves.
	files      map[string][]byte
	history    map[string][]airut.MessageEvent // per channel id, oldest first
	members    map[string]map[string]bool      // channel id -> user id
	memberErrs map[string]error                // channel id -> the error Member fails with
	// keys maps a message's Key to the ref it was sent as.
	keys map[string]string
	// reactions holds the reactions made through the Conn, by ref, and
	// live maps a channel, message, and emoji to the live one's ref.
	reactions map[string]reaction
	live      map[reaction]string
}

// reaction is one reaction the bot made through the Conn.
type reaction struct{ channel, target, emoji string }

// New returns a fake adapter for platform p, or for Platform when p is
// empty.
func New(p airut.Platform) *Adapter {
	if p == "" {
		p = Platform
	}
	return &Adapter{platform: p, ready: make(chan struct{})}
}

// Platform reports the adapter's platform.
func (a *Adapter) Platform() airut.Platform { return a.platform }

// Run serves until ctx ends.
func (a *Adapter) Run(ctx context.Context, h airut.Host) error {
	a.mu.Lock()
	a.host, a.ctx = h, ctx
	a.mu.Unlock()
	h.Ready(ctx, a.Conn())
	close(a.ready)
	<-ctx.Done()
	return nil
}

// Ready is closed once the bot is serving through the adapter.
func (a *Adapter) Ready() <-chan struct{} { return a.ready }

// Start runs b on a new fake adapter until the test ends, and returns
// the adapter once the bot is serving.
func Start(t testing.TB, b *airut.Bot) *Adapter {
	t.Helper()
	a := New("")
	ctx, cancel := context.WithCancel(context.Background())
	// finished is closed, never sent on, so startup and cleanup can both
	// see that Run returned.
	finished := make(chan struct{})
	var runErr error
	go func() {
		defer close(finished)
		runErr = b.Run(ctx, a)
	}()
	early := false
	t.Cleanup(func() {
		cancel()
		<-finished
		if runErr != nil && !early {
			t.Errorf("bot.Run: %v", runErr)
		}
	})
	select {
	case <-a.ready:
	case <-finished:
		early = true
		t.Fatalf("bot.Run returned before serving: %v", runErr)
	}
	return a
}

// Specs returns the bot's command specs, as an adapter would register
// them.
func (a *Adapter) Specs() []airut.Spec { return a.host.Specs() }

// OnAsk sets how asks are answered. Without it, an ask waits until it
// times out.
func (a *Adapter) OnAsk(f func(airut.Ask) (string, error)) {
	a.mu.Lock()
	a.answer = f
	a.mu.Unlock()
}

// Events returns everything the bot did through the adapter, in order.
func (a *Adapter) Events() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Event(nil), a.events...)
}

// Option adjusts an invocation, or the message it arrives as, before it
// is delivered.
type Option func(*delivery)

// delivery is what an Option adjusts: the invocation, and when its
// message was sent.
type delivery struct {
	airut.Invocation
	sent time.Time
}

// SentAt dates the delivered message, as a platform stamps it, so a test
// can deliver one sent long ago. A message is otherwise sent when it is
// delivered.
func SentAt(t time.Time) Option { return func(d *delivery) { d.sent = t } }

// As makes u the caller.
func As(u airut.User) Option { return func(i *delivery) { i.Caller = u } }

// In delivers the invocation in channel ch.
func In(ch airut.Channel) Option { return func(i *delivery) { i.Channel = ch } }

// InThread delivers the invocation in the thread with this id.
func InThread(id string) Option { return func(i *delivery) { i.Thread = id } }

// WithRoles gives the caller roles.
func WithRoles(r ...airut.Role) Option { return func(i *delivery) { i.Roles = r } }

// WithFiles attaches files to a text invocation.
func WithFiles(f ...airut.Attachment) Option { return func(i *delivery) { i.Files = f } }

// Slash delivers a structured invocation, as a platform with typed
// arguments does, and returns what the bot did in answer. values holds
// string, bool, int64, float64, User, Channel, Role, or Attachment
// values; nil means no arguments.
func (a *Adapter) Slash(name string, values map[string]any, opts ...Option) []Event {
	if values == nil {
		values = map[string]any{}
	}
	return a.deliver(airut.Invocation{Command: name, Values: values}, opts, nil)
}

// Text delivers a line of chat, and returns what the bot did in answer.
// A line that starts with the bot's prefix and a command name is a
// command; any other line is a message for the bot's OnMessage handlers,
// as a text adapter delivers ordinary chat.
func (a *Adapter) Text(line string, opts ...Option) []Event {
	name, rest, ok := airut.SplitCommand(line, a.host.Prefix())
	if !ok {
		return a.deliver(airut.Invocation{Text: line}, opts, func(inv airut.Invocation, sent time.Time) airut.Event {
			return airut.MessageEvent{
				ID: inv.ID, Message: inv.Message, Sender: inv.Caller, Roles: inv.Roles, Channel: inv.Channel,
				Thread: inv.Thread, Text: inv.Text, Files: inv.Files, ReplyTo: inv.ReplyTo, Native: inv.Native, Sent: sent,
			}
		})
	}
	return a.deliver(airut.Invocation{Command: name, Text: rest}, opts, nil)
}

// BotName is the name a line delivered by Mention addresses the bot by.
const BotName = "Bot"

// Mention delivers a line of chat that opens with a mention of the bot,
// as "Bot: line", and returns what the bot did in answer. As on Matrix,
// a line that starts with the prefix and a command name calls it, and so
// does a bare first word that names a registered command. Any other line
// reaches the OnMessage handlers with Mentioned set, and as Request the
// line without the leading space Matrix skips after a mention.
func (a *Adapter) Mention(line string, opts ...Option) []Event {
	if name, rest, ok := airut.SplitCommand(line, a.host.Prefix()); ok {
		return a.deliver(airut.Invocation{Command: name, Text: rest}, opts, nil)
	}
	if name, rest, ok := airut.SplitCommand(line, ""); ok && a.known(name) {
		return a.deliver(airut.Invocation{Command: name, Text: rest}, opts, nil)
	}
	request := strings.TrimLeft(line, ":, \t\n")
	return a.deliver(airut.Invocation{Text: BotName + ": " + line}, opts, func(inv airut.Invocation, sent time.Time) airut.Event {
		return airut.MessageEvent{
			ID: inv.ID, Message: inv.Message, Sender: inv.Caller, Roles: inv.Roles, Channel: inv.Channel,
			Thread: inv.Thread, Text: inv.Text, Mentioned: true, Request: request, Files: inv.Files,
			ReplyTo: inv.ReplyTo, Native: inv.Native, Sent: sent,
		}
	})
}

func (a *Adapter) known(name string) bool {
	for _, s := range a.host.Specs() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// React delivers a reaction with emoji on the message ref, and returns
// what the bot did in answer.
func (a *Adapter) React(ref airut.MessageRef, emoji string, opts ...Option) []Event {
	return a.deliver(airut.Invocation{}, opts, func(inv airut.Invocation, sent time.Time) airut.Event {
		return airut.ReactionEvent{
			ID: inv.ID, Sender: inv.Caller, Roles: inv.Roles, Channel: inv.Channel, Thread: inv.Thread,
			Target: ref, Emoji: emoji, Native: inv.Native,
		}
	})
}

// ReplyingTo makes the delivered message a reply to ref.
func ReplyingTo(ref airut.MessageRef) Option { return func(i *delivery) { i.ReplyTo = ref } }

// deliver fills in the invocation's defaults and applies opts, then
// dispatches it as a command, or as the event asEvent builds from it.
func (a *Adapter) deliver(inv airut.Invocation, opts []Option, asEvent func(airut.Invocation, time.Time) airut.Event) []Event {
	a.mu.Lock()
	a.invSeq++
	inv.ID = "inv-" + strconv.Itoa(a.invSeq)
	inv.Caller = airut.User{Platform: a.platform, ID: "user-1", Name: "tester"}
	inv.Channel = airut.Channel{Platform: a.platform, ID: "channel-1", Name: "general"}
	inv.Message = airut.MessageRef{ID: "msg-" + inv.ID}
	inv.Respond = &responder{a: a}
	inv.Native = "native-" + inv.ID
	start := len(a.events)
	h, ctx := a.host, a.ctx
	a.mu.Unlock()
	d := delivery{Invocation: inv, sent: time.Now()}
	for _, o := range opts {
		o(&d)
	}
	inv = d.Invocation
	// A message in a thread is answered inside it, as on Matrix.
	inv.Respond = &responder{a: a, thread: inv.Thread}
	if asEvent != nil {
		h.Deliver(ctx, asEvent(inv, d.sent), inv.Respond)
	} else {
		h.Dispatch(ctx, inv)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Event(nil), a.events[start:]...)
}

type responder struct {
	a      *Adapter
	thread string
}

func (r *responder) record(e Event) string {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	return r.recordLocked(e)
}

// recordLocked records e; the caller holds the adapter's lock.
func (r *responder) recordLocked(e Event) string {
	e.Thread = r.thread
	if e.Ref == "" && e.Kind == "reply" {
		r.a.refSeq++
		e.Ref = "sent-" + strconv.Itoa(r.a.refSeq)
	}
	r.a.events = append(r.a.events, e)
	return e.Ref
}

func fileNames(fs []airut.File) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

// Reply records a reply. A repeated Key records nothing and returns the
// first message's ref; the lookup and the record happen under one lock,
// so concurrent sends with one key record one message.
func (r *responder) Reply(_ context.Context, m airut.Message) (airut.MessageRef, error) {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	if ref, ok := r.a.keys[m.Key]; ok && m.Key != "" {
		return airut.MessageRef{ID: ref}, nil
	}
	ref := r.recordLocked(Event{Kind: "reply", Text: m.Text, Private: m.Private, Files: fileNames(m.Files), ReplyTo: m.ReplyTo.ID})
	r.a.rememberLocked(m.Key, ref)
	return airut.MessageRef{ID: ref}, nil
}

// rememberLocked notes the ref a keyed message was sent as; the caller
// holds the adapter's lock.
func (a *Adapter) rememberLocked(key, ref string) {
	if key == "" {
		return
	}
	if a.keys == nil {
		a.keys = map[string]string{}
	}
	a.keys[key] = ref
}

func (r *responder) Edit(_ context.Context, ref airut.MessageRef, m airut.Message) error {
	r.record(Event{Kind: "edit", Ref: ref.ID, Text: m.Text})
	return nil
}

func (r *responder) React(_ context.Context, ref airut.MessageRef, emoji string) error {
	r.record(Event{Kind: "react", Ref: ref.ID, Text: emoji})
	return nil
}

func (r *responder) Typing(context.Context) error {
	r.record(Event{Kind: "typing"})
	return nil
}

func (r *responder) Ask(ctx context.Context, q airut.Ask) (string, error) {
	r.record(Event{Kind: "ask", Text: q.Question, Choices: q.Choices})
	r.a.mu.Lock()
	f := r.a.answer
	r.a.mu.Unlock()
	if f != nil {
		return f(q)
	}
	<-ctx.Done()
	return "", ctx.Err()
}

// Thread opens a thread whose id is its name, as its events record.
func (r *responder) Thread(_ context.Context, name string) (airut.Responder, string, error) {
	r.record(Event{Kind: "thread", Text: name})
	return &responder{a: r.a, thread: name}, name, nil
}

// Resolve resolves "@id" to a User, "#id" to a Channel, and "&id" to a
// Role, on the adapter's platform.
func (r *responder) Resolve(_ context.Context, kind airut.Kind, token string) (any, error) {
	p := r.a.platform
	sigil := map[airut.Kind]string{airut.KindUser: "@", airut.KindChannel: "#", airut.KindRole: "&"}[kind]
	id, ok := strings.CutPrefix(token, sigil)
	if sigil == "" || !ok || id == "" {
		return nil, fmt.Errorf("airuttest: %q is not a %s", token, kind)
	}
	switch kind {
	case airut.KindUser:
		return airut.User{Platform: p, ID: id, Name: id}, nil
	case airut.KindChannel:
		return airut.Channel{Platform: p, ID: id, Name: id}, nil
	default:
		return airut.Role{Platform: p, ID: id, Name: id}, nil
	}
}

// Step is one thing the fake agent does: say Text, or call the tool
// named Call with the JSON arguments Args.
type Step struct {
	Text string
	Call string
	Args string
}

// Agent is a scripted agent. It plays Steps in order, may call only the
// tools its Turn names, and records each Turn and tool result.
type Agent struct {
	Steps []Step
	// Err, when set, is returned after the steps run.
	Err error

	mu      sync.Mutex
	turns   []airut.Turn
	results []string
}

// Run plays the script.
func (g *Agent) Run(c *airut.Ctx, t airut.Turn, out airut.TurnOutput) error {
	g.mu.Lock()
	g.turns = append(g.turns, t)
	g.mu.Unlock()
	for _, s := range g.Steps {
		if s.Text != "" {
			out.Text(s.Text)
		}
		if s.Call == "" {
			continue
		}
		var res string
		var tool airut.Tool
		for _, tl := range t.Tools {
			if tl.Name() == s.Call {
				tool = tl
			}
		}
		if tool == nil {
			res = "refused: the turn does not name tool " + s.Call
		} else {
			out.ToolCall(s.Call)
			var err error
			res, err = tool.Call(c, json.RawMessage(s.Args))
			if err != nil {
				res = strings.TrimSpace(res + "\nerror: " + err.Error())
			}
		}
		g.mu.Lock()
		g.results = append(g.results, res)
		g.mu.Unlock()
	}
	return g.Err
}

// Turns returns the turns the agent ran.
func (g *Agent) Turns() []airut.Turn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]airut.Turn(nil), g.turns...)
}

// Results returns each tool call's result, in order.
func (g *Agent) Results() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.results...)
}

var _ airut.Agent = (*Agent)(nil)
var _ airut.Adapter = (*Adapter)(nil)
var _ airut.Conn = conn{}

// Conn returns the Conn the adapter announces to the bot. Its Send,
// Open, History, and Member are the Adapter's own; React and Unreact
// make and retract the bot's reactions, which the Adapter's React, a
// member's reaction, does not.
func (a *Adapter) Conn() airut.Conn { return conn{a} }

// conn is the Adapter as the bot's Conn. It exists because the Adapter's
// own React delivers a member's reaction to the bot.
type conn struct{ *Adapter }

// React records a reaction the bot added, and returns its ref. Reacting
// again with the same emoji while the reaction stands records nothing
// and returns the same ref, as both platforms do.
func (c conn) React(_ context.Context, ch airut.Channel, ref airut.MessageRef, emoji string) (airut.MessageRef, error) {
	a := c.Adapter
	a.mu.Lock()
	defer a.mu.Unlock()
	r := reaction{ch.ID, ref.ID, emoji}
	if own, ok := a.live[r]; ok {
		return airut.MessageRef{ID: own}, nil
	}
	a.refSeq++
	own := "reaction-" + strconv.Itoa(a.refSeq)
	if a.reactions == nil {
		a.reactions, a.live = map[string]reaction{}, map[reaction]string{}
	}
	a.reactions[own], a.live[r] = r, own
	a.events = append(a.events, Event{Kind: "react", Ref: ref.ID, Text: emoji, Channel: ch.ID, Reaction: own})
	return airut.MessageRef{ID: own}, nil
}

// Unreact records the retraction of a reaction React made in ch. Any
// other ref fails with airut.ErrNotOwnReaction, and a reaction already
// retracted records nothing.
func (c conn) Unreact(_ context.Context, ch airut.Channel, ref airut.MessageRef) error {
	a := c.Adapter
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.reactions[ref.ID]
	if !ok || r.channel != ch.ID {
		return airut.ErrNotOwnReaction
	}
	if a.live[r] != ref.ID {
		return nil
	}
	delete(a.live, r)
	a.events = append(a.events, Event{Kind: "unreact", Ref: r.target, Text: r.emoji, Channel: ch.ID, Reaction: ref.ID})
	return nil
}

// AddFile makes Conn.Open serve data for an attachment with this URL.
func (a *Adapter) AddFile(url string, data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.files == nil {
		a.files = map[string][]byte{}
	}
	a.files[url] = data
}

// AddHistory appends messages, oldest first, to the history Conn.History
// pages through for the channel with this id.
func (a *Adapter) AddHistory(channel string, msgs ...airut.MessageEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.history == nil {
		a.history = map[string][]airut.MessageEvent{}
	}
	a.history[channel] = append(a.history[channel], msgs...)
}

// SetMembers sets who Conn.Member reports as joined to the channel.
func (a *Adapter) SetMembers(channel string, users ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.members == nil {
		a.members = map[string]map[string]bool{}
	}
	set := map[string]bool{}
	for _, u := range users {
		set[u] = true
	}
	a.members[channel] = set
}

// Send records a message posted through the Conn. A repeated Key
// records nothing and returns the first message's ref.
func (a *Adapter) Send(_ context.Context, ch airut.Channel, m airut.Message) (airut.MessageRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ref, ok := a.keys[m.Key]; ok && m.Key != "" {
		return airut.MessageRef{ID: ref}, nil
	}
	a.refSeq++
	ref := "sent-" + strconv.Itoa(a.refSeq)
	a.events = append(a.events, Event{
		Kind: "send", Ref: ref, Text: m.Text, Private: m.Private, Files: fileNames(m.Files),
		ReplyTo: m.ReplyTo.ID, Channel: ch.ID,
	})
	a.rememberLocked(m.Key, ref)
	return airut.MessageRef{ID: ref}, nil
}

// Open serves a file added with AddFile.
func (a *Adapter) Open(_ context.Context, att airut.Attachment) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, ok := a.files[att.URL]
	if !ok {
		return nil, fmt.Errorf("airuttest: no file at %s", att.URL)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// History pages newest first through the messages added with AddHistory.
// A cursor is the index the next page ends before.
func (a *Adapter) History(_ context.Context, ch airut.Channel, cursor string, limit int) (airut.HistoryPage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	msgs := a.history[ch.ID]
	end := len(msgs)
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 || n > len(msgs) {
			return airut.HistoryPage{}, fmt.Errorf("airuttest: bad history cursor %q", cursor)
		}
		end = n
	}
	if limit <= 0 {
		limit = 10
	}
	start := max(end-limit, 0)
	page := airut.HistoryPage{}
	for i := end - 1; i >= start; i-- {
		page.Messages = append(page.Messages, msgs[i])
	}
	if start > 0 {
		page.Next = strconv.Itoa(start)
	}
	return page, nil
}

// FailMember makes Conn.Member fail with err for the channel, as a
// platform that cannot read a room's members does. A nil err undoes it.
func (a *Adapter) FailMember(channel string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.memberErrs == nil {
		a.memberErrs = map[string]error{}
	}
	a.memberErrs[channel] = err
}

// Member reports whether a user was set as joined to the channel.
func (a *Adapter) Member(_ context.Context, ch airut.Channel, u airut.User) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.memberErrs[ch.ID]; err != nil {
		return false, err
	}
	return a.members[ch.ID][u.ID], nil
}
