package airut

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrNoAgent is returned by an agent turn on a bot that has none.
var ErrNoAgent = errors.New("airut: no agent is configured; set Config.Agent, for example to one built with terva.sh/airut/agent")

// SessionScope says which conversation an agent turn remembers.
type SessionScope int

// The session scopes. NoSession, the zero value, starts every turn fresh.
const (
	NoSession SessionScope = iota
	PerChannel
	PerThread
	PerUser
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
	Budget  Budget
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
	// ToolCall reports that the agent is calling the named tool.
	ToolCall(name string)
}

// Agent runs agent turns. terva.sh/airut/agent implements it on terva's
// engine; the core knows only this interface.
type Agent interface {
	Run(c *Ctx, t Turn, out TurnOutput) error
}

// SessionForgetter is an Agent that can clear a conversation it
// remembers. When Config.Agent implements it, the bot gains a built-in
// forget command that clears the caller's own PerUser session; a command
// the bot registers under that name takes its place. Clearing a channel
// or thread session would clear it for everyone in it, so no built-in
// does.
type SessionForgetter interface {
	// ForgetSession deletes the session named key, as Ctx.SessionKey
	// names it. Forgetting a session that does not exist is not an error.
	ForgetSession(ctx context.Context, key string) error
}

// forgetCommand is the built-in a SessionForgetter brings.
func forgetCommand(b *Bot, f SessionForgetter) *Command {
	c, err := newCommand(b, "forget", "Make the agent forget your conversation with it", func(c *Ctx) error {
		if err := f.ForgetSession(c.Context(), c.SessionKey(PerUser)); err != nil {
			return err
		}
		_, err := c.ReplyPrivate("The agent has forgotten your conversation with it.")
		return err
	})
	if err != nil {
		panic("airut: the built-in forget command: " + err.Error())
	}
	return c
}

// TurnRun is an agent turn not yet started. StreamTo or Text runs it.
type TurnRun struct {
	c *Ctx
	t Turn
}

// Agent prepares an agent turn for this invocation.
func (c *Ctx) Agent(t Turn) *TurnRun { return &TurnRun{c: c, t: t} }

func (r *TurnRun) run(out TurnOutput) error {
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
	t.Budget = g.budget(t.Budget)
	t.Tools = r.c.reachable(t.Tools)
	// Deferred, so a turn whose agent panics is counted as one: the
	// panic goes on to the command's own recovery.
	start, outcome := time.Now(), "panic"
	defer func() {
		m.Since("airut_agent_turn_seconds", start, "command", command)
		m.Add("airut_agent_turns_total", 1, "command", command, "outcome", outcome)
	}()
	err := r.c.bot.cfg.Agent.Run(r.c, t, out)
	outcome = "ok"
	if err != nil {
		outcome = errorKind(err)
	}
	return err
}

// Text runs the turn and returns the whole answer.
func (r *TurnRun) Text() (string, error) {
	var b collector
	err := r.run(&b)
	return b.String(), err
}

// StreamTo runs the turn and streams the answer into c's chat: one reply
// that is edited as the answer grows, at most once per
// Config.EditInterval, with a typing notice while tools run.
func (r *TurnRun) StreamTo(c *Ctx) error {
	s := &streamer{c: c, every: c.bot.cfg.EditInterval, now: time.Now}
	err := r.run(s)
	if ferr := s.flush(); err == nil {
		err = ferr
	}
	return err
}

type collector struct{ strings.Builder }

func (b *collector) Text(d string)   { b.WriteString(d) }
func (b *collector) ToolCall(string) {}

// streamer turns an agent's output into a reply and throttled edits.
type streamer struct {
	c     *Ctx
	every time.Duration
	now   func() time.Time

	buf    strings.Builder
	ref    MessageRef
	posted bool
	sent   string // the text last sent
	last   time.Time
	err    error
}

func (s *streamer) Text(d string) {
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
	if s.err == nil {
		_ = s.c.Typing()
	}
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

// flush sends what the throttle held back.
func (s *streamer) flush() error {
	if s.err != nil {
		return s.err
	}
	if !s.posted {
		if strings.TrimSpace(s.buf.String()) != "" {
			s.post()
		}
		return s.err
	}
	s.edit()
	return s.err
}
