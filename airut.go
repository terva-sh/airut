package airut

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// Config configures a Bot.
type Config struct {
	// Prefix triggers a text command, as in "!roll 2d6". It defaults to
	// "!".
	Prefix string
	// Agent runs agent turns. Without one, Ctx.Agent returns ErrNoAgent.
	Agent Agent
	// Gate says who may start an agent turn, how often, and the budget of
	// a turn that sets none. The zero Gate refuses every turn.
	Gate Gate
	// Logger receives ids, kinds, and sizes, never message text. It
	// defaults to slog.Default().
	Logger *slog.Logger
	// AskTimeout is how long an Ask waits when it sets no timeout. It
	// defaults to two minutes.
	AskTimeout time.Duration
	// EditInterval is the least time between the edits that stream an
	// agent's answer into a message. It defaults to one second.
	EditInterval time.Duration
}

// Bot holds a set of commands and serves them through adapters.
type Bot struct {
	cfg      Config
	mu       sync.Mutex
	commands map[string]*Command
	// onMessage and onReaction are the event handlers, in registration
	// order.
	onMessage  []eventHandler
	onReaction []eventHandler
	onReady    []func(context.Context, Conn)
	// conns are the connections adapters announced, by platform.
	conns    map[Platform]Conn
	gate     *gate
	running  bool
	inflight sync.WaitGroup
}

// New returns a Bot with no commands. It panics on a Gate whose limits
// make no sense, such as a rate limit with no window.
func New(cfg Config) *Bot {
	if cfg.Prefix == "" {
		cfg.Prefix = "!"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.AskTimeout <= 0 {
		cfg.AskTimeout = 2 * time.Minute
	}
	if cfg.EditInterval <= 0 {
		cfg.EditInterval = time.Second
	}
	return &Bot{cfg: cfg, commands: map[string]*Command{}, gate: newGate(cfg.Gate)}
}

var (
	ctxType   = reflect.TypeFor[*Ctx]()
	errorType = reflect.TypeFor[error]()
)

// Command registers a command. handler is a func(*Ctx, A) error, where
// the fields of the struct A are the command's parameters, or a
// func(*Ctx) error for a command with none.
//
// A field's parameter name comes from its airut tag, or from the field
// name in snake_case, and its description from its desc tag. Tags:
//
//	airut:"name,optional,rest"  name, and options; "-" skips the field
//	desc:"..."                  required, 1 to 100 characters
//	choices:"a,b,c"             allowed values of a string
//	min:"1" max:"6"             bounds of a number, or of a string's length
//
// A field is a string, bool, int, int64, float64, User, Channel, Role,
// or Attachment. A pointer field is optional. Required parameters come
// before optional ones, and only a final string can be rest.
//
// Command panics on a definition no platform would accept, naming the
// command and the field, so a mistake shows on the bot's first start.
// It also panics once the bot is running.
func (b *Bot) Command(name, description string, handler any) *Command {
	c, err := newCommand(b, name, description, handler)
	if err != nil {
		panic(fmt.Sprintf("airut: command %q: %v", name, err))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		panic(fmt.Sprintf("airut: command %q: registered after Run", name))
	}
	if _, dup := b.commands[name]; dup {
		panic(fmt.Sprintf("airut: command %q: registered twice", name))
	}
	b.commands[name] = c
	return c
}

func newCommand(b *Bot, name, description string, handler any) (*Command, error) {
	if !validName(name) {
		return nil, fmt.Errorf("a command name must match %s", nameRE)
	}
	if err := checkDescription("the command", description); err != nil {
		return nil, err
	}
	fn := reflect.ValueOf(handler)
	ft := fn.Type()
	if fn.Kind() != reflect.Func || ft.NumIn() < 1 || ft.NumIn() > 2 || ft.In(0) != ctxType ||
		ft.NumOut() != 1 || ft.Out(0) != errorType || (ft.NumIn() == 2 && ft.In(1).Kind() != reflect.Struct) {
		return nil, fmt.Errorf("the handler must be a func(*airut.Ctx, A) error with A a struct, or a func(*airut.Ctx) error; it is %s", ft)
	}
	c := &Command{bot: b, fn: fn, spec: Spec{Name: name, Description: description}}
	if ft.NumIn() == 2 {
		c.args = ft.In(1)
		fields, err := deriveParams(c.args)
		if err != nil {
			return nil, err
		}
		c.fields = fields
		for _, f := range fields {
			c.spec.Params = append(c.spec.Params, f.Param)
		}
	}
	c.schema = jsonSchema(c.spec)
	return c, nil
}

// Tools returns the named commands as tools for a Turn. It panics on a
// name that was not registered with AsTool, because handing the agent a
// command its author did not mark is the mistake this guards.
func (b *Bot) Tools(names ...string) []Tool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		c := b.commands[n]
		if c == nil || !c.tool {
			panic(fmt.Sprintf("airut: Tools(%q): no command of that name is registered with AsTool", n))
		}
		out = append(out, c)
	}
	return out
}

