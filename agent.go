package airut

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrNoAgent is returned by an agent turn on a bot that has none.
var ErrNoAgent = errors.New("airut: no agent is configured; set Config.Agent, for example to one built with terva.sh/airut/agent")

// SessionScope says which conversation an agent turn remembers.
type SessionScope int

// The session scopes. NoSession, the zero value, starts every turn fresh.
// PerUser is one conversation with a person wherever they talk to the
// bot; PerUserChannel is one conversation with a person in one channel.
// In a direct channel every scope but NoSession is the conversation in
// that channel, so nothing said there is resumed from a group room.
//
// Conversation follows a person as someone in the room would, when the
// agent is a SessionLinker: a reply resumes what it replies to, a
// thread is one conversation, and a person who speaks again within
// History.FollowWithin goes on in the conversation they last spoke in
// in the channel, their own or a shared one; after a longer gap they
// start a new one of their own. Without a linker, and in a direct
// channel, it is PerUserChannel.
const (
	NoSession SessionScope = iota
	PerChannel
	PerThread
	PerUser
	PerUserChannel
	Conversation
)

// History bounds how much of a remembered conversation an agent turn
// sees. A zero field takes the bot's default, and a bot that sets none
// bounds nothing.
type History struct {
	// MaxMessages caps the messages a turn resumes. Every message in the
	// conversation's record counts: what each person said, each answer,
	// and each tool call and result.
	MaxMessages int
	// MaxTokens caps the tokens a turn resumes, as estimated from the
	// messages' size.
	MaxTokens int
	// MaxAge leaves out messages older than it, whatever Compaction says.
	MaxAge time.Duration
	// Compaction is what happens to what MaxMessages and MaxTokens leave
	// out. The zero value takes the bot's default, and CompactSummarize
	// when that is unset too.
	Compaction Compaction
	// RoomMessages is how many of the channel's latest messages the turn
	// sees, or of its thread's when it is in one, as quoted chat beside
	// the conversation, at most MaxRoomMessages. They are read afresh
	// for each turn and never kept in the conversation. NoRoomMessages
	// shows none, whatever the bot's default.
	RoomMessages int
	// FollowWithin is how soon a person must speak again for a
	// Conversation turn to go on with their last conversation in the
	// channel rather than start a new one. Zero takes the bot's default,
	// and DefaultFollowWithin when that is unset too.
	FollowWithin time.Duration
}

// DefaultFollowWithin is History.FollowWithin when neither the turn nor
// the bot sets it.
const DefaultFollowWithin = 15 * time.Minute

// NoRoomMessages, as History.RoomMessages, shows a turn none of the
// channel's messages.
const NoRoomMessages = -1

// MaxRoomMessages is the most of a channel's latest messages a turn may
// see. All of them together are bounded by MaxChain bytes too.
const MaxRoomMessages = 50

// Compaction says what happens to the part of a conversation that falls
// outside a turn's History.
type Compaction string

const (
	// CompactSummarize has the agent summarize what falls out, with the
	// conversation's own model, and keep the summary.
	CompactSummarize Compaction = "summarize"
	// CompactDrop forgets what falls out.
	CompactDrop Compaction = "drop"
)

// Budget caps what one agent turn may spend. A zero field takes the
// default in Config.Gate.
type Budget struct {
	// Turns caps the model requests in the turn.
	Turns int
	// Tokens caps the tokens the turn consumes.
	Tokens int
}

// Turn is one agent turn, scoped by the bot's author: the agent sees the
// prompt and persona, may call only Tools, remembers only Session, and
// stops at Budget.
type Turn struct {
	Prompt string
	// Persona names the system frame the agent package resolves.
	Persona string
	Tools   []Tool
	Session SessionScope
	// History bounds how much of Session the turn resumes.
	History History
	Budget  Budget
	// FinalOnly asks the agent to pass on only the answer: the text of
	// the turn's last model request. What the model says in a request
	// that ends in tool calls is narration, and the agent drops it
	// rather than passing it on, so it never reaches the chat, not even
	// for a moment; while tools run, the typing notice shows the turn is
	// working. The answer is passed on whole when its request ends, so
	// a streamed reply is posted once rather than growing. A turn whose
	// answer has no text passes on a notice saying so instead of
	// nothing. TurnRun.FinalOnly sets it for Text and StreamTo.
	FinalOnly bool
}

