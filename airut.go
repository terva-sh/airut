package airut

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"terva.sh/airut/changelog"
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
	// Judge decides the judgements Ctx.Judge asks for. Without one, every
	// judgement abstains with ErrNoJudge.
	Judge Judge
	// JudgeRate caps the judgements a channel may ask for, apart from the
	// Gate's rate, which a judgement never draws on. The zero value caps
	// nothing.
	JudgeRate JudgeRate
	// Logger receives ids, kinds, and sizes, never message text. It
	// defaults to slog.Default().
	Logger *slog.Logger
	// AskTimeout is how long an Ask waits when it sets no timeout. It
	// defaults to two minutes.
	AskTimeout time.Duration
	// EditInterval is the least time between the edits that stream an
	// agent's answer into a message. It defaults to one second.
	EditInterval time.Duration
	// TypingDelay is how long an agent turn runs before it shows a
	// typing notice, so a turn that answers at once shows none. It
	// defaults to one second; a negative value shows the notice at once.
	TypingDelay time.Duration
	// TypingEvery is how often a running turn renews its typing notice.
	// It defaults to eight seconds, inside the ten a Discord notice
	// lasts. A negative value turns the keepalive off, and the notice is
	// then sent only when the agent calls a tool.
	TypingEvery time.Duration
	// MetricsAddr, if set, is the address Run serves the bot's metrics
	// on, at /metrics in the Prometheus text format, as ":9464" or
	// "127.0.0.1:9464". Run fails before it starts an adapter if it
	// cannot listen there.
	MetricsAddr string
	// SummaryEvery, if positive, is how often Run logs a summary of the
	// metrics that changed since the last one. It is off by default.
	SummaryEvery time.Duration
	// ForgetShared decides who may make the agent forget a channel's or
	// a thread's conversation with the built-in forget command, which
	// clears it for everyone in it. Nil lets nobody.
	ForgetShared func(*Ctx) bool
	// MemoryWrites says, per scope, what happens when the model asks to
	// remember something, on an Agent that is a MemoryKeeper. A scope it
	// leaves out takes the default: WriteAsk for a user, WriteAuto for a
	// channel, and WriteOff for the bot.
	MemoryWrites map[MemoryScope]MemoryWrite
	// MemoryApprover decides who may write bot memories directly and
	// remove any channel or bot memory, such as a room's moderators. Nil
	// approves nobody, so every bot memory waits for approval.
	MemoryApprover func(*Ctx) bool
	// MemoryAccess decides who may use the built-in remember, memories,
	// and unremember commands, and the remember tool, as each command's
	// RestrictTo check: a turn whose caller it refuses is not offered the
	// tool. Nil lets everyone, and a check that refuses everyone shuts
	// the commands. Restrict them where what a member writes should not
	// reach the turns of people who act on it, as with airut.Owner.
	MemoryAccess AccessCheck
	// OnPendingMemory is told of each bot memory that waits for
	// approval, so the app can route it to an approver. It runs on the
	// invocation's goroutine. An approver makes the memory active with
	// MemoryKeeper.SetMemoryStatus.
	OnPendingMemory func(*Ctx, Memory)
	// Settings configures the bot's runtime settings: who may change
	// them, the config file's values, where overrides are kept, and who
	// hears of a change. A bot with an Agent has the Gate's limits as
	// settings, and the config file's values cap them.
	Settings Settings
	// ProfileEditors decides who may use the built-in profile command,
	// which changes the bot's name, description, and picture on the
	// platform it is used on, usually airut.Owner. Nil registers no
	// profile command. The command takes an attachment, so it is never a
	// tool.
	ProfileEditors AccessCheck
	// Version is the version of the bot that is running, such as the
	// output of git describe --tags set at build time. With it set, the
	// bot has a built-in version command that answers with it.
	Version string
	// Owners lists the bot's owners, by platform: user ids, as a config
	// file's [bot] owners gives them. Owner admits them, and the built-in
	// status command tells anyone whether the bot has one. A bot with no
	// owner on any platform it serves warns at every start, and answers a
	// direct message with how to claim it.
	Owners map[Platform][]string
	// Changelog is the bot's user-facing release notes. With it set, the
	// bot has a built-in changelog command that shows, privately, the
	// notes for a version: by default the release Version is or was
	// built from.
	Changelog *Changelog
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
	conns  map[Platform]Conn
	gate   *gate
	judged *limiter // judgements per channel, under Config.JudgeRate
	// chimes are the Chime listeners, by the roomKey of each of their
	// channels. Chime fills it before Run, so it is only read after.
	chimes   map[string]*chime
	running  bool
	inflight sync.WaitGroup
	metrics  *Metrics
	// settings are the declared settings, by name, and settingsMu
	// serialises their changes, since one can be checked against another.
	settings   map[string]*setting
	settingsMu sync.Mutex
	// changelog is Config.Changelog's notes, which Run parses, and
	// announceMu serialises the announcements of them, so a reconnect
	// during one does not race it.
	changelog  *changelog.Log
	announceMu sync.Mutex
	// owners is where Owner and HasOwner read the bot's owners.
	owners *owners
	// claimTold holds the senders, by platform and id, told in a direct
	// message during this Run that the bot has no owner.
	claimTold map[string]bool
}