// Run serves the bot through the adapters until ctx ends or one of them
// fails, then stops the rest and waits for invocations in flight. It
// returns nil after ctx ends, or the adapters' errors.
func (b *Bot) Run(ctx context.Context, adapters ...Adapter) error {
	if len(adapters) == 0 {
		return errors.New("airut: Run needs at least one adapter")
	}
	seen := map[Platform]bool{}
	for _, a := range adapters {
		if seen[a.Platform()] {
			return fmt.Errorf("airut: two adapters for platform %s", a.Platform())
		}
		seen[a.Platform()] = true
	}
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return errors.New("airut: Run called twice")
	}
	b.running = true
	if f, ok := b.cfg.Agent.(SessionForgetter); ok && b.commands["forget"] == nil {
		b.commands["forget"] = forgetCommand(b, f)
	}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := host{b}
	errs := make([]error, len(adapters))
	var wg sync.WaitGroup
	for i, a := range adapters {
		wg.Go(func() {
			err := a.Run(ctx, h)
			if err != nil && ctx.Err() == nil {
				errs[i] = fmt.Errorf("airut: %s adapter: %w", a.Platform(), err)
				b.cfg.Logger.Error("adapter stopped", "platform", string(a.Platform()))
			}
			cancel()
		})
	}
	wg.Wait()
	b.inflight.Wait()
	return errors.Join(errs...)
}

// host is the Host a running Bot gives its adapters.
type host struct{ b *Bot }

func (h host) Prefix() string       { return h.b.cfg.Prefix }
func (h host) Logger() *slog.Logger { return h.b.cfg.Logger }

func (h host) Specs() []Spec {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	out := make([]Spec, 0, len(h.b.commands))
	for _, c := range h.b.commands {
		out = append(out, c.Spec())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h host) Deliver(ctx context.Context, ev Event, r Responder) { h.b.deliver(ctx, ev, r) }
func (h host) Ready(ctx context.Context, c Conn)                  { h.b.ready(ctx, c) }

func (h host) Dispatch(ctx context.Context, inv Invocation) {
	b := h.b
	b.inflight.Add(1)
	defer b.inflight.Done()
	log := b.cfg.Logger.With("invocation", inv.ID, "platform", string(inv.Caller.Platform))

	b.mu.Lock()
	cmd := b.commands[inv.Command]
	b.mu.Unlock()
	c := &Ctx{ctx: ctx, bot: b, cmd: cmd, inv: inv, resp: inv.Respond}
	if cmd == nil {
		// The name is whatever was typed, so only its size is logged.
		log.Info("unknown command", "name_bytes", len(inv.Command))
		c.reportError(Errorf("There is no command %s%s.", b.cfg.Prefix, inv.Command))
		return
	}
	log = log.With("command", cmd.spec.Name)
	switch ok, err := cmd.Allows(c); {
	case err != nil:
		log.Info("command access check failed", "kind", errorKind(err))
		c.reportError(err)
		return
	case !ok:
		log.Info("command refused", "reason", RefusedAccess)
		c.reportError(cmd.refusedTo())
		return
	}
	raw, err := cmd.dispatchArgs(inv)
	if err == nil {
		err = cmd.invoke(c, raw, inv.Files)
	}
	if err == nil {
		log.Debug("command finished")
		return
	}
	var pe panicError
	if errors.As(err, &pe) {
		log.Error("command panicked", "stack", string(pe.stack))
	} else {
		log.Info("command failed", "kind", errorKind(err))
	}
	c.reportError(err)
}

// UserError is an error whose text is meant for the person who invoked
// the command. A handler returns one with Errorf.
type UserError struct{ msg string }

func (e *UserError) Error() string { return e.msg }

// Errorf returns an error that the core shows to the caller as it is.
// Other errors a handler returns are logged by kind and answered with a
// generic message, so their text never reaches the chat.
func Errorf(format string, args ...any) error {
	return &UserError{msg: fmt.Sprintf(format, args...)}
}

// panicError carries a recovered handler panic.
type panicError struct{ stack []byte }

func (panicError) Error() string { return "the command panicked" }

func recovered(v any) error {
	if v == nil {
		return nil
	}
	return panicError{stack: debug.Stack()}
}

// errorKind names an error's class for a log, without its text, which can
// carry chat content.
func errorKind(err error) string {
	var ae *ArgError
	var ue *UserError
	var pe panicError
	var rf *Refusal
	switch {
	case errors.As(err, &ae):
		return "arguments"
	case errors.As(err, &rf):
		return "refused_" + rf.Reason
	case errors.As(err, &ue):
		return "user"
	case errors.As(err, &pe):
		return "panic"
	case errors.Is(err, ErrNoAgent):
		return "no_agent"
	case errors.Is(err, ErrAskTimeout):
		return "ask_timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "handler"
}