// Tool is something an agent turn may call. A Command registered with
// AsTool is one; the agent package adapts other tools to it.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	// Call runs the tool for the turn started from c and returns what
	// the model reads.
	Call(c *Ctx, args json.RawMessage) (string, error)
}

// ApprovalTool is a Tool whose calls may need the caller's approval.
// The agent asks the caller before each call for which NeedsApproval
// reports true. A Command is one; see Command.RequireApproval.
type ApprovalTool interface {
	Tool
	NeedsApproval() bool
}

var _ ApprovalTool = (*Command)(nil)

// TurnOutput receives an agent turn's output as it happens. An Agent
// calls it from one goroutine at a time.
type TurnOutput interface {
	// Text receives the next piece of the answer.
	Text(delta string)
	// ToolCall reports that the agent is calling the named tool. It ends
	// a segment of the answer: what the model says before a tool call is
	// usually narration, and what it says after the last one is the
	// answer.
	ToolCall(name string)
}

// Agent runs agent turns. terva.sh/airut/agent implements it on terva's
// engine; the core knows only this interface.
type Agent interface {
	Run(c *Ctx, t Turn, out TurnOutput) error
}

// SessionForgetter is an Agent that can clear a conversation it
// remembers. When Config.Agent implements it, the bot gains a built-in
// forget command; a command the bot registers under that name takes its
// place. Plain forget clears the caller's own conversations: their
// PerUser session and, when the agent is also a SessionPrefixForgetter,
// every PerUserChannel one. "forget channel" and "forget thread" clear
// the conversation everyone in the channel or thread shares, and only
// for a caller Config.ForgetShared admits.
type SessionForgetter interface {
	// ForgetSession deletes the session named key, as Ctx.SessionKey
	// names it. Forgetting a session that does not exist is not an error.
	ForgetSession(ctx context.Context, key string) error
}

// SessionPrefixForgetter is a SessionForgetter that can also clear every
// session whose key starts with a prefix.
type SessionPrefixForgetter interface {
	SessionForgetter
	// ForgetSessionsWithPrefix deletes every session whose key starts
	// with prefix, matched exactly, byte for byte.
	ForgetSessionsWithPrefix(ctx context.Context, prefix string) error
}

type forgetArgs struct {
	What string `airut:",optional" choices:"channel,thread" desc:"Forget this channel's or thread's shared conversation instead of yours"`
}

// forgetCommand is the built-in a SessionForgetter brings.
func forgetCommand(b *Bot, f SessionForgetter) *Command {
	c, err := newCommand(b, "forget", "Make the agent forget your conversation with it", func(c *Ctx, a forgetArgs) error {
		ctx := c.Context()
		switch a.What {
		case "":
			// The PerUser key by name: in a direct channel SessionKey
			// would give the channel's own key instead.
			if err := f.ForgetSession(ctx, c.userKey()); err != nil {
				return err
			}
			if pf, ok := f.(SessionPrefixForgetter); ok {
				if err := pf.ForgetSessionsWithPrefix(ctx, c.userKey()+"/"); err != nil {
					return err
				}
			}
			_, err := c.ReplyPrivate("The agent has forgotten your conversations with it.")
			return err
		case "thread":
			if c.inv.Thread == "" {
				return Errorf("This is not a thread.")
			}
		}
		if allow := b.cfg.ForgetShared; allow == nil || !allow(c) {
			return Errorf("You may not make the agent forget a conversation others share.")
		}
		scope := map[string]SessionScope{"channel": PerChannel, "thread": PerThread}[a.What]
		if err := f.ForgetSession(ctx, c.SessionKey(scope)); err != nil {
			return err
		}
		_, err := c.ReplyPrivate("The agent has forgotten this " + a.What + "'s conversation.")
		return err
	})
	if err != nil {
		panic("airut: the built-in forget command: " + err.Error())
	}
	return c
}

// TurnRun is an agent turn not yet started. StreamTo, Text, or RunTo
// runs it.
//
// While it runs, the turn shows a typing notice in the chat its output
// goes to, after Config.TypingDelay and renewed every
// Config.TypingEvery. The notice is paused while an Ask waits on a
// person, and taken down when the turn ends where the platform allows.
type TurnRun struct {
	c         *Ctx
	t         Turn
	finalOnly bool
	key       string // the session the turn ran in, once it has run
}

