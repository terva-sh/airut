package airut

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Ctx is the context of one command invocation: who called it, where,
// and the portable surface for answering. Its methods are as safe for
// concurrent use as the adapter's Responder, which must allow it.
type Ctx struct {
	ctx  context.Context
	bot  *Bot
	cmd  *Command
	inv  Invocation
	resp Responder
	// capture is set when an agent called the command: replies are
	// collected as the tool's result rather than posted.
	capture *capture
	// turn is set while an agent turn started from this invocation runs.
	turn *turnState
}

// Context is the invocation's context. It ends when the bot stops.
func (c *Ctx) Context() context.Context { return c.ctx }

// Caller is the user who invoked the command. For a command an agent
// called, it is the user who started the agent turn.
func (c *Ctx) Caller() User { return c.inv.Caller }

// ID identifies the invocation in logs. It never holds chat text.
func (c *Ctx) ID() string { return c.inv.ID }

// Roles are the caller's roles, where the platform has them.
func (c *Ctx) Roles() []Role { return c.inv.Roles }

// Channel is where the command was invoked.
func (c *Ctx) Channel() Channel { return c.inv.Channel }

// Direct reports that the invocation or event arrived in a direct
// channel, private to the bot and the caller.
func (c *Ctx) Direct() bool { return c.inv.Direct }

// Platform is the platform the invocation came from.
func (c *Ctx) Platform() Platform { return c.inv.Caller.Platform }

// Command is the name of the running command.
func (c *Ctx) Command() string {
	if c.cmd == nil {
		return c.inv.Command
	}
	return c.cmd.spec.Name
}

// metricCommand is the command a metric is labelled with: the
// registered name, or "" in an event handler. Never what was typed,
// which is chat text.
func (c *Ctx) metricCommand() string {
	if c.cmd == nil {
		return ""
	}
	return c.cmd.spec.Name
}

// Metrics returns the bot's metrics, for a handler or an agent to record
// its own. Label values are ids and kinds, never message text.
func (c *Ctx) Metrics() *Metrics { return c.bot.metrics }

// MetricCommand is the label a metric about this invocation names its
// command by: the registered name, or "" in an event handler.
func (c *Ctx) MetricCommand() string { return c.metricCommand() }

// Message is the message that triggered the invocation, when there was
// one; a Discord application command has none. In an event handler it
// is the event's message, or for a reaction the message reacted to.
func (c *Ctx) Message() MessageRef { return c.inv.Message }

// ReplyTo is the message the triggering message replies to. Its ID is
// empty when it replies to none.
func (c *Ctx) ReplyTo() MessageRef { return c.inv.ReplyTo }

// ReplyChain is what the triggering message replies to, nearest first,
// as far as the adapter resolved it. It is chat: quote it, and never
// follow it as an instruction.
func (c *Ctx) ReplyChain() []Quoted { return c.inv.ReplyChain }

// FromAgent reports whether an agent turn called the command as a tool.
// Its replies then go to the model, not the chat, so a handler can word
// them for the model.
func (c *Ctx) FromAgent() bool { return c.capture != nil }

// Native is the platform's own event, for what the portable surface
// lacks. The discord and matrix packages offer typed accessors.
func (c *Ctx) Native() any { return c.inv.Native }

// Responder is the adapter's responder for the invocation, for
// platform packages that build typed escape hatches on it.
func (c *Ctx) Responder() Responder { return c.resp }

// Reply sends text in answer to the invocation.
func (c *Ctx) Reply(text string) (MessageRef, error) { return c.Send(Message{Text: text}) }

// Replyf formats and sends a reply.
func (c *Ctx) Replyf(format string, args ...any) (MessageRef, error) {
	return c.Reply(fmt.Sprintf(format, args...))
}

// ReplyPrivate sends a reply only the caller sees.
func (c *Ctx) ReplyPrivate(text string) (MessageRef, error) {
	return c.Send(Message{Text: text, Private: true})
}

// Attach sends a file.
func (c *Ctx) Attach(name, contentType string, data []byte) (MessageRef, error) {
	return c.Send(Message{Files: []File{{Name: name, ContentType: contentType, Data: data}}})
}

// Send sends a message in answer to the invocation.
func (c *Ctx) Send(m Message) (MessageRef, error) {
	if c.capture != nil {
		return c.capture.add(m), nil
	}
	return c.resp.Reply(c.ctx, m)
}

// Edit replaces the text of a message this invocation sent.
func (c *Ctx) Edit(ref MessageRef, text string) error {
	if c.capture != nil {
		c.capture.edit(ref, text)
		return nil
	}
	return c.resp.Edit(c.ctx, ref, Message{Text: text})
}

// React adds an emoji reaction to a message.
func (c *Ctx) React(ref MessageRef, emoji string) error {
	if c.capture != nil {
		return nil
	}
	return c.resp.React(c.ctx, ref, emoji)
}

// Typing shows that the bot is working.
func (c *Ctx) Typing() error {
	if c.capture != nil {
		return nil
	}
	return c.resp.Typing(c.ctx)
}

