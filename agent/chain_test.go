package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
	"terva.sh/airut/config"
)

// llamaSwap is a fake llama-swap's /running. It counts the requests it
// answers, and answers with status when that is set.
type llamaSwap struct {
	mu      sync.Mutex
	running []string
	status  int
	body    string // answered as it is, when set
	asked   int
	url     string
	close   func()
}

func newLlamaSwap(t *testing.T, running ...string) *llamaSwap {
	t.Helper()
	ls := &llamaSwap{running: running}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			http.NotFound(w, r)
			return
		}
		ls.mu.Lock()
		ls.asked++
		status, running, body := ls.status, ls.running, ls.body
		ls.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
			return
		}
		list := []map[string]string{}
		for _, m := range running {
			list = append(list, map[string]string{"model": m, "state": "ready"})
		}
		json.NewEncoder(w).Encode(map[string]any{"running": list})
	}))
	t.Cleanup(srv.Close)
	ls.url, ls.close = srv.URL, srv.Close
	return ls
}

func (ls *llamaSwap) set(running ...string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.running = running
}

func (ls *llamaSwap) requests() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.asked
}

// provider is a llama-swap provider of the fake, with load policy load.
func (ls *llamaSwap) provider(id, load string) config.Provider {
	return config.Provider{ID: id, BaseURL: ls.url + "/v1", Kind: config.KindLlamaSwap, Load: load}
}

// testClock is a test testClock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logBuffer is a debug logger's output.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) logger() *slog.Logger { return slogTo(l.write) }

func (l *logBuffer) write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines returns the log's lines with message msg.
func (l *logBuffer) lines(msg string) []string {
	var got []string
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			got = append(got, line)
		}
	}
	return got
}

func TestLlamaSwapAvailability(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		load    string
		running []string
		ok      bool
		kind    string
	}{
		{config.LoadNever, []string{"m"}, true, "loaded"},
		{config.LoadNever, []string{"other", "m"}, true, "loaded"},
		{config.LoadNever, nil, false, "not_loaded"},
		{config.LoadNever, []string{"other"}, false, "not_loaded"},
		{config.LoadWhenIdle, []string{"m"}, true, "loaded"},
		{config.LoadWhenIdle, nil, true, "loading_when_idle"},
		{config.LoadWhenIdle, []string{"other"}, false, "busy_other_model"},
		{config.LoadAlways, []string{"other"}, true, "loading_always"},
		{config.LoadAlways, nil, true, "loading_always"},
	} {
		ls := newLlamaSwap(t, c.running...)
		var logs logBuffer
		h := NewHealth(HealthOptions{Providers: []config.Provider{ls.provider("ls", c.load)}, Logger: logs.logger()})
		if got := h.available(ctx, "ls", "m"); got != c.ok {
			t.Errorf("%s with %v running: available = %v, want %v", c.load, c.running, got, c.ok)
		}
		line := logs.lines("model availability")
		if len(line) != 1 || !strings.Contains(line[0], "decision="+c.kind) || !strings.Contains(line[0], "provider=ls") ||
			!strings.Contains(line[0], "served=m") || !strings.Contains(line[0], "level=DEBUG") {
			t.Errorf("%s with %v running logged %v, want decision=%s", c.load, c.running, line, c.kind)
		}
	}

	// Unreachable is unavailable, under every policy: a server that fails,
	// one that answers something else, and one that is gone.
	for _, load := range []string{config.LoadNever, config.LoadWhenIdle, config.LoadAlways} {
		failing := newLlamaSwap(t, "m")
		failing.status = http.StatusInternalServerError
		gone := newLlamaSwap(t, "m")
		gone.close()
		for _, ls := range []*llamaSwap{failing, gone} {
			var logs logBuffer
			h := NewHealth(HealthOptions{Providers: []config.Provider{ls.provider("ls", load)}, Logger: logs.logger()})
			if h.available(ctx, "ls", "m") || !strings.Contains(logs.String(), "decision=unreachable") {
				t.Errorf("%s: an unreachable llama-swap is available, or logged %s", load, logs.String())
			}
		}
	}

	// A 200 that does not carry the list is no answer, so it never reads
	// as idle: when_idle and never refuse, and the kind is unreachable.
	for _, body := range []string{`{}`, `null`, `{"running":null}`, `{"models":["m"]}`, `[]`, `{"running":"m"}`, `not json`} {
		for _, load := range []string{config.LoadNever, config.LoadWhenIdle} {
			ls := newLlamaSwap(t)
			ls.body = body
			var logs logBuffer
			h := NewHealth(HealthOptions{Providers: []config.Provider{ls.provider("ls", load)}, Logger: logs.logger()})
			if h.available(ctx, "ls", "m") || !strings.Contains(logs.String(), "decision=unreachable") {
				t.Errorf("%s: the answer %s made the model available, or logged %s", load, body, logs.String())
			}
		}
	}
	// An empty list is idle.
	empty := newLlamaSwap(t)
	empty.body = `{"running":[]}`
	if h := NewHealth(HealthOptions{Providers: []config.Provider{empty.provider("ls", config.LoadWhenIdle)}}); !h.available(ctx, "ls", "m") {
		t.Error("an empty running list is not idle")
	}

	// A provider of another kind, or one Health was not given, is always
	// available, and nobody is asked.
	ls := newLlamaSwap(t)
	h := NewHealth(HealthOptions{Providers: []config.Provider{{ID: "plain", BaseURL: ls.url + "/v1", Kind: config.KindOpenAI}}})
	if !h.available(ctx, "plain", "m") || !h.available(ctx, "unknown", "m") || ls.requests() != 0 {
		t.Errorf("an OpenAI provider is unavailable, or /running was asked %d times", ls.requests())
	}
}

