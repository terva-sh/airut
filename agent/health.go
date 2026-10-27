package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// The defaults of HealthOptions.
const (
	// DefaultBreakerFailures is how many failures in a row open a
	// served model's breaker.
	DefaultBreakerFailures = 3
	// DefaultBreakerCoolOff is how long an open breaker skips its model
	// before it lets one request through to try it again.
	DefaultBreakerCoolOff = 30 * time.Second
	// DefaultNotLoadedFor is retained for source compatibility. Negative
	// observations are no longer cached.
	// Deprecated: fresh selection ignores NotLoadedFor.
	DefaultNotLoadedFor = 30 * time.Second
	// DefaultRunningTimeout bounds one request to a llama-swap
	// provider's /running.
	DefaultRunningTimeout = 3 * time.Second
)

// ErrNoModel is the error of a turn or judgement that found no model of
// its chain it could use: each was unavailable, or its breaker open.
var ErrNoModel = errors.New("agent: no model of the chain is available")

// HealthOptions configure NewHealth.
type HealthOptions struct {
	// Discovery supplies application-owned routing and residency. Nil uses
	// llama-swap discovery and treats other providers as unmanaged/available.
	Discovery Discovery
	// DiscoveryProviders scopes Discovery to these provider IDs. Empty means
	// all providers. Others retain native llama-swap/unmanaged discovery.
	DiscoveryProviders []string
	// BackendLoads explicitly permits cold loads on application backend IDs.
	// Unlisted backends default to never. Provider load=never always wins.
	BackendLoads map[string]string
	// DiscoveryTimeout bounds each complete lookup; default 3s, maximum 30s.
	DiscoveryTimeout time.Duration
	// HTTPClients are pooled clients by provider, shared with inference where
	// routing and authentication match. HTTP is the fallback.
	HTTPClients map[string]*http.Client
	// Providers supplies native llama-swap discovery and load restrictions.
	// Without an application adapter, other providers remain unmanaged.
	Providers []config.Provider
	// HTTP is the fallback pooled client for /running. DiscoveryTimeout
	// bounds each lookup even when a supplied client has no timeout.
	HTTP *http.Client
	// Failures and CoolOff tune the breakers. Zero takes the defaults.
	// Deprecated: NotLoadedFor is ignored; every observation is fresh.
	Failures     int
	CoolOff      time.Duration
	NotLoadedFor time.Duration
	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
	// Logger receives each availability decision at Debug, by provider,
	// served model, and kind, and each breaker that opens at Info. Nil
	// discards them.
	Logger *slog.Logger
}

// Health is what airut knows of each served model's health: whether its
// provider says it can serve it now, and a circuit breaker that skips it
// after failures in a row. Both key on the provider and the served id,
// never the alias, so a failure seen through one variant holds for every
// variant of the model. An Agent and a Judge may share one, so that a
// failure one sees the other skips. It is safe for concurrent use.
type Health struct {
	discoveryProviders map[string]bool
	discovery          Discovery
	backendLoads       map[string]string
	discoveryTimeout   time.Duration
	httpClients        map[string]*http.Client
	providers          map[string]config.Provider
	http               *http.Client
	failures           int
	coolOff            time.Duration
	now                func() time.Time
	log                *slog.Logger

	mu       sync.Mutex
	breakers map[servedModel]*breaker
}

type servedModel struct{ provider, served string }

// breaker is one served model's circuit breaker. It is closed while
// openedAt is zero, open until the cool-off has passed, and half-open
// after, when one request, the probe, may try the model.
type breaker struct {
	fails    int
	openedAt time.Time
	probing  bool
}

