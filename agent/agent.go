package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

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
	// Models is the chain of models a turn walks, as config.Load resolves
	// one. A turn starts on the first entry Health finds usable, and
	// falls through to the next usable one when its first request fails
	// before anything streamed, by a connect error, a 5xx, a 429, a
	// timeout, or a missing model. Each entry runs under its served id,
	// with its MaxOutput, Temperature, and Timeout. Empty means Model
	// alone.
	Models config.Chain
	// Model is the model every turn uses when Models is empty.
	Model config.ModelRef
	// Health says which entries of Models are usable now. Nil means one
	// of the agent's own that counts every provider as available and
	// keeps breakers with the defaults.
	Health *Health
	// Personas maps a Turn's Persona to its system prompt. The empty name
	// is the persona of a turn that names none. A turn naming a persona
	// that is not here fails.
	Personas map[string]string
	// Character is who the bot is and how it speaks: its voice, its
	// temperament, how it addresses people. Every turn's system prompt
	// opens with it, whatever the persona, and the persona's prompt
	// follows, so the rules and tools a persona sets are read last. A
	// judge does not take it. Empty means none.
	Character string
	// Sessions keeps the conversations turns remember, and the bot's
	// long-term memories.
	Sessions *sessions.DB
	// MemoryCaps cap the active memories per scope. A zero field takes
	// its default.
	MemoryCaps MemoryCaps
	// History bounds what a turn resumes in each field the turn's own
	// History leaves zero. The zero value bounds nothing.
	History airut.History
	// Notices are what a turn tells the chat when it ends without a whole
	// answer.
	Notices Notices
	// Now is the clock MaxAge is measured by, for tests. Nil means
	// time.Now.
	Now func() time.Time
	// Logger receives ids, counts, and stop reasons, never text. Nil
	// discards them.
	Logger *slog.Logger
}

// Agent runs airut's agent turns on terva's engine. It implements
// airut.Agent and airut.SessionPrefixForgetter.
type Agent struct {
	clients  map[string]provider.Client
	catalog  provider.ModelCatalog
	chain    config.Chain
	health   *Health
	personas map[string]string
	// character opens every turn's system prompt.
	character string
	notices   Notices
	db        *sessions.DB
	history   airut.History
	caps      MemoryCaps
	now       func() time.Time
	ownsDB    bool
	log       *slog.Logger
	locks     keyLocks
}

var (
	_ airut.Agent                  = (*Agent)(nil)
	_ airut.SessionPrefixForgetter = (*Agent)(nil)
	_ airut.SessionLinker          = (*Agent)(nil)
)