func TestLlamaSwapNeverCachesState(t *testing.T) {
	ctx := context.Background()
	ls := newLlamaSwap(t, "m")
	h := NewHealth(HealthOptions{Providers: []config.Provider{ls.provider("ls", config.LoadNever)}})
	if !h.available(ctx, "ls", "m") {
		t.Fatal("not ready")
	}
	ls.set("other")
	if h.available(ctx, "ls", "m") {
		t.Fatal("stale ready")
	}
	ls.set("m")
	if !h.available(ctx, "ls", "m") {
		t.Fatal("stale negative")
	}
	ls.status = 503
	if h.available(ctx, "ls", "m") {
		t.Fatal("failed discovery")
	}
	ls.status = 0
	if !h.available(ctx, "ls", "m") {
		t.Fatal("cached failure")
	}
	if ls.requests() != 5 {
		t.Fatalf("got %d reads", ls.requests())
	}
}

func TestBreaker(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{t: time.Unix(1000, 0)}
	var logs logBuffer
	h := NewHealth(HealthOptions{Now: clk.now, Logger: logs.logger()})
	if DefaultBreakerFailures != 3 || DefaultBreakerCoolOff != 30*time.Second {
		t.Fatalf("the defaults changed: %d failures, %v", DefaultBreakerFailures, DefaultBreakerCoolOff)
	}

	// A request-specific failure never opens it, and a success resets the
	// count.
	for range 5 {
		h.done("p", "m", classBadRequest)
	}
	h.done("p", "m", class5xx)
	h.done("p", "m", class5xx)
	h.done("p", "m", "")
	h.done("p", "m", classTimeout)
	h.done("p", "m", classConnect)
	if why := h.usable(ctx, "p", "m"); why != "" {
		t.Fatalf("the breaker opened early: %s", why)
	}
	h.done("p", "m", "") // the request usable let through

	// Three failures in a row open it, for that served model only.
	for _, c := range []string{class429, classNotFound, class5xx} {
		h.done("p", "m", c)
	}
	if why := h.usable(ctx, "p", "m"); why != reasonBreakerOpen {
		t.Errorf("after 3 failures usable = %q", why)
	}
	if why := h.usable(ctx, "p", "other"); why != "" {
		t.Errorf("another model's breaker is %q", why)
	}
	if line := logs.lines("model breaker opened"); len(line) != 1 || !strings.Contains(line[0], "served=m") {
		t.Errorf("the open was logged as %v", line)
	}

	// After the cool-off it is half-open: one probe goes, others wait.
	clk.advance(29 * time.Second)
	if h.usable(ctx, "p", "m") != reasonBreakerOpen {
		t.Error("the breaker closed before its cool-off")
	}
	clk.advance(time.Second)
	if why := h.usable(ctx, "p", "m"); why != "" {
		t.Fatalf("half-open, the probe was refused: %s", why)
	}
	if h.usable(ctx, "p", "m") != reasonBreakerOpen {
		t.Error("half-open, a second request went with the probe")
	}
	// A failed probe opens it again at once, for another cool-off.
	h.done("p", "m", classConnect)
	clk.advance(15 * time.Second)
	if h.usable(ctx, "p", "m") != reasonBreakerOpen {
		t.Error("a failed probe did not reopen the breaker")
	}
	clk.advance(15 * time.Second)
	// A probe handed back unsent leaves it half-open for the next one.
	if h.usable(ctx, "p", "m") != "" {
		t.Fatal("no probe after the second cool-off")
	}
	h.done("p", "m", classCanceled)
	if h.usable(ctx, "p", "m") != "" {
		t.Fatal("a handed-back probe kept the breaker shut")
	}
	// A probe that succeeds closes it.
	h.done("p", "m", "")
	for range 3 {
		if why := h.usable(ctx, "p", "m"); why != "" {
			t.Fatalf("after a good probe usable = %q", why)
		}
	}
}