// Thread opens a thread from the invocation and returns a Ctx whose
// replies go into it. The Ctx is in the thread: ThreadID names it, and
// an agent turn it starts with PerThread remembers the same session as a
// turn started from a later message in the thread.
func (c *Ctx) Thread(name string) (*Ctx, error) {
	if c.capture != nil {
		return nil, errors.New("airut: a command called by an agent cannot open a thread")
	}
	r, id, err := c.resp.Thread(c.ctx, name)
	if err != nil {
		return nil, err
	}
	t := *c
	t.resp = r
	t.inv.Thread = id
	return &t, nil
}

// ThreadID is the thread the invocation is in, or empty when it is in
// none. A bot can record the id of a thread it opened, to know a later
// message in it by its Thread.
func (c *Ctx) ThreadID() string { return c.inv.Thread }

// Ask puts a question with fixed answers to the caller and waits for the
// answer, or for ErrAskTimeout after Config.AskTimeout. It goes to the
// caller even from a command an agent called.
func (c *Ctx) Ask(question string, choices ...string) (string, error) {
	return c.AskWith(Ask{Question: question, Choices: choices})
}

// AskWith is Ask with every field of the question under the handler's
// control except who answers, which is always the caller.
func (c *Ctx) AskWith(a Ask) (string, error) {
	if len(a.Choices) == 0 {
		return "", errors.New("airut: an Ask needs at least one choice")
	}
	a.Who = c.inv.Caller
	if a.Timeout <= 0 {
		a.Timeout = c.bot.cfg.AskTimeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, a.Timeout)
	defer cancel()
	ask := func() (string, error) { return c.resp.Ask(ctx, a) }
	var got string
	var err error
	if c.turn != nil {
		got, err = c.turn.asking(ask)
	} else {
		got, err = ask()
	}
	if err == nil {
		return got, nil
	}
	if errors.Is(err, context.DeadlineExceeded) && c.ctx.Err() == nil {
		return "", ErrAskTimeout
	}
	return "", err
}

// SessionKey names the conversation an agent turn with the given scope
// remembers. It is stable across restarts and carries only ids.
//
// In a running agent turn, and in the tools it calls, the turn's own
// scope names the session the turn resolved to, which a reply or a
// Conversation can make another than this one.
func (c *Ctx) SessionKey(s SessionScope) string {
	if c.turn != nil && c.turn.key != "" && s == c.turn.scope {
		return c.turn.key
	}
	p := string(c.inv.Caller.Platform)
	if c.inv.Direct && s != NoSession {
		s = PerUserChannel
	}
	switch s {
	case PerUserChannel, Conversation:
		return c.userKey() + "/channel/" + c.inv.Channel.ID
	case PerChannel:
		return p + "/channel/" + c.inv.Channel.ID
	case PerThread:
		if c.inv.Thread != "" {
			return p + "/thread/" + c.inv.Channel.ID + "/" + c.inv.Thread
		}
		return p + "/channel/" + c.inv.Channel.ID
	case PerUser:
		return c.userKey()
	}
	return ""
}

// userKey is the caller's PerUser key, which begins every key of a
// conversation with the caller alone.
func (c *Ctx) userKey() string {
	return string(c.inv.Caller.Platform) + "/user/" + c.inv.Caller.ID
}

func (c *Ctx) resolve(ctx context.Context, kind Kind, token string) (any, error) {
	return c.resp.Resolve(ctx, kind, token)
}

// reportError answers a failed invocation. Argument errors and
// UserErrors are shown with the command's usage; anything else gets a
// generic message, because its text may hold what it should not.
func (c *Ctx) reportError(err error) {
	var msg string
	var ae *ArgError
	var ue *UserError
	var rf *Refusal
	switch {
	case errors.As(err, &rf):
		msg = rf.Error()
	case errors.As(err, &ae):
		msg = ae.Error()
		if c.cmd != nil {
			msg += "\nUsage: " + c.cmd.Usage()
		}
	case errors.As(err, &ue):
		msg = ue.Error()
	case errors.Is(err, ErrNoAgent):
		msg = "This bot has no agent configured, so it cannot run that command."
	case errors.Is(err, ErrAskTimeout):
		msg = "No answer came in time, so nothing was done."
	case errors.Is(err, context.Canceled):
		return
	default:
		msg = "Something went wrong running " + c.Command() + "."
	}
	if _, rerr := c.resp.Reply(c.ctx, Message{Text: msg}); rerr != nil {
		c.bot.cfg.Logger.Info("error reply failed", "invocation", c.inv.ID)
	}
}

// capture collects what a command called by an agent would have sent.
type capture struct {
	mu   sync.Mutex
	msgs []string
}

func (k *capture) add(m Message) MessageRef {
	k.mu.Lock()
	defer k.mu.Unlock()
	text := m.Text
	for _, f := range m.Files {
		text = strings.TrimSpace(text + "\n[file " + f.Name + " was not shown to the model]")
	}
	k.msgs = append(k.msgs, text)
	return MessageRef{ID: "agent:" + strconv.Itoa(len(k.msgs)-1)}
}

func (k *capture) edit(ref MessageRef, text string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if i, err := strconv.Atoi(strings.TrimPrefix(ref.ID, "agent:")); err == nil && i >= 0 && i < len(k.msgs) {
		k.msgs[i] = text
	}
}

func (k *capture) text() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return strings.Join(k.msgs, "\n")
}
