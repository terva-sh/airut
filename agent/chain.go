package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// turnClient guards one selected request. The engine is configured for this
// entry; a changed selection returns before dispatch and rebuilds the engine.
type turnClient struct {
	c              *airut.Ctx
	health         *Health
	chain          config.Chain
	clients        map[string]provider.Client
	tm             *timing
	model          provider.Model
	cur            int
	keys           []ModelKey
	excluded       []bool
	mu             sync.Mutex
	sent, streamed bool
}

func (t *turnClient) Name() string            { return t.chain[t.cur].Provider }
func (t *turnClient) Unwrap() provider.Client { return t.clients[t.chain[t.cur].Provider] }
func (t *turnClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	e := t.chain[t.cur]
	t.mu.Lock()
	t.sent = false
	t.streamed = false
	t.mu.Unlock()
	if !t.model.Has(provider.CapImageInput) {
		req.Messages = requestWithoutImages(req.Messages)
	}
	if t.model.ContextWindow > 0 {
		raw, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		// Conservative estimate, including framing and the reserved reply. A
		// tokenizer is not available at this boundary; never silently truncate.
		if (len(raw)+2)/3+req.MaxTokens > t.model.ContextWindow {
			return nil, ErrRequestTooLarge
		}
	}
	guard := func(checkCtx context.Context) error {
		if err := t.health.dispatch(checkCtx, t.keys, t.excluded, t.cur); err != nil {
			return err
		}
		t.mu.Lock()
		t.sent = true
		t.mu.Unlock()
		return nil
	}
	if hasHTTPBoundary(t.Unwrap()) {
		ctx = withDispatch(ctx, guard)
	} else if err := guard(ctx); err != nil {
		return nil, err
	}
	events, err := t.attempt(ctx, e, req, true)
	t.mu.Lock()
	sent := t.sent
	t.mu.Unlock()
	if sent && err != nil && !errors.Is(err, errSelectionChanged) {
		t.health.done(e.Provider, e.Served, classify(ctx, err))
	}
	return events, err
}

// attempt sends req to e. When peek is set, it holds the stream back
// until its first event that is output, so that a failure before any
// returns as an error the caller can fall through on.
func (t *turnClient) attempt(ctx context.Context, e config.ChainEntry, req provider.Request, peek bool) (<-chan provider.Event, error) {
	if e.MaxOutput > 0 && (req.MaxTokens == 0 || req.MaxTokens > e.MaxOutput) {
		req.MaxTokens = e.MaxOutput
	}
	if e.Temperature != nil {
		temp := float32(*e.Temperature)
		req.Temperature = &temp
	}
	parent := ctx
	var cancel context.CancelFunc
	if e.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	events, err := t.clients[e.Provider].Stream(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	var held []provider.Event
	if peek {
		said := false
		for ev := range events {
			if d, ok := ev.(provider.EventDone); ok && d.Err != nil {
				cancel()
				for range events {
				}
				return nil, d.Err
			}
			held = append(held, ev)
			if output(ev) {
				t.mu.Lock()
				t.streamed = true
				t.mu.Unlock()
				said = true
				break
			}
		}
		// A stream that closed with nothing said and no end, as a client
		// may when its request is canceled, failed by ctx.
		if !said && ctx.Err() != nil {
			err := ctx.Err()
			cancel()
			return nil, err
		}
	}
	out := make(chan provider.Event, len(held)+1)
	go t.forward(parent, ctx, cancel, e, held, events, out)
	return out, nil
}

// forward hands held and then events on to out, and records how the
// request ended. When ctx ends first, as when the engine stops reading
// because the turn was stopped, it cancels the request, leaves events to
// drain on its own, and records the end, so neither this goroutine nor a
// half-open breaker's probe is left waiting. When the entry's own
// timeout ended it while the caller still reads, the caller is sent the
// timeout as the stream's end, so a cut-off answer does not pass for a
// whole one.
func (t *turnClient) forward(parent, ctx context.Context, cancel context.CancelFunc, e config.ChainEntry,
	held []provider.Event, events <-chan provider.Event, out chan<- provider.Event) {
	defer close(out)
	defer cancel()
	var failed error
	ended := false
	send := func(ev provider.Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// timedOut tells a caller still reading that the request ended
	// because ctx did.
	timedOut := func() {
		if parent.Err() != nil {
			return
		}
		select {
		case out <- provider.EventDone{Err: ctx.Err()}:
		case <-parent.Done():
		}
	}
	for _, ev := range held {
		if !send(ev) {
			t.abandon(parent, ctx, cancel, e, events)
			timedOut()
			return
		}
	}
	for ev := range events {
		if d, ok := ev.(provider.EventDone); ok {
			failed, ended = d.Err, true
		}
		if !send(ev) {
			t.abandon(parent, ctx, cancel, e, events)
			timedOut()
			return
		}
	}
	// A stream that closed without its end, as a client may when its
	// request is canceled, ended by ctx.
	if !ended && ctx.Err() != nil {
		failed = ctx.Err()
		timedOut()
	}
	t.health.done(e.Provider, e.Served, classify(parent, failed))
}

// abandon ends a request nobody reads any more: it cancels it, drains
// what the client still sends on a goroutine of its own, which ends when
// the client closes events, and records the end by why ctx ended.
func (t *turnClient) abandon(parent, ctx context.Context, cancel context.CancelFunc, e config.ChainEntry, events <-chan provider.Event) {
	cancel()
	go func() {
		for range events {
		}
	}()
	t.health.done(e.Provider, e.Served, classify(parent, ctx.Err()))
}

// output reports whether ev is something the model said: anything but
// the stream's opening, its usage, and its transport notes.
func output(ev provider.Event) bool {
	switch ev.(type) {
	case provider.EventStart, provider.EventUsage, provider.EventTransport:
		return false
	}
	return true
}

// ErrRequestTooLarge means the selected candidate's conservative input
// estimate plus reserved output exceeds its configured context cap.
var ErrRequestTooLarge = errors.New("agent: request exceeds selected model context")

func requestWithoutImages(messages []provider.Message) []provider.Message {
	out := append([]provider.Message(nil), messages...)
	var content func([]provider.Content) []provider.Content
	content = func(in []provider.Content) []provider.Content {
		out := append([]provider.Content(nil), in...)
		for i, c := range out {
			switch b := c.(type) {
			case provider.ImageBlock:
				out[i] = provider.TextBlock{Text: "[image omitted: this model does not accept images]"}
			case provider.ToolResultBlock:
				b.Content = content(b.Content)
				out[i] = b
			}
		}
		return out
	}
	for i := range out {
		out[i].Content = content(out[i].Content)
	}
	return out
}