// roundTrip is an http.RoundTripper of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A breaker that opens while /running is asked refuses the request, even
// though the model is loaded and the breaker was closed when it was
// first checked.
func TestBreakerOpensDuringAvailability(t *testing.T) {
	ctx := context.Background()
	ls := newLlamaSwap(t, "m")
	var h *Health
	h = NewHealth(HealthOptions{
		Providers: []config.Provider{ls.provider("p", config.LoadNever)},
		HTTP: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			for range DefaultBreakerFailures {
				h.done("p", "m", class5xx)
			}
			return http.DefaultTransport.RoundTrip(r)
		})},
	})
	if why := h.usable(ctx, "p", "m"); why != reasonBreakerOpen {
		t.Errorf("usable = %q after the breaker opened mid-check, want %q", why, reasonBreakerOpen)
	}
	if ls.requests() != 1 {
		t.Errorf("/running was asked %d times, want 1", ls.requests())
	}
}

func TestClassify(t *testing.T) {
	live := context.Background()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name   string
		parent context.Context
		err    error
		want   string
	}{
		{"judge 404", live, &statusError{code: 404}, classNotFound},
		{"judge 400 naming a missing model", live, &statusError{code: 400, notFound: true}, classNotFound},
		{"judge 400", live, &statusError{code: 400}, classBadRequest},
		{"judge 413", live, &statusError{code: 413}, classBadRequest},
		{"judge 429", live, &statusError{code: 429}, class429},
		{"judge 503", live, &statusError{code: 503}, class5xx},
		{"judge 408", live, &statusError{code: 408}, classTimeout},
		{"terva 500", live, provider.NewHTTPError("x", 500, "", "oops"), class5xx},
		{"terva 404", live, provider.NewHTTPError("x", 404, "", ""), classNotFound},
		{"terva 400 naming a missing model", live, provider.NewHTTPError("x", 400, "", `{"error":"model 'q' not found"}`), classNotFound},
		{"terva 400", live, provider.NewHTTPError("x", 400, "", "too long"), classBadRequest},
		{"terva 401", live, provider.NewHTTPError("x", 401, "", ""), classBadRequest},
		{"terva 429", live, provider.NewHTTPError("x", 429, "", ""), class429},
		{"terva in-stream missing model", live, provider.NewAPIError("x", "The model `q` does not exist", false), classNotFound},
		{"terva transient in-stream", live, provider.NewAPIError("x", "overloaded", true), classConnect},
		{"terva permanent in-stream", live, provider.NewAPIError("x", "content filter", false), classOther},
		{"refused", live, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, classConnect},
		{"wrapped refusal", live, errors.Join(errors.New("post"), syscall.ECONNRESET), classConnect},
		{"own deadline", live, context.DeadlineExceeded, classTimeout},
		{"caller gone", gone, context.DeadlineExceeded, classCanceled},
		{"caller gone mid 5xx", gone, &statusError{code: 503}, classCanceled},
		{"canceled", live, context.Canceled, classCanceled},
		{"other", live, errors.New("agent: the judge's reply has no choices"), classOther},
	} {
		if got := classify(c.parent, c.err); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
	for _, c := range []string{classConnect, class5xx, class429, classTimeout, classNotFound} {
		if !fallsThrough(c) {
			t.Errorf("%s does not fall through", c)
		}
	}
	for _, c := range []string{classBadRequest, classCanceled, classOther} {
		if fallsThrough(c) {
			t.Errorf("%s falls through", c)
		}
	}
}

