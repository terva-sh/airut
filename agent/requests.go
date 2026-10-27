package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/config"
	"terva.sh/terva/packages/core"
	"terva.sh/terva/packages/provider"
)

// oneStep ends only after the engine has executed and recorded the entire
// tool batch. The next engine resumes it; no tool dispatch is repeated.
type oneStep struct{}

func (oneStep) StepGate() core.StepGate {
	return core.StepGate{After: func(context.Context, core.StepState, func(core.AgentEvent)) bool { return true }}
}

type selectedCatalog struct {
	provider.ModelCatalog
	model provider.Model
}

func (c selectedCatalog) FindModel(p, id string) (provider.Model, error) {
	if (p == "" || p == c.model.Provider) && id == c.model.ID {
		return c.model, nil
	}
	return c.ModelCatalog.FindModel(p, id)
}
func effectiveModel(m provider.Model, e config.ChainEntry) provider.Model {
	if e.ContextWindow > 0 {
		m.ContextWindow = e.ContextWindow
	}
	if e.MaxOutput > 0 {
		m.MaxOutput = e.MaxOutput
	}
	if e.ImageInput != nil {
		caps := map[provider.Capability]bool{}
		for k, v := range m.Caps {
			caps[k] = v
		}
		caps[provider.CapImageInput] = *e.ImageInput
		m.Caps = caps
	}
	if e.Reasoning {
		m.Reasoning = true
	}
	return m
}

func (a *Agent) runRequests(c *airut.Ctx, ctx context.Context, t airut.Turn, maxSide int, opts []core.Option,
	memory *core.MemoryTranscriptStore, budget *budget, tm *timing, steps *int, sink func(core.AgentEvent)) error {
	keys := make([]ModelKey, len(a.chain))
	for i, e := range a.chain {
		keys[i] = ModelKey{e.Provider, e.Served}
	}
	excluded := make([]bool, len(keys))
	first := true
	cacheKey := "airut-" + rand.Text()
	var last error
	retries, changes, fell := 0, 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if retries == 0 && changes == 0 {
			if allowed, _ := budget.BeforeTurn(*steps + 1); !allowed {
				(*steps)++
				return nil
			}
			(*steps)++
		}
		i := a.health.selectModel(ctx, keys, excluded)
		if i < 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if last != nil {
				return last
			}
			return ErrNoModel
		}
		e := a.chain[i]
		// Count skipped entries for compatibility, without reserving their probes.
		if retries == 0 && changes == 0 {
			for n := 0; n < i; n++ {
				why := reasonUnavailable
				if !a.health.breakerAllows(keys[n], false) {
					why = reasonBreakerOpen
				}
				skipped(c, a.log, "agent", a.chain[n].Provider, a.chain[n].Model, a.chain[n].Served, why)
				fell++
			}
		}
		tm.on(e, fell)
		model := effectiveModel(a.entryModels[i], e)
		tc := &turnClient{c: c, health: a.health, chain: a.chain, cur: i, keys: keys, excluded: excluded, tm: tm, model: model,
			clients: map[string]provider.Client{e.Provider: a.entryClients[i]}}
		requestOpts := append([]core.Option{}, opts...)
		requestOpts = append(requestOpts, core.WithCatalog(selectedCatalog{a.catalog, model}), core.WithProvider(e.Provider), core.WithMaxTokens(model.MaxOutput))
		if !first {
			requestOpts = append(requestOpts, core.WithCompactionPolicy(core.DefaultCompactionPolicy{}))
		}
		eng, err := core.New(tc, e.Served, requestOpts...)
		if err != nil {
			return err
		}
		eng.AdoptSessionIdentity(core.TranscriptIdentity{ID: cacheKey, CacheKey: cacheKey})
		eng.Resume(memory.Transcript())
		eng.AttachTranscriptStore(memory)
		var stop provider.StopReason
		observe := func(ev core.AgentEvent) {
			if end, ok := ev.(core.EvTurnEnd); ok {
				stop = end.Stop
				tc.mu.Lock()
				sent := tc.sent
				tc.sent = false
				tc.mu.Unlock()
				if sent {
					tm.end(end.Stop)
				}
			}
			sink(ev)
		}
		if first {
			err = eng.Run(ctx, a.prompt(c, t, maxSide), observe)
			first = false
		} else {
			if d := eng.Compaction(core.CompactBeforeTurn); d.Compact {
				_, compactErr := eng.CompactWith(ctx, d, func(string) {})
				if errors.Is(compactErr, errSelectionChanged) {
					changes++
					if changes > 8 {
						return ErrNoModel
					}
					continue
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			err = eng.Continue(ctx, observe)
		}
		if perr := eng.PersistenceError(); perr != nil {
			return perr
		}
		if errors.Is(err, ErrRequestTooLarge) {
			excluded[i] = true
			last = err
			changes++
			if changes > len(keys)+8 {
				return err
			}
			continue
		}
		if errors.Is(err, errSelectionChanged) {
			changes++
			if changes > 8 {
				return ErrNoModel
			}
			continue
		}
		if err != nil {
			// A response already streaming stays on its model and is never
			// restarted on a different model after partial output.
			tc.mu.Lock()
			streamed := tc.streamed
			tc.mu.Unlock()
			class := classify(ctx, err)
			if streamed || !fallsThrough(class) {
				return err
			}
			last = err
			excluded[i] = true
			next := a.health.selectModel(ctx, keys, excluded)
			if next >= 0 {
				skipped(c, a.log, "agent", e.Provider, e.Model, e.Served, class)
				fell++
				retries++
				continue
			}
			// Preserve bounded transient retries when no fallback is usable.
			// Every retry ranks the full list and passes a fresh dispatch check.
			if retries >= 6 {
				return err
			}
			retries++
			delay := 2 * time.Second << min(retries-1, 5)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			clear(excluded)
			continue
		}
		if stop != provider.StopToolUse {
			return nil
		}
		clear(excluded)
		last = nil
		retries, changes = 0, 0
	}
}