// New returns a Bot with no commands. It panics on a Gate whose limits
// make no sense, such as a rate limit with no window. With an Agent, the
// bot has the Gate's limits as settings.
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
	if cfg.TypingDelay == 0 {
		cfg.TypingDelay = defaultTypingDelay
	}
	if cfg.TypingEvery == 0 {
		cfg.TypingEvery = defaultTypingEvery
	}
	if cfg.JudgeRate.PerChannel < 0 || cfg.JudgeRate.PerChannel > 0 && cfg.JudgeRate.Window <= 0 {
		panic("airut: JudgeRate needs a positive Window and a PerChannel that is not negative")
	}
	for s, w := range cfg.MemoryWrites {
		if s != UserMemory && s != ChannelMemory && s != BotMemory || w != WriteOff && w != WriteAsk && w != WriteAuto {
			panic(fmt.Sprintf("airut: MemoryWrites[%q] = %q is not a scope and a write mode", s, w))
		}
	}
	b := &Bot{cfg: cfg, commands: map[string]*Command{}, gate: newGate(cfg.Gate), metrics: newMetrics(),
		judged: &limiter{now: time.Now, seen: map[string][]time.Time{}}, owners: newOwners(cfg.Owners, claimsOf(cfg.Agent), cfg.Logger)}
	if cfg.Agent != nil {
		b.gate.knobs = b.declareGateSettings(cfg.Gate)
	}
	return b
}