// flaky fails its next requests as scripted, then hands the rest to its
// fake.
type flaky struct {
	*fake
	mu    sync.Mutex
	fails []func(provider.Request) (<-chan provider.Event, error)
	calls []string
	// stalls is how many of the next requests say nothing until they
	// end, then close without an end, as a client may when canceled.
	stalls int
}

func (f *flaky) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Model)
	var next func(provider.Request) (<-chan provider.Event, error)
	stall := f.stalls > 0
	if stall {
		f.stalls--
	} else if len(f.fails) > 0 {
		next, f.fails = f.fails[0], f.fails[1:]
	}
	f.mu.Unlock()
	if stall {
		ch := make(chan provider.Event, 1)
		ch <- provider.EventStart{Model: req.Model}
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	if next != nil {
		return next(req)
	}
	return f.fake.Stream(ctx, req)
}

func (f *flaky) fail(fs ...func(provider.Request) (<-chan provider.Event, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = append(f.fails, fs...)
}

func (f *flaky) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// refuse fails a request before it starts.
func refuse(err error) func(provider.Request) (<-chan provider.Event, error) {
	return func(provider.Request) (<-chan provider.Event, error) { return nil, err }
}

// dies fails a request after it starts, having first streamed text, if
// any.
func dies(text string, err error) func(provider.Request) (<-chan provider.Event, error) {
	return func(req provider.Request) (<-chan provider.Event, error) {
		ch := make(chan provider.Event, 4)
		ch <- provider.EventStart{Model: req.Model}
		if text != "" {
			ch <- provider.EventTextDelta{Delta: text}
		}
		ch <- provider.EventUsage{}
		ch <- provider.EventDone{Err: err}
		close(ch)
		return ch, nil
	}
}

// chainEnv is a bot whose agent walks a chain of two fake providers:
// "one" on provider a, served as m1, then "two" on b, served as m2.
type chainEnv struct {
	a, b  *flaky
	agent *Agent
	bot   *airut.Bot
	logs  *logBuffer
	clk   *testClock
}

func newChainEnv(t *testing.T, providers []config.Provider, turn airut.Turn) *chainEnv {
	t.Helper()
	e := &chainEnv{a: &flaky{fake: &fake{}}, b: &flaky{fake: &fake{}}, logs: &logBuffer{}, clk: &testClock{t: time.Unix(1000, 0)}}
	reg := provider.NewRegistry()
	reg.RegisterExtraModel(provider.Model{Provider: "a", ID: "m1", ContextWindow: 32000, MaxOutput: 1024})
	reg.RegisterExtraModel(provider.Model{Provider: "b", ID: "m2", ContextWindow: 32000, MaxOutput: 1024})
	log := e.logs.logger()
	ag, err := New(Options{
		Clients: map[string]provider.Client{"a": e.a, "b": e.b},
		Catalog: reg,
		Models: config.Chain{
			{Provider: "a", Model: "one", Served: "m1"},
			{Provider: "b", Model: "two", Served: "m2", Capabilities: config.Capabilities{MaxOutput: 512}},
		},
		Health:   NewHealth(HealthOptions{Providers: providers, Now: e.clk.now, Logger: log}),
		Sessions: openDB(t, filepath.Join(t.TempDir(), "sessions.db")),
		Logger:   log,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.agent = ag
	e.bot = airut.New(airut.Config{Agent: ag, Gate: allow, EditInterval: time.Nanosecond})
	e.bot.Command("ask", "Ask", func(c *airut.Ctx) error { return c.Agent(turn).StreamTo(c) })
	return e
}

func (e *chainEnv) fellThrough(from, reason string) float64 {
	s, _ := e.bot.Metrics().Get("airut_model_fallthrough_total", "use", "agent", "from", from, "reason", reason)
	return s.Value
}

func TestAgentFallsThroughBeforeOutput(t *testing.T) {
	for _, c := range []struct {
		name   string
		fail   func(provider.Request) (<-chan provider.Event, error)
		reason string
	}{
		{"connect", refuse(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), classConnect},
		{"5xx", refuse(provider.NewHTTPError("a", 503, "", "the prompt was: secret prompt")), class5xx},
		{"429", refuse(provider.NewHTTPError("a", 429, "", "")), class429},
		{"not found", refuse(provider.NewHTTPError("a", 404, "", "")), classNotFound},
		{"timeout", refuse(context.DeadlineExceeded), classTimeout},
		{"in-stream before output", dies("", provider.NewAPIError("a", "model m1 not found", false)), classNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newChainEnv(t, nil, airut.Turn{Prompt: "secret prompt"})
			e.a.fail(c.fail)
			e.b.play(say("from two"))
			evs := airuttest.Start(t, e.bot).Text("!ask")
			if got := final(evs); got != "from two" {
				t.Errorf("answer = %q", got)
			}
			if a, b := e.a.requests(), e.b.requests(); len(a) != 1 || a[0] != "m1" || len(b) != 1 || b[0] != "m2" {
				t.Errorf("requests: a %v, b %v", a, b)
			}
			// The next entry runs under its own served id and output cap.
			if r := e.b.fake.requests(); len(r) != 1 || r[0].MaxTokens != 512 {
				t.Errorf("b's request: %+v", r)
			}
			if n := e.fellThrough("one", c.reason); n != 1 {
				t.Errorf("airut_model_fallthrough_total{from=one,reason=%s} = %v", c.reason, n)
			}
			req := e.logs.lines("agent request")
			fin := e.logs.lines("agent turn finished")
			fell := e.logs.lines("model fell through")
			if len(req) != 2 || !strings.Contains(req[1], "model=two") || !strings.Contains(req[1], "served=m2") ||
				!strings.Contains(req[1], "provider=b") || !strings.Contains(req[1], "fell_through=1") {
				t.Errorf("the request lines: %v", req)
			}
			if len(fin) != 1 || !strings.Contains(fin[0], "model=two") || !strings.Contains(fin[0], "served=m2") ||
				!strings.Contains(fin[0], "fell_through=1") || !strings.Contains(fin[0], "stop=done") {
				t.Errorf("the finished lines: %v", fin)
			}
			if len(fell) != 1 || !strings.Contains(fell[0], "model=one") || !strings.Contains(fell[0], "served=m1") ||
				!strings.Contains(fell[0], "reason="+c.reason) || !strings.Contains(fell[0], "use=agent") {
				t.Errorf("the fall-through lines: %v", fell)
			}
			if s := e.logs.String(); strings.Contains(s, "secret") || strings.Contains(s, "from two") {
				t.Errorf("text reached the log:\n%s", s)
			}
		})
	}
}

func TestAgentStopsOnABadRequest(t *testing.T) {
	e := newChainEnv(t, nil, airut.Turn{Prompt: "p"})
	e.a.fail(refuse(provider.NewHTTPError("a", 400, "", "bad")))
	airuttest.Start(t, e.bot).Text("!ask")
	if b := e.b.requests(); len(b) != 0 {
		t.Errorf("a bad request fell through to b: %v", b)
	}
	if fin := e.logs.lines("agent turn finished"); len(fin) != 1 || !strings.Contains(fin[0], "stop=error") ||
		!strings.Contains(fin[0], "fell_through=0") {
		t.Errorf("the finished lines: %v", fin)
	}
}

func TestAgentDoesNotFallThroughAfterOutput(t *testing.T) {
	e := newChainEnv(t, nil, airut.Turn{Prompt: "p"})
	// A 5xx, but after text reached the chat; not transient, so terva
	// does not retry it either.
	e.a.fail(dies("half an answer", &provider.ProviderError{Status: 503, Msg: "gone"}))
	airuttest.Start(t, e.bot).Text("!ask")
	if b := e.b.requests(); len(b) != 0 {
		t.Errorf("the turn fell through after output: %v", b)
	}
	if fin := e.logs.lines("agent turn finished"); len(fin) != 1 || !strings.Contains(fin[0], "stop=error") ||
		!strings.Contains(fin[0], "model=one") {
		t.Errorf("the finished lines: %v", fin)
	}

	// A completed tool batch is now a fresh request and may fall through.
	e2 := newChainEnv(t, nil, airut.Turn{Prompt: "p", Tools: nil})
	e2.bot.Command("roll", "Roll", func(c *airut.Ctx) error { _, err := c.Reply("rolled"); return err }).AsTool()
	e2.bot.Command("askroll", "Ask", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "p", Tools: e2.bot.Tools("roll")}).StreamTo(c)
	})
	e2.a.play(call("c1", "roll", `{}`))
	e2.a.fail(func(req provider.Request) (<-chan provider.Event, error) {
		// The first request goes to the fake; this fails the second.
		return e2.a.fake.Stream(context.Background(), req)
	}, refuse(provider.NewHTTPError("a", 404, "", "")))
	airuttest.Start(t, e2.bot).Text("!askroll")
	if a, b := e2.a.requests(), e2.b.requests(); len(a) != 2 || len(b) != 1 {
		t.Errorf("after a tool ran: a %v, b %v", a, b)
	}
}