// Agent prepares an agent turn for this invocation.
func (c *Ctx) Agent(t Turn) *TurnRun { return &TurnRun{c: c, t: t} }

// FinalOnly keeps only the answer, so what the model narrates while it
// works never reaches the chat. It sets Turn.FinalOnly for Text and
// StreamTo, and so the agent leaves the narration out. For an agent
// that does not honour Turn.FinalOnly, Text still returns only the
// last segment, the text after the last tool call, and StreamTo
// replaces the reply's text with each new segment, so the reply ends
// as the answer. A turn that calls no tool is unchanged. RunTo is not
// affected: it passes the output on as it comes, and an app that wants
// only the answer from it sets Turn.FinalOnly itself.
func (r *TurnRun) FinalOnly() *TurnRun {
	r.finalOnly = true
	return r
}

// RunTo runs the turn and hands its output to out as it happens, for an
// app that presents the answer its own way. Typing shows in the chat c
// replies to.
func (r *TurnRun) RunTo(out TurnOutput) error {
	return r.run(out, r.c, nil, false)
}

// run runs the turn with out as its output and a typing keepalive in
// chat's chat. flush, if set, sends what out holds back; an Ask calls
// it so the question posts below a complete reply. finalOnly sets the
// turn's FinalOnly.
func (r *TurnRun) run(out TurnOutput, chat *Ctx, flush func(), finalOnly bool) error {
	switch {
	case r.c.capture != nil:
		return errNested
	case r.c.bot.cfg.Agent == nil:
		return ErrNoAgent
	}
	m, command := r.c.bot.metrics, r.c.metricCommand()
	g := r.c.bot.gate
	if err := g.check(r.c); err != nil {
		var rf *Refusal
		errors.As(err, &rf)
		r.c.bot.cfg.Logger.Info("agent turn refused", "invocation", r.c.inv.ID,
			"platform", string(r.c.inv.Caller.Platform), "reason", rf.Reason)
		m.Add("airut_agent_turns_total", 1, "command", command, "outcome", "refused_"+rf.Reason)
		return err
	}
	t := r.t
	t.FinalOnly = t.FinalOnly || finalOnly
	t.Budget = g.budget(t.Budget)
	t.Tools = r.c.reachable(t.Tools)
	key, how, err := r.c.sessionKey(t)
	if err != nil {
		m.Add("airut_agent_turns_total", 1, "command", command, "outcome", "session")
		return err
	}
	if how != "" {
		r.c.bot.cfg.Logger.Debug("agent turn session", "invocation", r.c.inv.ID, "by", how)
	}
	r.key = key
	// Deferred, so a turn whose agent panics is counted as one: the
	// panic goes on to the command's own recovery.
	start, outcome := time.Now(), "panic"
	defer func() {
		m.Since("airut_agent_turn_seconds", start, "command", command)
		m.Add("airut_agent_turns_total", 1, "command", command, "outcome", outcome)
	}()
	// The agent and every tool it calls share the turn's state through
	// a copy of the Ctx, so the handler's own Ctx is left as it was.
	st := &turnState{flush: flush, scope: t.Session, key: key}
	stopTyping := func() {}
	if every := r.c.bot.cfg.TypingEvery; every > 0 && chat.capture == nil {
		st.typing = startKeepalive(chat.ctx, chat.resp, r.c.bot.cfg.TypingDelay, every)
		stopped := false
		stopTyping = func() {
			if !stopped {
				stopped = true
				st.typing.stop()
			}
		}
		defer stopTyping()
	}
	tc := *r.c
	tc.turn = st
	err = r.c.bot.cfg.Agent.Run(&tc, t, out)
	r.c.follow(t, key)
	outcome = "ok"
	if err != nil {
		outcome = errorKind(err)
	}
	if k, ok := r.c.bot.cfg.Agent.(MemoryKeeper); ok && err == nil && r.c.chime == nil {
		// Someone the agent was asked to remember things about is asked
		// after their next turn, below its complete answer, so a question
		// they ignore never holds the answer back. A turn nobody asked
		// for asks nobody.
		if flush != nil {
			flush()
		}
		stopTyping()
		if aerr := r.c.bot.askPendingMemories(r.c, k, maxPendingAsks); aerr != nil && !errors.Is(aerr, ErrAskTimeout) {
			r.c.bot.cfg.Logger.Info("could not ask about pending memories", "invocation", r.c.inv.ID, "error_kind", errorKind(aerr))
		}
	}
	return err
}

