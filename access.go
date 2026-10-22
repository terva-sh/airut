package airut

import "encoding/json"

// AccessCheck decides whether the caller of c may use a command. It
// reports false to refuse. An error refuses too: the core logs its kind
// and answers that the command failed, rather than that the caller is
// not allowed, which may not be true.
type AccessCheck func(c *Ctx) (bool, error)

// RestrictedTool is a Tool only some callers may reach. Before a turn
// starts, the core leaves out every tool whose Allows refuses the turn's
// caller, so the model is never offered it. A Command is one; see
// Command.RestrictTo. A Tool that does not implement this is open to
// everyone the Gate lets start a turn.
type RestrictedTool interface {
	Tool
	Allows(c *Ctx) (bool, error)
}

var _ RestrictedTool = (*Command)(nil)

// RestrictTo limits who may use the command to the callers check
// allows, whether they invoke it directly or an agent turn they started
// calls it as a tool. A turn whose caller check refuses is not offered
// the tool, and a call is checked again when it is made, because what
// check reads, such as a room's members, can change during a turn.
func (c *Command) RestrictTo(check AccessCheck) *Command {
	c.bot.mu.Lock()
	c.restrict = check
	c.bot.mu.Unlock()
	return c
}

// Allows reports whether the caller of ctx may use the command. A
// command without RestrictTo allows everyone.
func (c *Command) Allows(ctx *Ctx) (bool, error) {
	c.bot.mu.Lock()
	check := c.restrict
	c.bot.mu.Unlock()
	if check == nil {
		return true, nil
	}
	return check(ctx)
}

// refusedTo is the refusal of a command its RestrictTo check refused.
func (c *Command) refusedTo() *Refusal {
	return &Refusal{Reason: RefusedAccess, msg: "You are not allowed to use " + c.bot.cfg.Prefix + c.spec.Name + "."}
}

// Listed allows a caller whose user id, or one of whose role ids, is
// listed for their platform, or who is a member of a listed channel, as
// Gate.Access does.
func Listed(access map[Platform]Access) AccessCheck {
	return func(c *Ctx) (bool, error) {
		return access[c.Platform()].allows(c)
	}
}

// MemberOf allows a caller joined to ch, as the platform's Conn reports
// it, such as the members of a moderators' room. A caller from another
// platform than ch's is refused.
func MemberOf(ch Channel) AccessCheck {
	return func(c *Ctx) (bool, error) {
		if c.Platform() != ch.Platform {
			return false, nil
		}
		conn, err := c.Conn()
		if err != nil {
			return false, err
		}
		return conn.Member(c.Context(), ch, c.Caller())
	}
}

// AnyOf allows a caller any of checks allows. It stops at the first that
// does, and fails with the first error when none does.
func AnyOf(checks ...AccessCheck) AccessCheck {
	return func(c *Ctx) (bool, error) {
		var first error
		for _, check := range checks {
			ok, err := check(c)
			if ok && err == nil {
				return true, nil
			}
			if err != nil && first == nil {
				first = err
			}
		}
		return false, first
	}
}

// reachable returns the tools the turn's caller may reach, leaving out
// any whose check refuses or fails. A restricted tool that is not a
// Command, whose Call checks for itself, is wrapped so each call is
// checked again.
func (c *Ctx) reachable(tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if rt, ok := t.(RestrictedTool); ok {
			allowed, err := rt.Allows(c)
			if err != nil {
				c.bot.cfg.Logger.Info("tool access check failed", "invocation", c.inv.ID, "tool", t.Name(), "error_kind", errorKind(err))
			}
			if !allowed || err != nil {
				continue
			}
			if _, isCommand := t.(*Command); !isCommand {
				if _, images := t.(ImageTool); images {
					t = guardedImages{guarded{rt}}
				} else {
					t = guarded{rt}
				}
			}
		}
		out = append(out, t)
	}
	return out
}

// guarded checks a restricted tool's access again on every call, as
// Command.Call does for a command.
type guarded struct{ RestrictedTool }

func (g guarded) Call(c *Ctx, args json.RawMessage) (string, error) {
	if err := g.check(c); err != nil {
		return "", err
	}
	return g.RestrictedTool.Call(c, args)
}

// check refuses a caller the tool's access check no longer allows.
func (g guarded) check(c *Ctx) error {
	switch ok, err := g.Allows(c); {
	case err != nil:
		return err
	case !ok:
		return &Refusal{Reason: RefusedAccess, msg: "You are not allowed to use " + g.Name() + "."}
	}
	return nil
}

// NeedsApproval keeps the wrapped tool's approval, so wrapping it does
// not let its calls skip the caller's say-so.
func (g guarded) NeedsApproval() bool {
	at, ok := g.RestrictedTool.(ApprovalTool)
	return ok && at.NeedsApproval()
}

var _ ApprovalTool = guarded{}

// guardedImages is guarded for a restricted ImageTool, so wrapping it
// keeps its images.
type guardedImages struct{ guarded }

func (g guardedImages) CallImages(c *Ctx, args json.RawMessage) (ToolOutput, error) {
	if err := g.check(c); err != nil {
		return ToolOutput{}, err
	}
	return g.RestrictedTool.(ImageTool).CallImages(c, args)
}

var (
	_ ImageTool    = guardedImages{}
	_ ApprovalTool = guardedImages{}
)