func TestAgentSkipsUnavailableAndOpenBreakers(t *testing.T) {
	ls := newLlamaSwap(t, "something-else")
	e := newChainEnv(t, []config.Provider{ls.provider("a", config.LoadWhenIdle)}, airut.Turn{Prompt: "p"})
	e.b.play(say("ok"))
	airuttest.Start(t, e.bot).Text("!ask")
	if a, b := e.a.requests(), e.b.requests(); len(a) != 0 || len(b) != 1 {
		t.Errorf("a busy llama-swap: a %v, b %v", a, b)
	}
	if n := e.fellThrough("one", reasonUnavailable); n != 1 {
		t.Errorf("unavailable counted %v", n)
	}

	// With every entry unavailable the turn fails without a request.
	e2 := newChainEnv(t, []config.Provider{ls.provider("a", config.LoadNever), ls.provider("b", config.LoadNever)}, airut.Turn{Prompt: "p"})
	airuttest.Start(t, e2.bot).Text("!ask")
	if a, b := e2.a.requests(), e2.b.requests(); len(a) != 0 || len(b) != 0 {
		t.Errorf("with nothing available: a %v, b %v", a, b)
	}
	if fin := e2.logs.lines("agent turn finished"); len(fin) != 1 || !strings.Contains(fin[0], "stop=error") {
		t.Errorf("the finished lines: %v", fin)
	}

	// Three turns that fail on a open its breaker; the fourth skips it.
	e3 := newChainEnv(t, nil, airut.Turn{Prompt: "p"})
	ad := airuttest.Start(t, e3.bot)
	for range 3 {
		e3.a.fail(refuse(provider.NewHTTPError("a", 502, "", "")))
		e3.b.play(say("ok"))
		ad.Text("!ask")
	}
	e3.b.play(say("ok"))
	ad.Text("!ask")
	if a, b := e3.a.requests(), e3.b.requests(); len(a) != 3 || len(b) != 4 {
		t.Errorf("with a's breaker open: a %v, b %v", a, b)
	}
	if n, m := e3.fellThrough("one", class5xx), e3.fellThrough("one", reasonBreakerOpen); n != 3 || m != 1 {
		t.Errorf("fell through %v times on 5xx and %v on the breaker", n, m)
	}
	// After the cool-off a probes again, and its success closes it.
	e3.clk.advance(DefaultBreakerCoolOff)
	e3.a.play(say("ok"))
	ad.Text("!ask")
	if a := e3.a.requests(); len(a) != 4 {
		t.Errorf("the half-open breaker sent no probe: %v", a)
	}
}