// New returns an Agent. It makes no network request.
func New(o Options) (*Agent, error) {
	switch {
	case o.Catalog == nil:
		return nil, errors.New("agent: Options.Catalog is required")
	case o.Sessions == nil:
		return nil, errors.New("agent: Options.Sessions is required")
	}
	chain := slices.Clone(o.Models)
	if len(chain) == 0 {
		chain = config.Chain{{Provider: o.Model.Provider, Model: o.Model.Model, Served: o.Model.Model}}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	clients := map[string]provider.Client{}
	for i := range chain {
		e := &chain[i]
		if e.Served == "" {
			e.Served = e.Model
		}
		ref := config.ModelRef{Provider: e.Provider, Model: e.Served}
		client, ok := o.Clients[e.Provider]
		if !ok {
			return nil, fmt.Errorf("agent: no client for provider %q of model %s", e.Provider, ref)
		}
		clients[e.Provider] = client
		m, err := o.Catalog.FindModel(e.Provider, e.Served)
		if err != nil {
			return nil, fmt.Errorf("agent: model %s: %w", ref, err)
		}
		if e.MaxOutput == 0 && m.MaxOutput == 0 {
			// No max_tokens is sent, so the server's own default caps
			// every answer, and on a llama.cpp server that is 1024.
			o.Logger.Warn("agent model sets no output limit; the server's default applies",
				"provider", e.Provider, "model", e.Model)
		}
	}
	if o.Health == nil {
		o.Health = NewHealth(HealthOptions{Logger: o.Logger})
	}
	if err := checkHistory(o.History); err != nil {
		return nil, fmt.Errorf("agent: Options.History: %w", err)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MemoryCaps.User < 0 || o.MemoryCaps.Channel < 0 || o.MemoryCaps.Bot < 0 {
		return nil, errors.New("agent: Options.MemoryCaps cannot be negative")
	}
	caps := o.MemoryCaps
	if caps.User == 0 {
		caps.User = DefaultMaxUserMemories
	}
	if caps.Channel == 0 {
		caps.Channel = DefaultMaxChannelMemories
	}
	if caps.Bot == 0 {
		caps.Bot = DefaultMaxBotMemories
	}
	personas := make(map[string]string, len(o.Personas))
	for k, v := range o.Personas {
		personas[k] = v
	}
	return &Agent{
		clients: clients, catalog: o.Catalog, chain: chain, health: o.Health, personas: personas, character: o.Character,
		db: o.Sessions, history: o.History, caps: caps, now: o.Now, log: o.Logger, notices: o.Notices.withDefaults(),
	}, nil
}

// OpenOption sets an Option that Open does not read from the file.
type OpenOption func(*Options)

// WithCharacter sets Options.Character.
func WithCharacter(character string) OpenOption {
	return func(o *Options) { o.Character = character }
}

// WithNotices sets Options.Notices.
func WithNotices(n Notices) OpenOption {
	return func(o *Options) { o.Notices = n }
}

// WithHealth sets Options.Health, so that the agent shares h with a judge
// and each skips a model the other saw fail.
func WithHealth(h *Health) OpenOption {
	return func(o *Options) { o.Health = h }
}

// Open builds the agent a configuration file describes: a client for
// each provider, the chain of models, a Health over the file's providers,
// and the session store, which Close closes. It fails when the file has
// no [agent] table.
func Open(ctx context.Context, c *config.Config, personas map[string]string, log *slog.Logger, opts ...OpenOption) (*Agent, error) {
	if c.Agent == nil {
		return nil, errors.New("agent: the configuration has no [agent] table")
	}
	db, err := sessions.Open(ctx, c.Store.Path, sessions.Options{Retention: c.Store.Retention, Logger: log})
	if err != nil {
		return nil, err
	}
	clients, catalog := Providers(c)
	// Each entry is described to the engine as the entry describes it.
	// The last registered wins, so where entries share a served model,
	// the earliest describes it.
	for _, e := range slices.Backward(c.Agent.Models) {
		catalog.RegisterExtraModel(catalogModel(e.Provider, e.Served, e.Capabilities))
	}
	o := Options{
		Clients: clients, Catalog: catalog, Models: c.Agent.Models, Model: c.Agent.Model,
		Health:   NewHealth(HealthOptions{Providers: c.Providers, Logger: log}),
		Personas: personas, Sessions: db, Logger: log,
		MemoryCaps: MemoryCaps{User: c.Agent.Memory.MaxUser, Channel: c.Agent.Memory.MaxChannel, Bot: c.Agent.Memory.MaxBot},
		History: airut.History{
			MaxMessages:  c.Agent.History.MaxMessages,
			MaxTokens:    c.Agent.History.MaxTokens,
			MaxAge:       c.Agent.History.MaxAge,
			RoomMessages: c.Agent.History.RoomMessages,
			FollowWithin: c.Agent.History.FollowWithin,
			Compaction:   airut.Compaction(c.Agent.History.Compaction),
		},
	}
	for _, opt := range opts {
		opt(&o)
	}
	a, err := New(o)
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

// ForgetSessionsWithPrefix deletes every session whose key starts with
// prefix. A turn running in one of them finishes first; one that starts
// while the sessions are deleted may fail to record what it says.
func (a *Agent) ForgetSessionsWithPrefix(ctx context.Context, prefix string) error {
	for _, key := range a.locks.held(prefix) {
		unlock, err := a.locks.lock(ctx, key)
		if err != nil {
			return err
		}
		defer unlock()
	}
	return a.db.ForgetPrefix(ctx, prefix)
}

// LinkSession records that the session named key produced the bot's
// message ref in ch. It records nothing once the session is forgotten.
func (a *Agent) LinkSession(ctx context.Context, ref airut.MessageRef, ch airut.Channel, key string) error {
	return a.db.Link(ctx, message(ref, ch), key)
}

// LinkedSession returns the session that produced the bot's message ref
// in ch, while the session is kept.
func (a *Agent) LinkedSession(ctx context.Context, ref airut.MessageRef, ch airut.Channel) (string, bool, error) {
	return a.db.Linked(ctx, message(ref, ch))
}

// FollowSession records that person spoke, now, in the session named
// key.
func (a *Agent) FollowSession(ctx context.Context, person, key string) error {
	return a.db.Follow(ctx, person, key)
}

// FollowedSession returns the session person last spoke in, if that was
// within within, or within the agent's default FollowWithin when within
// is zero.
func (a *Agent) FollowedSession(ctx context.Context, person string, within time.Duration) (string, bool, error) {
	if within == 0 {
		within = a.history.FollowWithin
	}
	if within == 0 {
		within = airut.DefaultFollowWithin
	}
	key, at, ok, err := a.db.Followed(ctx, person)
	if err != nil || !ok || a.now().Sub(at) > within {
		return "", false, err
	}
	return key, true, nil
}

func message(ref airut.MessageRef, ch airut.Channel) sessions.Message {
	return sessions.Message{Platform: string(ch.Platform), Channel: ch.ID, ID: ref.ID}
}

// Run runs one agent turn started from c. The core has already checked
// the Gate and filled in the budget.
func (a *Agent) Run(c *airut.Ctx, t airut.Turn, out airut.TurnOutput) error {
	system, ok := a.personas[t.Persona]
	if !ok && t.Persona != "" {
		return fmt.Errorf("agent: no persona %q", t.Persona)
	}
	if a.character != "" {
		system = strings.TrimSpace(a.character + "\n\n" + system)
	}
	began := time.Now()
	tm := &timing{c: c, log: a.log}
	tm.on(a.chain[0], 0)
	tc := &turnClient{c: c, health: a.health, chain: a.chain, clients: a.clients, tm: tm}
	maxSide := t.ImageMaxSide
	switch {
	case maxSide < 0:
		return fmt.Errorf("agent: the turn's ImageMaxSide %d is negative", maxSide)
	case maxSide == 0:
		maxSide = airut.DefaultImageMaxSide
	}
	tools := make(core.Registry, len(t.Tools))
	for _, tl := range t.Tools {
		if _, dup := tools[tl.Name()]; dup {
			return fmt.Errorf("agent: the turn names tool %q twice", tl.Name())
		}
		tools[tl.Name()] = &tool{c: c, t: tl, tm: tm, a: a, maxSide: maxSide}
	}

	ctx := c.Context()
	if err := checkHistory(t.History); err != nil {
		return fmt.Errorf("agent: the turn's History: %w", err)
	}
	budget := &budget{turns: t.Budget.Turns, tokens: t.Budget.Tokens}
	if !tc.start(ctx) {
		a.finished(c, tm, began, 0, budget, "", ErrNoModel)
		return ErrNoModel
	}
	first := a.chain[tc.cur]
	defer tc.finish()
	opts := []core.Option{
		core.WithGate(gate{c: c}),
		core.WithTools(tools),
		core.WithCatalog(a.catalog),
		core.WithProvider(first.Provider),
		core.WithComponent(budget),
	}
	h := history(t.History, a.history)
	stable, volatile := a.memories(c, t.Prompt)
	opts = append(opts, core.WithAssembler(assembler{system: system, stable: stable,
		quoted: append(volatile, a.quoted(c, h.RoomMessages)...)}))
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
		// A session written before images were left out may still hold
		// some; none is sent again.
		tr.Messages = allWithoutImages(tr.Messages)
		tr, due := window(tr, h, a.now())
		resume = &tr
		opts = append(opts, core.WithTranscriptStore(imageless{s}),
			core.WithCompactionPolicy(policy{due: due, keep: keepTail(tr.Messages, h)}))
	}
	eng, err := core.New(tc, first.Served, opts...)
	if err != nil {
		return err
	}
	if resume != nil {
		eng.Resume(*resume)
	}
	base := tokens(eng.Cost())
	budget.used = func() int { return tokens(eng.Cost()) - base }

	steps := 0
	// last is the stop of the last request that ended without failing,
	// and said whether the turn has passed on any text.
	var last provider.StopReason
	said := false
	pass := func(text string) {
		said = said || strings.TrimSpace(text) != ""
		out.Text(text)
	}
	// Under FinalOnly, held is the text of the request under way. It is
	// passed on when the request ends without calling a tool, and
	// dropped when it calls one or fails, so narration never streams.
	// called says the request under way began a tool call, which marks
	// its text as narration even when the output limit cut the call off.
	var held strings.Builder
	called := false
	err = eng.Run(ctx, a.prompt(c, t, maxSide), func(e core.AgentEvent) {
		switch e := e.(type) {
		case core.EvTurnStart:
			steps = e.Step
			tm.begin(e.Step)
		case core.EvTextDelta:
			tm.first()
			if t.FinalOnly {
				held.WriteString(e.Delta)
			} else {
				pass(e.Delta)
			}
		case core.EvToolUseStart:
			tm.first()
			called = true
		case core.EvUsage:
			tm.usage(e.Usage)
		case core.EvTurnEnd:
			tm.end(e.Stop)
			if e.Err == nil {
				last = e.Stop
			}
			if t.FinalOnly {
				if e.Err == nil && e.Stop != provider.StopToolUse && !called && strings.TrimSpace(held.String()) != "" {
					pass(held.String())
				}
				held.Reset()
			}
			called = false
		case core.EvToolCall:
			out.ToolCall(e.Name)
		}
	})
	stop := a.finished(c, tm, began, steps, budget, last, err)
	if err != nil {
		return err
	}
	var notice string
	switch {
	case stop == "budget":
		notice = "(" + budget.stopped + ")"
	case stop == "length":
		notice = a.notices.CutOff
	case t.FinalOnly && !said:
		notice = a.notices.NoAnswer
	}
	if notice != "" {
		if said {
			notice = "\n\n" + notice
		}
		out.Text(notice)
	}
	return nil
}

// prompt is the turn's prompt for the engine: its text, and its images
// prepared for the model. A note for each image that cannot be sent
// follows the text.
func (a *Agent) prompt(c *airut.Ctx, t airut.Turn, maxSide int) core.PromptInput {
	in := core.PromptInput{Text: t.Prompt}
	var notes []string
	for _, b := range a.images(c, "prompt", "", t.Images, maxSide) {
		switch b := b.(type) {
		case provider.ImageBlock:
			in.Images = append(in.Images, b)
		case provider.TextBlock:
			notes = append(notes, b.Text)
		}
	}
	if len(notes) > 0 {
		in.Text = strings.TrimSpace(in.Text + "\n\n" + strings.Join(notes, "\n"))
	}
	return in
}

// Notices are what a turn tells the chat when it ends without a whole
// answer. Each is posted as written, so it should read as the bot's own
// words. An empty field takes its default.
type Notices struct {
	// CutOff ends the reply of a turn whose last model request stopped
	// at its output limit. Default: DefaultCutOffNotice.
	CutOff string
	// NoAnswer is the reply of a FinalOnly turn that ended with nothing
	// to say, so the chat is never left with no reply. Default:
	// DefaultNoAnswerNotice.
	NoAnswer string
}

// The notices a turn gives when Notices leaves a field empty.
const (
	DefaultCutOffNotice   = "My answer was cut off. Ask again, or narrow the request."
	DefaultNoAnswerNotice = "I finished without an answer. Ask again, or put it another way."
)

func (n Notices) withDefaults() Notices {
	if n.CutOff == "" {
		n.CutOff = DefaultCutOffNotice
	}
	if n.NoAnswer == "" {
		n.NoAnswer = DefaultNoAnswerNotice
	}
	return n
}

// finished logs and counts a turn that ended with err after steps, the
// last of which stopped for last, and returns how the turn stopped.
func (a *Agent) finished(c *airut.Ctx, tm *timing, began time.Time, steps int, budget *budget, last provider.StopReason, err error) string {
	stop := stopKind(err, budget.stopped, last)
	modelTime, toolTime := tm.totals()
	used := 0
	if budget.used != nil {
		used = budget.used()
	}
	provider, model, served, fell := tm.entry()
	a.log.Info("agent turn finished", "invocation", c.ID(), "platform", string(c.Platform()),
		"provider", provider, "model", model, "served", served, "fell_through", fell,
		"requests", budget.allowed(steps), "tokens", used, "stop", stop,
		"ms", time.Since(began).Milliseconds(), "model_ms", modelTime.Milliseconds(), "tool_ms", toolTime.Milliseconds())
	m, command := c.Metrics(), c.MetricCommand()
	m.Add("airut_agent_requests_total", float64(budget.allowed(steps)), "command", command)
	m.Add("airut_agent_tokens_total", float64(used), "command", command)
	m.Add("airut_agent_stops_total", 1, "command", command, "stop", stop)
	return stop
}

// stopKind is how a turn stopped: with an error, at its budget, cut off
// by the output limit on its last request, or done. A last request cut
// off with a tool call in it counts as length too: the engine ends the
// turn there without running the call.
func stopKind(err error, stopped string, last provider.StopReason) string {
	switch {
	case err != nil:
		return "error"
	case stopped != "":
		return "budget"
	case last == provider.StopLength:
		return "length"
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

// timing times a turn's model requests and tool calls, for the log and
// the metrics. Each request, a retry of a step included, runs from the
// step's start or the last attempt's end to its own end, so a retry's
// time holds its backoff.
type timing struct {
	c   *airut.Ctx
	log *slog.Logger
	// provider, model, and served name the entry the turn runs on, by
	// its alias and its served id, and fell how many entries it passed
	// over to reach it. An empty served is model's.
	provider, model, served string
	fell                    int

	mu         sync.Mutex
	step       int
	start, tok time.Time
	used       provider.Usage
	modelTotal time.Duration
	toolTotal  time.Duration
}

// on records that the turn runs on e, after passing over fell entries.
func (t *timing) on(e config.ChainEntry, fell int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.provider, t.model, t.served, t.fell = e.Provider, e.Model, e.Served, fell
}

// entry is the entry the turn runs on, as on recorded it.
func (t *timing) entry() (provider, model, served string, fell int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	served = t.served
	if served == "" {
		served = t.model
	}
	return t.provider, t.model, served, t.fell
}

func (t *timing) begin(step int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.step, t.start, t.tok, t.used = step, time.Now(), time.Time{}, provider.Usage{}
}

// first marks the request's first text or tool-use delta.
func (t *timing) first() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok.IsZero() {
		t.tok = time.Now()
	}
}

func (t *timing) usage(u provider.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.used = u
}

// end logs and counts the request that just ended, and starts the next
// attempt's clock, in case the step is retried.
func (t *timing) end(stop provider.StopReason) {
	t.mu.Lock()
	now := time.Now()
	d, first, used, step := now.Sub(t.start), time.Duration(-1), t.used, t.step
	if !t.tok.IsZero() {
		first = t.tok.Sub(t.start)
	}
	t.modelTotal += d
	t.start, t.tok, t.used = now, time.Time{}, provider.Usage{}
	t.mu.Unlock()
	prov, model, served, fell := t.entry()
	m := t.c.Metrics()
	m.Observe("airut_agent_request_seconds", d, "provider", prov, "model", model, "served", served)
	attrs := []any{"invocation", t.c.ID(), "step", step, "provider", prov, "model", model, "served", served,
		"fell_through", fell, "ms", d.Milliseconds(), "input_tokens", used.InputTokens, "output_tokens", used.OutputTokens,
		"stop", string(stop)}
	if first >= 0 {
		m.Observe("airut_agent_first_token_seconds", first, "provider", prov, "model", model, "served", served)
		attrs = append(attrs, "first_token_ms", first.Milliseconds())
	}
	t.log.Info("agent request", attrs...)
}

// tool logs and counts one tool call.
func (t *timing) tool(name string, d time.Duration, outcome string) {
	t.mu.Lock()
	t.toolTotal += d
	step := t.step
	t.mu.Unlock()
	t.c.Metrics().Observe("airut_agent_tool_seconds", d, "tool", name)
	t.log.Info("agent tool", "invocation", t.c.ID(), "step", step, "tool", name, "ms", d.Milliseconds(), "outcome", outcome)
}

func (t *timing) totals() (model, tools time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.modelTotal, t.toolTotal
}

// tool adapts an airut.Tool to terva's. maxSide caps the images an
// airut.ImageTool returns.
type tool struct {
	c       *airut.Ctx
	t       airut.Tool
	tm      *timing
	a       *Agent
	maxSide int
}

func (x *tool) Name() string            { return x.t.Name() }
func (x *tool) Description() string     { return x.t.Description() }
func (x *tool) Schema() json.RawMessage { return x.t.Schema() }

// Execute runs the tool for the turn's caller. What the model reads of a
// failure is only text meant for people, as the chat would show it: an
// error's own text can hold what neither should see.
func (x *tool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	start, outcome := time.Now(), "ok"
	defer func() { x.tm.tool(x.t.Name(), time.Since(start), outcome) }()
	var text string
	var imgs []airut.Image
	var err error
	if it, ok := x.t.(airut.ImageTool); ok {
		var out airut.ToolOutput
		out, err = it.CallImages(x.c, args)
		text, imgs = out.Text, out.Images
	} else {
		text, err = x.t.Call(x.c, args)
	}
	if err != nil {
		outcome = "error"
		// Only when the turn itself has ended does the engine record the
		// call as aborted. A tool's own timeout is a failure like any
		// other, which the model can read and recover from.
		if ctx.Err() != nil {
			return core.ToolResult{}, err
		}
		text = strings.TrimSpace(text + "\n" + forModel(err))
		return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}, nil
	}
	if len(imgs) > 0 {
		// The images follow the text, each in its place or a note saying
		// why it is not there.
		content := []provider.Content{}
		if strings.TrimSpace(text) != "" {
			content = append(content, provider.TextBlock{Text: text})
		}
		content = append(content, x.a.images(x.c, "tool", x.t.Name(), imgs, x.maxSide)...)
		return core.ToolResult{Content: content}, nil
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

// held returns, sorted, the keys starting with prefix that a turn holds
// or waits for.
func (l *keyLocks) held(prefix string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keys []string
	for k := range l.m {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
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
