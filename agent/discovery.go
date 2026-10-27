package agent

import (
	"context"
	"errors"
	"net/http"
	"time"

	"terva.sh/airut/config"
)

// ModelState is residency on one backend, not presence in a catalogue.
type ModelState string

const (
	StateReady    ModelState = "ready"
	StateLoading  ModelState = "loading"
	StateUnloaded ModelState = "unloaded"
	StateUnknown  ModelState = "unknown"
)

// ModelKey identifies the inference route, after configuration aliases resolve.
type ModelKey struct{ Provider, Served string }

// Route names one possible backend and the model identity it serves. Discovery
// must include every possible route unless inference is independently pinned.
type Route struct{ Backend, Served string }

// BackendState describes one authoritative observation. Missing Models entries
// mean unknown. Idle may be true only when the backend is authoritatively idle.
type BackendState struct {
	Models map[string]ModelState
	Idle   bool
}

// Snapshot separates inference routing from backend residency. An absent model,
// route or backend, including an empty catalogue, means unknown, never unloaded.
// Backends must use the same identity across providers that share hardware.
type Snapshot struct {
	Routes   map[ModelKey][]Route
	Backends map[string]BackendState
}

// Discovery is implemented by the application for its own discovery protocol.
// Snapshot must do a fresh, read-only, bounded, cancellable lookup on every
// call, including concurrent calls. Do not cache or coalesce ready observations.
// Health supplies a short deadline; adapters must honor it and must not return
// cached observations after an error. Errors and response bodies are never logged.
//
// Discovery is best effort: only provider-side atomic admission can prevent
// another client changing residency or routing between discovery and inference.
type Discovery interface {
	Snapshot(context.Context, []ModelKey) (Snapshot, error)
}

var errSelectionChanged = errors.New("agent: model selection changed before dispatch")