func TestAgentFallThroughKeepsTheSessionWhole(t *testing.T) {
	e := newChainEnv(t, nil, airut.Turn{Prompt: "remember 7", Session: airut.PerChannel})
	e.a.fail(refuse(provider.NewHTTPError("a", 503, "", "")))
	e.b.play(say("noted"))
	ad := airuttest.Start(t, e.bot)
	ad.Text("!ask")
	// a is healthy again, so the next turn runs there.
	e.a.play(say("7"))
	ad.Text("!ask")
	reqs := e.a.fake.requests()
	if len(reqs) != 1 || len(e.b.fake.requests()) != 1 {
		t.Fatalf("%d requests to a, %d to b", len(reqs), len(e.b.fake.requests()))
	}
	users := 0
	for _, m := range reqs[0].Messages {
		if m.Role == provider.RoleUser {
			users++
		}
	}
	// The first turn's prompt once, and the second's.
	if users != 2 {
		t.Errorf("the resumed session holds %d user messages, want 2", users)
	}
}

// endless streams text until its request is canceled, and then closes.
type endless struct{ closed chan struct{} }

func (endless) Name() string { return "endless" }

func (f endless) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event)
	go func() {
		defer close(f.closed)
		defer close(ch)
		ch <- provider.EventStart{Model: req.Model}
		for {
			select {
			case ch <- provider.EventTextDelta{Delta: "more"}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// A stream the engine stops reading, as when the turn is stopped, ends:
// its forwarding goroutine closes its channel, cancels the request, and
// hands back a half-open breaker's probe.
func TestAgentStreamTheEngineAbandons(t *testing.T) {
	clk := &testClock{t: time.Unix(1000, 0)}
	h := NewHealth(HealthOptions{Now: clk.now})
	for range DefaultBreakerFailures {
		h.done("a", "m1", class5xx)
	}
	clk.advance(DefaultBreakerCoolOff)
	before := runtime.NumGoroutine()

	up := endless{closed: make(chan struct{})}
	chain := config.Chain{{Provider: "a", Model: "one", Served: "m1"}}
	tc := &turnClient{health: h, chain: chain, clients: map[string]provider.Client{"a": up}, tm: &timing{}}
	ctx, cancel := context.WithCancel(context.Background())
	if why := h.usable(ctx, "a", "m1"); why != "" {
		t.Fatalf("no probe: %s", why)
	}
	out, err := tc.attempt(ctx, chain[0], provider.Request{Model: "m1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	// Read a little, then stop reading for good, long enough for the
	// stream to fill what out buffers, and stop the turn.
	<-out
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-up.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the request was never canceled")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.usable(context.Background(), "a", "m1") != "" {
		if time.Now().After(deadline) {
			t.Fatal("the probe is still held")
		}
		time.Sleep(time.Millisecond)
	}
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines, %d before", runtime.NumGoroutine(), before)
		}
		time.Sleep(time.Millisecond)
	}
	// out was closed, whatever it still buffered.
	for range out {
	}
}

// stalls says something, then waits for its request to end and closes
// its stream without an end, as a client may when canceled.
type stalls struct{}

func (stalls) Name() string { return "stalls" }

func (stalls) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event)
	go func() {
		defer close(ch)
		ch <- provider.EventStart{Model: req.Model}
		ch <- provider.EventTextDelta{Delta: "the start of"}
		<-ctx.Done()
	}()
	return ch, nil
}