// Metrics returns the bot's metrics: those the core records, and any
// the bot records itself.
func (b *Bot) Metrics() *Metrics { return b.metrics }

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
//	airut:",optional,named"     set in text only as name=value or --name
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
	if err := b.parseChangelog(); err != nil {
		b.mu.Unlock()
		return err
	}
	b.running = true
	if f, ok := b.cfg.Agent.(SessionForgetter); ok && b.commands["forget"] == nil {
		b.commands["forget"] = forgetCommand(b, f)
	}
	if k, ok := b.cfg.Agent.(MemoryKeeper); ok {
		for name, mk := range map[string]func(*Bot, MemoryKeeper) *Command{
			"remember": rememberCommand, "memories": memoriesCommand, "unremember": unrememberCommand,
		} {
			if b.commands[name] == nil {
				c := mk(b, k)
				// b.mu is held, so the field is set as RestrictTo would.
				c.restrict = b.cfg.MemoryAccess
				b.commands[name] = c
			}
		}
	}
	if len(b.settings) > 0 {
		for name, mk := range map[string]func(*Bot) *Command{
			"settings": settingsCommand, "set": setCommand, "unset": unsetCommand,
		} {
			if b.commands[name] == nil {
				b.commands[name] = mk(b)
			}
		}
	}
	if b.cfg.ProfileEditors != nil && b.commands["profile"] == nil {
		c := profileCommand(b)
		// b.mu is held, so the field is set as RestrictTo would.
		c.restrict = b.cfg.ProfileEditors
		b.commands["profile"] = c
	}
	if strings.TrimSpace(b.cfg.Version) != "" && b.commands["version"] == nil {
		b.commands["version"] = versionCommand(b)
	}
	if b.changelog != nil && b.commands["changelog"] == nil {
		b.commands["changelog"] = changelogCommand(b)
	}
	if b.commands["status"] == nil {
		b.commands["status"] = statusCommand(b)
	}
	b.claimTold = map[string]bool{}
	b.mu.Unlock()

	// stopped lets the bot be run again when Run fails before anything
	// started.
	stopped := func(err error) error {
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		return err
	}
	if err := b.startSettings(ctx); err != nil {
		return stopped(err)
	}
	platforms := make([]Platform, len(adapters))
	for i, a := range adapters {
		platforms[i] = a.Platform()
	}
	b.warnUnowned(ctx, platforms)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	if b.cfg.MetricsAddr != "" {
		stop, err := b.serveMetrics(b.cfg.MetricsAddr)
		if err != nil {
			return stopped(err)
		}
		defer stop()
	}
	if b.cfg.SummaryEvery > 0 {
		wg.Go(func() { b.summarize(ctx, b.cfg.SummaryEvery) })
	}
	h := host{b}
	errs := make([]error, len(adapters))
	var adapterWG sync.WaitGroup
	for i, a := range adapters {
		platform := string(a.Platform())
		b.metrics.Set("airut_adapter_up", 1, "platform", platform)
		adapterWG.Go(func() {
			err := a.Run(ctx, h)
			b.metrics.Set("airut_adapter_up", 0, "platform", platform)
			if err != nil && ctx.Err() == nil {
				errs[i] = fmt.Errorf("airut: %s adapter: %w", a.Platform(), err)
				b.cfg.Logger.Error("adapter stopped", "platform", platform)
			}
			cancel()
		})
	}
	adapterWG.Wait()
	b.inflight.Wait()
	cancel()
	wg.Wait()
	if b.cfg.SummaryEvery > 0 {
		// The last summary, with everything that finished while Run
		// waited.
		b.metrics.logSummary(b.cfg.Logger)
	}
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
		// The name is whatever was typed, so only its size is logged, and
		// it is no metric's label.
		log.Info("unknown command", "name_bytes", len(inv.Command))
		b.metrics.Add("airut_commands_total", 1, "command", "", "outcome", "unknown")
		c.reportError(Errorf("There is no command %s%s.", b.cfg.Prefix, inv.Command))
		return
	}
	log = log.With("command", cmd.spec.Name)
	outcome := func(o string) { b.metrics.Add("airut_commands_total", 1, "command", cmd.spec.Name, "outcome", o) }
	switch ok, err := cmd.Allows(c); {
	case err != nil:
		log.Info("command access check failed", "kind", errorKind(err))
		outcome("access_check_failed")
		c.reportError(err)
		return
	case !ok:
		log.Info("command refused", "reason", RefusedAccess)
		outcome("refused_" + RefusedAccess)
		c.reportError(cmd.refusedTo())
		return
	}
	start := time.Now()
	raw, err := cmd.dispatchArgs(inv)
	if err == nil {
		err = cmd.invoke(c, raw, inv.Files)
	}
	b.metrics.Since("airut_command_seconds", start, "command", cmd.spec.Name)
	if err == nil {
		log.Debug("command finished")
		outcome("ok")
		return
	}
	outcome(errorKind(err))
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