// Text runs the turn and returns the whole answer, or its last segment
// under FinalOnly.
func (r *TurnRun) Text() (string, error) {
	b := &collector{finalOnly: r.finalOnly}
	err := r.run(b, r.c, nil, r.finalOnly)
	return b.String(), err
}

// StreamTo runs the turn and streams the answer into c's chat: one reply
// that is edited as the answer grows, at most once per
// Config.EditInterval. What the throttle holds back is sent when a tool
// is called and before a question is asked.
func (r *TurnRun) StreamTo(c *Ctx) error {
	s := &streamer{c: c, every: c.bot.cfg.EditInterval, now: time.Now, finalOnly: r.finalOnly,
		typing: c.bot.cfg.TypingEvery <= 0}
	err := r.run(s, c, s.sendPending, r.finalOnly)
	if ferr := s.flush(); err == nil {
		err = ferr
	}
	if s.posted {
		if lerr := r.Link(c, s.ref); lerr != nil {
			c.bot.cfg.Logger.Warn("could not link an answer to its session", "invocation", r.c.inv.ID, "error_kind", errorKind(lerr))
		}
	}
	return err
}

// Link records that ref, a message posted in c's chat with this turn's
// answer, came from the turn's session, so that a reply to it resumes
// the conversation. StreamTo links the reply it posts; an app that posts
// what Text or RunTo gave it links its own message. Link does nothing
// before the turn has run, for a turn that remembers nothing, or when
// the agent is not a SessionLinker.
func (r *TurnRun) Link(c *Ctx, ref MessageRef) error {
	l, ok := r.c.bot.cfg.Agent.(SessionLinker)
	if !ok || r.key == "" || ref.ID == "" {
		return nil
	}
	return l.LinkSession(r.c.ctx, ref, c.inv.Channel, r.key)
}

type collector struct {
	strings.Builder
	finalOnly bool
}

func (b *collector) Text(d string) { b.WriteString(d) }

func (b *collector) ToolCall(string) {
	if b.finalOnly {
		b.Reset()
	}
}

// streamer turns an agent's output into a reply and throttled edits.
// Its methods take mu, because a tool's Ask flushes it from the
// goroutine the tool runs on.
type streamer struct {
	c         *Ctx
	every     time.Duration
	now       func() time.Time
	finalOnly bool
	typing    bool // send a typing notice at each tool call; set when no keepalive runs

	mu      sync.Mutex
	buf     strings.Builder
	restart bool // the next text starts a new segment
	ref     MessageRef
	posted  bool
	sent    string // the text last sent
	last    time.Time
	err     error
}

func (s *streamer) Text(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restart {
		s.buf.Reset()
		s.restart = false
	}
	s.buf.WriteString(d)
	if s.err != nil || strings.TrimSpace(s.buf.String()) == "" {
		return
	}
	if !s.posted {
		s.post()
		return
	}
	if s.now().Sub(s.last) >= s.every {
		s.edit()
	}
}

func (s *streamer) ToolCall(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending()
	s.restart = s.finalOnly
	if s.typing && s.err == nil {
		_ = s.c.Typing()
	}
}

// sendPending sends what the throttle held back.
func (s *streamer) sendPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending()
}

// pending is sendPending with mu held.
func (s *streamer) pending() {
	if s.err != nil || strings.TrimSpace(s.buf.String()) == "" {
		return
	}
	if !s.posted {
		s.post()
		return
	}
	s.edit()
}

func (s *streamer) post() {
	s.ref, s.err = s.c.Reply(s.buf.String())
	s.posted, s.sent, s.last = s.err == nil, s.buf.String(), s.now()
}

func (s *streamer) edit() {
	text := s.buf.String()
	if text == s.sent {
		return
	}
	s.err = s.c.Edit(s.ref, text)
	s.sent, s.last = text, s.now()
}

// flush sends what the throttle held back once the turn has ended.
func (s *streamer) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending()
	return s.err
}