// A request the entry's own timeout cuts off after some output ends in
// the timeout, not as a whole answer, and counts as a timeout.
func TestAgentStreamTimesOutAfterOutput(t *testing.T) {
	h := NewHealth(HealthOptions{})
	chain := config.Chain{{Provider: "a", Model: "one", Served: "m1", Capabilities: config.Capabilities{Timeout: 20 * time.Millisecond}}}
	for i := range DefaultBreakerFailures {
		tc := &turnClient{health: h, chain: chain, clients: map[string]provider.Client{"a": stalls{}}, tm: &timing{}}
		out, err := tc.attempt(context.Background(), chain[0], provider.Request{Model: "m1"}, true)
		if err != nil {
			t.Fatal(err)
		}
		var last provider.Event
		for ev := range out {
			last = ev
		}
		if d, ok := last.(provider.EventDone); !ok || !errors.Is(d.Err, context.DeadlineExceeded) {
			t.Fatalf("request %d ended with %#v, want the timeout", i, last)
		}
	}
	if why := h.usable(context.Background(), "a", "m1"); why != reasonBreakerOpen {
		t.Errorf("after %d timeouts usable = %q", DefaultBreakerFailures, why)
	}
}

// A request the entry's own timeout ends before anything is said, on a
// stream that closes without its end, falls through to the next entry.
func TestAgentFallsThroughOnATimeoutBeforeOutput(t *testing.T) {
	e := newChainEnv(t, nil, airut.Turn{Prompt: "tag it"})
	e.agent.chain[0].Timeout = 20 * time.Millisecond
	e.a.stalls = 1
	e.b.play(say("from two"))
	evs := airuttest.Start(t, e.bot).Text("!ask")
	if got := final(evs); got != "from two" {
		t.Errorf("answer = %q", got)
	}
	if n := e.fellThrough("one", classTimeout); n != 1 {
		t.Errorf("airut_model_fallthrough_total{from=one,reason=timeout} = %v", n)
	}
}