// NewHealth returns a Health. It makes no network request.
func NewHealth(o HealthOptions) *Health {
	h := &Health{
		providers: map[string]config.Provider{}, http: o.HTTP,
		discovery: o.Discovery, discoveryProviders: map[string]bool{}, backendLoads: map[string]string{}, discoveryTimeout: discoveryDeadline(o.DiscoveryTimeout), httpClients: map[string]*http.Client{},
		failures: o.Failures, coolOff: o.CoolOff,
		now: o.Now, log: o.Logger,
		breakers: map[servedModel]*breaker{},
	}
	for _, id := range o.DiscoveryProviders {
		h.discoveryProviders[id] = true
	}
	for k, v := range o.BackendLoads {
		h.backendLoads[k] = v
	}
	for k, v := range o.HTTPClients {
		h.httpClients[k] = v
	}
	for _, p := range o.Providers {
		h.providers[p.ID] = p
	}
	if h.http == nil {
		h.http = providerHTTP()
	}
	for _, p := range o.Providers {
		if h.httpClients[p.ID] == nil {
			if o.HTTP != nil {
				h.httpClients[p.ID] = o.HTTP
			} else {
				h.httpClients[p.ID] = providerHTTP()
			}
		}
	}
	if h.failures <= 0 {
		h.failures = DefaultBreakerFailures
	}
	if h.coolOff <= 0 {
		h.coolOff = DefaultBreakerCoolOff
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	return h
}

// The reasons a chain passes over an entry, as airut_model_fallthrough_total
// counts them. Each failure class is also an errClass.
const (
	reasonUnavailable = "unavailable"
	reasonBreakerOpen = "breaker_open"
	reasonTooLarge    = "too_large"
)

// The classes of a failed model request. The first five say the model
// could not answer now, so a chain falls through to its next entry and
// the breaker counts the failure. The rest stop the chain.
const (
	classConnect    = "connect"
	class5xx        = "5xx"
	class429        = "429"
	classTimeout    = "timeout"
	classNotFound   = "not_found"
	classBadRequest = "bad_request"
	classCanceled   = "canceled"
	classOther      = "other"
)

// fallsThrough reports whether a failure of class moves a chain on.
func fallsThrough(class string) bool {
	switch class {
	case classConnect, class5xx, class429, classTimeout, classNotFound:
		return true
	}
	return false
}

// usable reports why the entry for served on providerID cannot take a
// request now, or "" when it can. A half-open breaker lets this caller's
// request through as its probe, which done ends.
func (h *Health) usable(ctx context.Context, providerID, served string) string {
	k := ModelKey{providerID, served}
	if !h.breakerAllows(k, false) {
		return reasonBreakerOpen
	}
	if !h.available(ctx, providerID, served) {
		if !h.breakerAllows(k, false) {
			return reasonBreakerOpen
		}
		return reasonUnavailable
	}
	if !h.breakerAllows(k, true) {
		return reasonBreakerOpen
	}
	return ""
}

func (h *Health) breakerAllows(k ModelKey, reserve bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.breakers[servedModel{k.Provider, k.Served}]
	if b == nil || b.openedAt.IsZero() {
		return true
	}
	if b.probing || h.now().Sub(b.openedAt) < h.coolOff {
		return false
	}
	if reserve {
		b.probing = true
	}
	return true
}

// done records how a request to served on providerID ended, by its
// failure class, or "" for a success. A failure that falls through
// counts towards the breaker; a success closes it. Any other failure is
// about the request, not the model, and shows the server answered, so it
// closes the breaker too, except a cancellation, which shows nothing.
func (h *Health) done(providerID, served, class string) {
	key := servedModel{providerID, served}
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.breakers[key]
	switch {
	case class == classCanceled:
		if b != nil {
			b.probing = false
		}
		return
	case !fallsThrough(class):
		delete(h.breakers, key)
		return
	}
	if b == nil {
		b = &breaker{}
		h.breakers[key] = b
	}
	b.fails++
	half := b.probing
	b.probing = false
	if half || b.fails >= h.failures {
		b.openedAt = h.now()
		h.log.Info("model breaker opened", "provider", providerID, "served", served,
			"failures", b.fails, "class", class, "cool_off_ms", h.coolOff.Milliseconds())
	}
}

// available reports whether providerID says it can serve served now.
func (h *Health) available(ctx context.Context, providerID, served string) bool {
	k := ModelKey{providerID, served}
	s := h.snapshot(ctx, []ModelKey{k})
	ok := ctx.Err() == nil && h.choose(s, []ModelKey{k}, []bool{false}) == 0
	kind := decisionUnreachable
	if ready(s, k) {
		kind = decisionLoaded
	} else if routes := s.Routes[k]; len(routes) > 0 {
		b := s.Backends[routes[0].Backend]
		if b.Models[routes[0].Served] == StateUnloaded {
			kind = decisionNotLoaded
			p := h.providers[providerID]
			if p.Load == config.LoadWhenIdle {
				kind = decisionBusyOtherModel
				if ok {
					kind = decisionLoadingWhenIdle
				}
			}
			if ok && p.Load == config.LoadAlways {
				kind = decisionLoadingAlways
			}
		}
	}
	h.log.Debug("model availability", "provider", providerID, "served", served, "decision", kind, "best_effort", true)
	return ok
}

// The kinds of availability decision, as the log names them.
const (
	decisionLoaded          = "loaded"
	decisionLoadingWhenIdle = "loading_when_idle"
	decisionLoadingAlways   = "loading_always"
	decisionNotLoaded       = "not_loaded"
	decisionBusyOtherModel  = "busy_other_model"
	decisionUnreachable     = "unreachable"
)

// runningState reads an entire bounded llama-swap inventory. A missing,
// malformed or non-ready process state never establishes ready or idle.
func (h *Health) runningState(ctx context.Context, p config.Provider) BackendState {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.RunningURL(), nil)
	if err != nil {
		return BackendState{}
	}
	req.Header.Set("Cache-Control", "no-cache, no-store")
	client := h.httpClients[p.ID]
	if client == nil {
		client = h.http
	}
	// Discovery never follows redirects, including on a supplied client.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		return BackendState{}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || resp.StatusCode/100 != 2 {
		return BackendState{}
	}
	var out struct {
		Running *[]struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if json.Unmarshal(data, &out) != nil || out.Running == nil || duplicateFields(data) {
		return BackendState{}
	}
	b := BackendState{Models: map[string]ModelState{}, Idle: len(*out.Running) == 0}
	for _, m := range *out.Running {
		if m.Model == "" {
			return BackendState{}
		}
		if _, exists := b.Models[m.Model]; exists {
			return BackendState{}
		}
		state := StateUnknown
		switch m.State {
		case "ready":
			state = StateReady
		case "starting":
			state = StateLoading
		}
		b.Models[m.Model] = state
	}
	return b
}

