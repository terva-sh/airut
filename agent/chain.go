package agent

import (
	"context"
	"sync"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// turnClient is the provider client one agent turn runs on. It walks the
// agent's chain: at the turn's start it takes the first entry that is
// available and whose breaker is closed. Until that entry's first
// request has streamed something, a failure that falls through moves the
// turn to the next usable entry, inside the same Stream call, so the
// engine sees one request and its transcript holds the prompt once. Once
// a request has streamed anything the turn is committed to its entry,
// and a failure fails the request, as without a chain.
type turnClient struct {
	c       *airut.Ctx
	health  *Health
	chain   config.Chain
	clients map[string]provider.Client
	tm      *timing

	mu        sync.Mutex
	cur       int
	fell      int
	committed bool
	sent      bool
}

func (t *turnClient) Name() string { return "airut-chain" }

// Unwrap is the entry the turn started on, so that terva reads that
// client's capabilities through this one.
func (t *turnClient) Unwrap() provider.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clients[t.chain[t.cur].Provider]
}

// start picks the turn's first entry, from the chain's start. It reports
// false when no entry is usable.
func (t *turnClient) start(ctx context.Context) bool {
	return t.moveFrom(ctx, 0, "")
}

// moveFrom makes the first usable entry from i the turn's. why is the
// reason the turn leaves its entry, or "" at the start. Each entry
// passed over is counted when the walk goes on to the next; the last
// one, with nothing after it, is not. It reports false, leaving the
// turn's entry as it was, when no entry from i is usable.
func (t *turnClient) moveFrom(ctx context.Context, i int, why string) bool {
	t.mu.Lock()
	from := t.cur
	t.mu.Unlock()
	for ; i < len(t.chain); i++ {
		e := t.chain[i]
		reason := t.health.usable(ctx, e.Provider, e.Served)
		if why != "" {
			left := t.chain[from]
			skipped(t.c, t.tm.log, "agent", left.Provider, left.Model, left.Served, why)
			t.mu.Lock()
			t.fell++
			t.mu.Unlock()
		}
		if reason == "" {
			t.mu.Lock()
			t.cur = i
			fell := t.fell
			t.mu.Unlock()
			t.tm.on(e, fell)
			return true
		}
		from, why = i, reason
	}
	return false
}

// finish ends a turn that may never have sent its entry a request, as
// when the engine failed to start: the probe start may have taken from a
// half-open breaker is handed back, so the breaker does not stay open.
func (t *turnClient) finish() {
	t.mu.Lock()
	sent, e := t.sent, t.chain[t.cur]
	t.mu.Unlock()
	if !sent {
		t.health.done(e.Provider, e.Served, classCanceled)
	}
}

// Stream sends req to the turn's entry, under its served id.
func (t *turnClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	t.mu.Lock()
	committed := t.committed
	t.sent = true
	t.mu.Unlock()
	for {
		t.mu.Lock()
		i := t.cur
		t.mu.Unlock()
		e := t.chain[i]
		events, err := t.attempt(ctx, e, req, !committed)
		if err == nil {
			t.mu.Lock()
			t.committed = true
			t.mu.Unlock()
			return events, nil
		}
		class := classify(ctx, err)
		t.health.done(e.Provider, e.Served, class)
		if committed || !fallsThrough(class) || !t.moveFrom(ctx, i+1, class) {
			return nil, err
		}
	}
}

// attempt sends req to e. When peek is set, it holds the stream back
// until its first event that is output, so that a failure before any
// returns as an error the caller can fall through on.
func (t *turnClient) attempt(ctx context.Context, e config.ChainEntry, req provider.Request, peek bool) (<-chan provider.Event, error) {
	req.Model = e.Served
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
