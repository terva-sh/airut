package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"terva.sh/terva/packages/core"
	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/agent/sessions"
	"terva.sh/airut/config"
)

// Options configure New.
type Options struct {
	// Clients are the model clients by provider id, and Catalog holds the
	// models they serve, as Providers builds them.
	Clients map[string]provider.Client
	Catalog provider.ModelCatalog
	// Model is the model every turn uses.
	Model config.ModelRef
	// Personas maps a Turn's Persona to its system prompt. The empty name
	// is the persona of a turn that names none. A turn naming a persona
	// that is not here fails.
	Personas map[string]string
	// Sessions keeps the conversations turns remember.
	Sessions *sessions.DB
	// Logger receives ids, counts, and stop reasons, never text. Nil
	// discards them.
	Logger *slog.Logger
}

// Agent runs airut's agent turns on terva's engine. It implements
// airut.Agent and airut.SessionForgetter.
type Agent struct {
	client   provider.Client
	catalog  provider.ModelCatalog
	model    config.ModelRef
	personas map[string]string
	db       *sessions.DB
	ownsDB   bool
	log      *slog.Logger
	locks    keyLocks
}

var (
	_ airut.Agent            = (*Agent)(nil)
	_ airut.SessionForgetter = (*Agent)(nil)
)

// New returns an Agent. It makes no network request.
func New(o Options) (*Agent, error) {
	client, ok := o.Clients[o.Model.Provider]
	switch {
	case !ok:
		return nil, fmt.Errorf("agent: no client for provider %q of model %s", o.Model.Provider, o.Model)
	case o.Catalog == nil:
		return nil, errors.New("agent: Options.Catalog is required")
	case o.Sessions == nil:
		return nil, errors.New("agent: Options.Sessions is required")
	}
	if _, err := o.Catalog.FindModel(o.Model.Provider, o.Model.Model); err != nil {
		return nil, fmt.Errorf("agent: model %s: %w", o.Model, err)
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	personas := make(map[string]string, len(o.Personas))
	for k, v := range o.Personas {
		personas[k] = v
	}
	return &Agent{
		client: client, catalog: o.Catalog, model: o.Model, personas: personas,
		db: o.Sessions, log: o.Logger,
	}, nil
}

// Open builds the agent a configuration file describes: a client for
// each provider, the default model, and the session store, which Close
// closes. It fails when the file has no [agent] table.
func Open(ctx context.Context, c *config.Config, personas map[string]string, log *slog.Logger) (*Agent, error) {
	if c.Agent == nil {
		return nil, errors.New("agent: the configuration has no [agent] table")
	}
	db, err := sessions.Open(ctx, c.Store.Path, sessions.Options{Retention: c.Store.Retention, Logger: log})
	if err != nil {
		return nil, err
	}
	clients, catalog := Providers(c)
	a, err := New(Options{
		Clients: clients, Catalog: catalog, Model: c.Agent.Model,
		Personas: personas, Sessions: db, Logger: log,
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	a.ownsDB = true
	return a, nil
}

// Close closes the session store when Open opened it.
func (a *Agent) Close() error {
	if a.ownsDB {
		return a.db.Close()
	}
	return nil
}

// ForgetSession deletes the session named key. A turn running in it
// finishes first.
func (a *Agent) ForgetSession(ctx context.Context, key string) error {
	unlock, err := a.locks.lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	return a.db.Forget(ctx, key)
}

// Run runs one agent turn started from c. The core has already checked
// the Gate and filled in the budget.
func (a *Agent) Run(c *airut.Ctx, t airut.Turn, out airut.TurnOutput) error {
	system, ok := a.personas[t.Persona]
	if !ok && t.Persona != "" {
		return fmt.Errorf("agent: no persona %q", t.Persona)
	}
	tools := make(core.Registry, len(t.Tools))
	for _, tl := range t.Tools {
		if _, dup := tools[tl.Name()]; dup {
			return fmt.Errorf("agent: the turn names tool %q twice", tl.Name())
		}
		tools[tl.Name()] = &tool{c: c, t: tl}
	}

	ctx := c.Context()
	budget := &budget{turns: t.Budget.Turns, tokens: t.Budget.Tokens}
	opts := []core.Option{
		core.WithGate(gate{c: c}),
		core.WithTools(tools),
		core.WithCatalog(a.catalog),
		core.WithProvider(a.model.Provider),
		core.WithComponent(budget),
	}
	if system != "" {
		opts = append(opts, core.WithAssembler(core.StaticSystem(system)))
	}
	var resume *core.Transcript
	if key := c.SessionKey(t.Session); key != "" {
		// One turn at a time per session, so two never resume the same
		// log and append to it out of order.
		unlock, err := a.locks.lock(ctx, key)
		if err != nil {
			return err
		}
		defer unlock()
		s, err := a.db.Session(ctx, key)
		if err != nil {
			return err
		}
		tr, err := s.Reload()
		if err != nil {
			return err
		}
		resume = &tr
		opts = append(opts, core.WithTranscriptStore(s))
	}
	eng, err := core.New(a.client, a.model.Model, opts...)
	if err != nil {
		return err
	}
	if resume != nil {
		eng.Resume(*resume)
	}
	base := tokens(eng.Cost())
	budget.used = func() int { return tokens(eng.Cost()) - base }

	steps := 0
	err = eng.Run(ctx, core.PromptInput{Text: t.Prompt}, func(e core.AgentEvent) {
		switch e := e.(type) {
		case core.EvTurnStart:
			steps = e.Step
		case core.EvTextDelta:
			out.Text(e.Delta)
		case core.EvToolCall:
			out.ToolCall(e.Name)
		}
	})
	a.log.Info("agent turn finished", "invocation", c.ID(), "platform", string(c.Platform()),
		"requests", budget.allowed(steps), "tokens", budget.used(), "stop", stopKind(err, budget.stopped))
	if err != nil {
		return err
	}
	if budget.stopped != "" {
		out.Text("\n\n(" + budget.stopped + ")")
	}
	return nil
}

func stopKind(err error, stopped string) string {
	switch {
	case err != nil:
		return "error"
	case stopped != "":
		return "budget"
	}
	return "done"
}

// tokens is what a budget counts: every token the provider reported,
// cached or not. Providers differ on whether input counts cached tokens,
// so this errs towards stopping early.
func tokens(u provider.Usage) int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// budget stops a turn before a model request it has no budget for. A
// stop lets the turn end cleanly, with what it has said kept, rather
// than cutting a response off mid-stream.
type budget struct {
	turns, tokens int
	used          func() int
	stopped       string // why the turn stopped, for the chat; empty if it did not
}

// BeforeTurn implements core.TurnFilter. step counts from one.
func (b *budget) BeforeTurn(step int) (bool, string) {
	switch {
	case b.turns > 0 && step > b.turns:
		b.stopped = fmt.Sprintf("Stopped: this turn used all %d of its model requests.", b.turns)
	case b.tokens > 0 && b.used() >= b.tokens:
		b.stopped = fmt.Sprintf("Stopped: this turn used its budget of %d tokens.", b.tokens)
	default:
		return true, ""
	}
	return false, "budget"
}

// allowed is how many of steps went to the model: the step the budget
// refused never did.
func (b *budget) allowed(steps int) int {
	if b.stopped != "" {
		return steps - 1
	}
	return steps
}

// tool adapts an airut.Tool to terva's.
type tool struct {
	c *airut.Ctx
	t airut.Tool
}

func (x *tool) Name() string            { return x.t.Name() }
func (x *tool) Description() string     { return x.t.Description() }
func (x *tool) Schema() json.RawMessage { return x.t.Schema() }

// Execute runs the tool for the turn's caller. What the model reads of a
// failure is only text meant for people, as the chat would show it: an
// error's own text can hold what neither should see.
func (x *tool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	text, err := x.t.Call(x.c, args)
	if err != nil {
		// Only when the turn itself has ended does the engine record the
		// call as aborted. A tool's own timeout is a failure like any
		// other, which the model can read and recover from.
		if ctx.Err() != nil {
			return core.ToolResult{}, err
		}
		text = strings.TrimSpace(text + "\n" + forModel(err))
		return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}, nil
	}
	if strings.TrimSpace(text) == "" {
		text = "The command finished and sent no reply."
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}}, nil
}

func forModel(err error) string {
	var ue *airut.UserError
	var ae *airut.ArgError
	var rf *airut.Refusal
	switch {
	case errors.As(err, &rf):
		return "error: the caller is not allowed to use this tool"
	case errors.As(err, &ae):
		return "error: " + ae.Error()
	case errors.As(err, &ue):
		return "error: " + ue.Error()
	case errors.Is(err, airut.ErrAskTimeout):
		return "error: the caller did not answer in time"
	}
	return "error: the command failed"
}

// gate asks the caller before each call to a tool that needs approval.
type gate struct{ c *airut.Ctx }

// maxShownArgs caps the arguments shown in an approval question.
const maxShownArgs = 400

func (g gate) CheckTool(_ context.Context, call provider.ToolCallBlock, t core.Tool) (bool, string, json.RawMessage) {
	x, ok := t.(*tool)
	if !ok {
		return false, "This tool is not one the turn named.", nil
	}
	at, ok := x.t.(airut.ApprovalTool)
	if !ok || !at.NeedsApproval() {
		return true, "", nil
	}
	args := string(call.Arguments)
	if len(args) > maxShownArgs {
		args = strings.ToValidUTF8(args[:maxShownArgs], "") + "…"
	}
	answer, err := g.c.Ask(fmt.Sprintf("The assistant wants to run %s with %s. Allow it?", call.Name, args), "yes", "no")
	switch {
	case err == nil && answer == "yes":
		return true, "", nil
	case err == nil:
		return false, "The caller declined this call, so it did not run.", nil
	case errors.Is(err, airut.ErrAskTimeout):
		return false, "The caller did not approve this call in time, so it did not run.", nil
	}
	return false, "This call could not be put to the caller, so it did not run.", nil
}

// keyLocks is a mutex per session key that a waiter can abandon.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	ch   chan struct{}
	refs int
}

func (l *keyLocks) lock(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*keyLock{}
	}
	k := l.m[key]
	if k == nil {
		k = &keyLock{ch: make(chan struct{}, 1)}
		l.m[key] = k
	}
	k.refs++
	l.mu.Unlock()

	release := func() {
		l.mu.Lock()
		if k.refs--; k.refs == 0 {
			delete(l.m, key)
		}
		l.mu.Unlock()
	}
	select {
	case k.ch <- struct{}{}:
		return func() { <-k.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}