// Reject duplicate keys (including case aliases accepted by encoding/json),
// trailing values and deeply nested JSON instead of accepting a last-key win.
func duplicateFields(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		t, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return true
		}
		if delim != '{' && delim != '[' {
			return false
		}
		seen := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				if err != nil {
					return false
				}
				k, ok := key.(string)
				if !ok {
					return false
				}
				k = strings.ToLower(k)
				if seen[k] {
					return false
				}
				seen[k] = true
			}
			if !value(depth + 1) {
				return false
			}
		}
		_, err = d.Token()
		return err == nil
	}
	if !value(0) {
		return true
	}
	_, err := d.Token()
	return err != io.EOF
}

// classify names the class of a failed model request. parent is the
// caller's context: a request it ended was canceled, while one that ran
// out its own time timed out.
func classify(parent context.Context, err error) string {
	var physical *physicalFailure
	var se *statusError
	var pe *provider.ProviderError
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case parent.Err() != nil:
		return classCanceled
	case errors.As(err, &physical):
		return physical.class
	case errors.As(err, &se):
		return statusClass(se.code, se.notFound)
	case errors.As(err, &pe) && pe.Status != 0:
		return statusClass(pe.Status, mentionsMissingModel(pe.Msg))
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return classTimeout
	case errors.As(err, &pe) && mentionsMissingModel(pe.Msg):
		return classNotFound
	case errors.As(err, &pe) && pe.Transient, provider.IsTransportError(err):
		// A stream that died before it said anything, or a connection
		// that failed, refused, or reset.
		return classConnect
	case errors.Is(err, context.Canceled):
		return classCanceled
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return classConnect
	}
	return classOther
}

// statusClass is the class of an HTTP status. notFound says the body
// named a missing model, as some servers do with a 400.
func statusClass(code int, notFound bool) string {
	switch {
	case code == http.StatusNotFound, notFound && code/100 == 4:
		return classNotFound
	case code == http.StatusTooManyRequests:
		return class429
	case code == http.StatusRequestTimeout, code == http.StatusGatewayTimeout:
		return classTimeout
	case code >= 500:
		return class5xx
	case code/100 == 4:
		return classBadRequest
	}
	return classOther
}

// mentionsMissingModel reports whether a server's error text says the
// model it was asked for does not exist. The text is read, never kept
// or logged: a server may echo the request in it.
func mentionsMissingModel(msg string) bool {
	m := strings.ToLower(msg)
	if strings.Contains(m, "model_not_found") {
		return true
	}
	if !strings.Contains(m, "model") {
		return false
	}
	for _, s := range []string{"not found", "does not exist", "no such model", "unknown model", "could not find"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// skipped counts and logs an entry a chain passed over for reason, to
// try its next one, at use "agent" or "judge".
func skipped(c *airut.Ctx, log *slog.Logger, use, providerID, alias, served, reason string) {
	c.Metrics().Add("airut_model_fallthrough_total", 1, "use", use, "from", alias, "reason", reason)
	log.Info("model fell through", "invocation", c.ID(), "use", use, "provider", providerID,
		"model", alias, "served", served, "reason", reason)
}
