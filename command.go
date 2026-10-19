package airut

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
)

// Command is a registered command. One definition is a Discord
// application command, a text command, and, after AsTool, a tool an
// agent turn may call.
type Command struct {
	bot    *Bot
	spec   Spec
	args   reflect.Type // nil for a command without parameters
	fields []field
	fn     reflect.Value
	tool   bool
	// approve makes every call from an agent turn wait on the caller.
	approve bool
	// restrict, when set, decides who may use the command.
	restrict AccessCheck
	schema   json.RawMessage
}

// AsTool marks the command as one an agent turn may call, once a Turn
// names it through Bot.Tools. It panics on a command with an attachment
// parameter, which a model has no way to supply.
func (c *Command) AsTool() *Command {
	for _, f := range c.fields {
		if f.Kind == KindAttachment {
			panic("airut: command " + c.spec.Name + ": a command with an attachment parameter cannot be a tool")
		}
	}
	c.bot.mu.Lock()
	c.tool = true
	c.bot.mu.Unlock()
	return c
}

// RequireApproval makes every call to the command from an agent turn
// wait until the caller approves it in the chat. The agent is told the
// call was refused on a no or when no answer comes in time.
func (c *Command) RequireApproval() *Command {
	c.bot.mu.Lock()
	c.approve = true
	c.bot.mu.Unlock()
	return c
}

// NeedsApproval reports whether RequireApproval was called. With it a
// Command is an ApprovalTool.
func (c *Command) NeedsApproval() bool {
	c.bot.mu.Lock()
	defer c.bot.mu.Unlock()
	return c.approve
}

// Spec describes the command for an adapter to register.
func (c *Command) Spec() Spec {
	s := c.spec
	s.Params = slices.Clone(s.Params)
	return s
}

// Usage is the command's text grammar, such as "!roll <dice> [private]".
func (c *Command) Usage() string { return usage(c.bot.cfg.Prefix, c.spec) }

// Name is the command's name. With Description, Schema, and Call it
// makes a Command a Tool.
func (c *Command) Name() string { return c.spec.Name }

// Description is the command's one-line description.
func (c *Command) Description() string { return c.spec.Description }

// Schema is the JSON Schema of the command's arguments.
func (c *Command) Schema() json.RawMessage { return slices.Clone(c.schema) }

// Call runs the command for an agent turn started from parent. The
// command sees a Ctx marked FromAgent, and everything it would reply is
// returned as the tool's result instead of being posted; the model
// decides what the person sees. An Ask still goes to the caller. A call
// from a caller RestrictTo refuses returns a Refusal.
func (c *Command) Call(parent *Ctx, args json.RawMessage) (string, error) {
	switch ok, err := c.Allows(parent); {
	case err != nil:
		return "", err
	case !ok:
		return "", c.refusedTo()
	}
	raw, err := decodeJSON(args)
	if err != nil {
		return "", err
	}
	child := &Ctx{
		ctx: parent.ctx, bot: c.bot, cmd: c, inv: parent.inv, resp: parent.resp,
		capture: &capture{},
	}
	err = c.invoke(child, raw, nil)
	return child.capture.text(), err
}

// invoke binds raw arguments and runs the handler, turning a panic in
// either into an error.
func (c *Command) invoke(ctx *Ctx, raw map[string]any, files []Attachment) (err error) {
	defer func() {
		if e := recovered(recover()); e != nil {
			err = e
		}
	}()
	in := []reflect.Value{reflect.ValueOf(ctx)}
	if c.args != nil {
		v, err := bind(ctx.Context(), c.args, c.fields, raw, files, ctx.resolve)
		if err != nil {
			return err
		}
		in = append(in, v)
	} else if len(raw) > 0 {
		return &ArgError{Msg: "this command takes no arguments"}
	}
	out := c.fn.Call(in)
	if e, _ := out[0].Interface().(error); e != nil {
		return e
	}
	return nil
}

// dispatchArgs turns an invocation's arguments into raw values: its
// structured values when the platform typed them, or its parsed text.
func (c *Command) dispatchArgs(inv Invocation) (map[string]any, error) {
	if inv.Values != nil {
		return inv.Values, nil
	}
	if c.args == nil {
		if strings.TrimSpace(inv.Text) != "" {
			return nil, &ArgError{Msg: "this command takes no arguments"}
		}
		return nil, nil
	}
	return parseText(inv.Text, c.fields)
}

var errNested = errors.New("airut: an agent turn cannot start another agent turn")