// providerHTTP owns a pool without modifying the process default transport.
func providerHTTP() *http.Client {
	tr := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (h *Health) usesApplicationDiscovery(k ModelKey) bool {
	return h.discovery != nil && (len(h.discoveryProviders) == 0 || h.discoveryProviders[k.Provider])
}

func (h *Health) snapshot(ctx context.Context, keys []ModelKey) Snapshot {
	ctx, cancel := context.WithTimeout(ctx, h.discoveryTimeout)
	defer cancel()
	out := Snapshot{Routes: map[ModelKey][]Route{}, Backends: map[string]BackendState{}}
	// One application snapshot ranks its provider group consistently.
	custom := make([]ModelKey, 0, len(keys))
	for _, k := range keys {
		if h.usesApplicationDiscovery(k) {
			custom = append(custom, k)
		}
	}
	if len(custom) > 0 {
		s, err := h.discovery.Snapshot(ctx, custom)
		if err == nil && ctx.Err() == nil {
			for k, v := range s.Routes {
				out.Routes[k] = append([]Route(nil), v...)
			}
			for k, v := range s.Backends {
				b := BackendState{Idle: v.Idle}
				if v.Models != nil {
					b.Models = map[string]ModelState{}
					for id, state := range v.Models {
						b.Models[id] = state
					}
				}
				out.Backends[k] = b
			}
		}
		if out.Routes == nil {
			out.Routes = map[ModelKey][]Route{}
		}
		if out.Backends == nil {
			out.Backends = map[string]BackendState{}
		}
	}
	// A route overlaps even when its application backend observation is absent.
	applicationBackends := map[string]bool{}
	for id := range out.Backends {
		applicationBackends[id] = true
	}
	for _, k := range custom {
		for _, r := range out.Routes[k] {
			applicationBackends[r.Backend] = true
		}
	}
	native := map[string]BackendState{}
	nativeInventory := map[string]bool{}
	for _, k := range keys {
		if h.usesApplicationDiscovery(k) {
			continue
		}
		p, managed := h.providers[k.Provider]
		if managed && p.Kind == config.KindLlamaSwap {
			id := p.RunningURL()
			out.Routes[k] = []Route{{Backend: id, Served: k.Served}}
			if _, have := native[id]; !have {
				native[id] = h.runningState(ctx, p)
				nativeInventory[id] = true
			}
			// A valid running inventory proves absence of a requested model.
			b := native[id]
			if b.Models != nil {
				if _, have := b.Models[k.Served]; !have {
					b.Models[k.Served] = StateUnloaded
				}
			}
		} else {
			id := "provider:" + k.Provider
			out.Routes[k] = []Route{{Backend: id, Served: k.Served}}
			b := native[id]
			if b.Models == nil {
				b.Models = map[string]ModelState{}
			}
			b.Models[k.Served] = StateReady
			native[id] = b
		}
	}
	for id, observed := range native {
		if applicationBackends[id] {
			observed = reconcileBackend(out.Backends[id], observed, nativeInventory[id], out.Routes, id)
		}
		out.Backends[id] = observed
	}
	return out
}

// reconcileBackend intersects authority over shared hardware. A native
// running inventory can prove absence; an application catalogue cannot.
// Missing either observation never inherits the other's readiness or cold-load
// authority.
func reconcileBackend(application, native BackendState, nativeInventory bool, routes map[ModelKey][]Route, backend string) BackendState {
	merged := BackendState{Models: map[string]ModelState{}, Idle: application.Idle && native.Idle}
	for served := range application.Models {
		merged.Models[served] = StateUnknown
	}
	for served := range native.Models {
		merged.Models[served] = StateUnknown
	}
	for _, candidates := range routes {
		for _, r := range candidates {
			if r.Backend == backend {
				merged.Models[r.Served] = StateUnknown
			}
		}
	}
	for served := range merged.Models {
		a := application.Models[served]
		n, present := native.Models[served]
		if !present && nativeInventory && native.Models != nil {
			n = StateUnloaded
		}
		if a != "" && a == n {
			merged.Models[served] = a
		} else {
			merged.Idle = false
		}
	}
	return merged
}

func ready(s Snapshot, k ModelKey) bool {
	routes := s.Routes[k]
	if len(routes) == 0 {
		return false
	}
	for _, r := range routes {
		if r.Backend == "" || r.Served == "" || s.Backends[r.Backend].Models[r.Served] != StateReady {
			return false
		}
	}
	return true
}

// selectModel ranks a fresh snapshot without reserving breaker probes. Even
// candidates excluded after failure still participate in the cold-load veto:
// a loaded or unknown acceptable model must not authorize loading another.
func (h *Health) selectModel(ctx context.Context, keys []ModelKey, excluded []bool) int {
	s := h.snapshot(ctx, keys)
	if ctx.Err() != nil {
		return -1
	}
	i := h.choose(s, keys, excluded)
	if i >= 0 {
		kind := "cold_load"
		if ready(s, keys[i]) {
			kind = "ready"
		}
		h.log.Debug("model selection", "provider", keys[i].Provider, "served", keys[i].Served, "decision", kind, "best_effort", true)
	}
	return i
}

func (h *Health) choose(s Snapshot, keys []ModelKey, excluded []bool) int {

	for i, k := range keys {
		if !excluded[i] && h.breakerAllows(k, false) && ready(s, k) {
			return i
		}
	}
	// Every possible route of every acceptable model must prove absence.
	for _, k := range keys {
		routes := s.Routes[k]
		if len(routes) == 0 {
			return -1
		}
		for _, r := range routes {
			if r.Backend == "" || r.Served == "" || s.Backends[r.Backend].Models[r.Served] != StateUnloaded {
				return -1
			}
		}
	}
	// Restrictions attach to shared backends, regardless of provider aliases.
	forbidden := map[string]bool{}
	for _, k := range keys {
		p := h.providers[k.Provider]
		for _, r := range s.Routes[k] {
			policy := h.backendLoads[r.Backend]
			if p.Load == config.LoadNever || p.Kind == config.KindLlamaSwap && p.Load == "" ||
				h.usesApplicationDiscovery(k) && (policy == "" || policy == config.LoadNever) {
				forbidden[r.Backend] = true
			}
		}
	}
	// Native providers sharing an endpoint share its strictest no-load
	// restriction even when the protected provider is outside this role.
	for _, p := range h.providers {
		if p.Kind == config.KindLlamaSwap && (p.Load == "" || p.Load == config.LoadNever) {
			forbidden[p.RunningURL()] = true
		}
	}
	for i, k := range keys {
		if excluded[i] || !h.breakerAllows(k, false) {
			continue
		}
		allowed := true
		for _, r := range s.Routes[k] {
			b := s.Backends[r.Backend]
			policy := h.backendLoads[r.Backend]
			p := h.providers[k.Provider]
			if p.Kind == config.KindLlamaSwap && !h.usesApplicationDiscovery(k) {
				// Native routes inherit the provider policy, while an explicit
				// shared-backend restriction still applies. Application routes
				// always require their own backend permission, including llama-swap.
				if shared, have := h.backendLoads[r.Backend]; have && !permitsLoad(shared, b) {
					allowed = false
					break
				}
				policy = p.Load
			}
			if forbidden[r.Backend] || !permitsLoad(policy, b) || p.Load != "" && !permitsLoad(p.Load, b) {
				allowed = false
				break
			}
		}
		if allowed {
			return i
		}
	}
	return -1
}

func permitsLoad(policy string, b BackendState) bool {
	// Unknown or loading hardware is never authority for loading, even when
	// the requested model itself is reported unloaded.
	for _, state := range b.Models {
		if state != StateReady && state != StateUnloaded {
			return false
		}
		if b.Idle && state != StateUnloaded {
			return false
		}
	}
	return policy == config.LoadAlways || policy == config.LoadWhenIdle && b.Idle
}

func (h *Health) dispatch(ctx context.Context, keys []ModelKey, excluded []bool, selected int) error {
	i := h.selectModel(ctx, keys, excluded)
	if err := ctx.Err(); err != nil {
		return err
	}
	if i != selected {
		return errSelectionChanged
	}
	if !h.breakerAllows(keys[selected], true) {
		return errSelectionChanged
	}
	return nil
}

func discoveryDeadline(d time.Duration) time.Duration {
	if d <= 0 || d > 30*time.Second {
		return DefaultRunningTimeout
	}
	return d
}
