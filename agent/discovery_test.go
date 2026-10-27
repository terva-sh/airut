package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
	"terva.sh/airut/config"
	"terva.sh/terva/packages/provider"
)

type discoveryFunc func(context.Context, []ModelKey) (Snapshot, error)

func (f discoveryFunc) Snapshot(ctx context.Context, keys []ModelKey) (Snapshot, error) {
	return f(ctx, keys)
}

var selectionKeys = []ModelKey{{"a", "m1"}, {"b", "m2"}}

func selectionSnapshot(a, b ModelState) Snapshot {
	return Snapshot{Routes: map[ModelKey][]Route{
		selectionKeys[0]: {{Backend: "gpu", Served: "actual-1"}},
		selectionKeys[1]: {{Backend: "gpu", Served: "actual-2"}},
	}, Backends: map[string]BackendState{"gpu": {Models: map[string]ModelState{"actual-1": a, "actual-2": b}, Idle: a == StateUnloaded && b == StateUnloaded}}}
}
func healthWithSnapshot(s Snapshot) *Health {
	return NewHealth(HealthOptions{Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) { return s, nil }), BackendLoads: map[string]string{"gpu": config.LoadAlways}})
}
func TestDiscoveryTwoPassAndAuthority(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b ModelState
		want int
	}{
		{"first ready", StateReady, StateUnloaded, 0},
		{"second ready before cold first", StateUnloaded, StateReady, 1},
		{"ready ties", StateReady, StateReady, 0},
		{"cold first", StateUnloaded, StateUnloaded, 0},
		{"loading prevents load", StateLoading, StateUnloaded, -1},
		{"unknown prevents load", StateUnloaded, StateUnknown, -1},
		{"unknown does not hide ready", StateUnknown, StateReady, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := healthWithSnapshot(selectionSnapshot(tt.a, tt.b))
			if got := h.selectModel(context.Background(), selectionKeys, []bool{false, false}); got != tt.want {
				t.Fatalf("selected %d, want %d", got, tt.want)
			}
		})
	}
	for _, s := range []Snapshot{{}, {Routes: map[ModelKey][]Route{selectionKeys[0]: {{Backend: "gpu", Served: "actual-1"}}}, Backends: selectionSnapshot(StateUnloaded, StateUnloaded).Backends}} {
		if got := healthWithSnapshot(s).selectModel(context.Background(), selectionKeys, []bool{false, false}); got != -1 {
			t.Fatal("missing catalogue/route authorized cold load")
		}
	}
	s := selectionSnapshot(StateUnloaded, StateUnloaded)
	b := s.Backends["gpu"]
	b.Idle = false
	b.Models["unrelated"] = StateLoading
	s.Backends["gpu"] = b
	if healthWithSnapshot(s).selectModel(context.Background(), selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("loading shared backend authorized load")
	}
	// A ready model behind an open breaker still vetoes loading a different one.
	h := healthWithSnapshot(selectionSnapshot(StateUnloaded, StateReady))
	for range DefaultBreakerFailures {
		h.done("b", "m2", class5xx)
	}
	if h.selectModel(context.Background(), selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("open breaker authorized replacement of acceptable ready model")
	}
}
func TestAllPossibleRoutesAndSharedBackendPolicies(t *testing.T) {
	s := selectionSnapshot(StateReady, StateUnloaded)
	s.Routes[selectionKeys[0]] = append(s.Routes[selectionKeys[0]], Route{"elsewhere", "actual-1"})
	s.Backends["elsewhere"] = BackendState{Models: map[string]ModelState{"actual-1": StateUnloaded}}
	if healthWithSnapshot(s).selectModel(context.Background(), selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("one ready route authorized ambiguous inference")
	}
	h := healthWithSnapshot(selectionSnapshot(StateUnloaded, StateUnloaded))
	h.providers["b"] = config.Provider{ID: "b", Load: config.LoadNever}
	if h.selectModel(context.Background(), selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("provider alias bypassed shared no-load policy")
	}
	h = healthWithSnapshot(selectionSnapshot(StateUnloaded, StateUnloaded))
	delete(h.backendLoads, "gpu")
	if h.selectModel(context.Background(), selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("unconfigured backend authorized load")
	}
	// A no-load provider outside the role still protects its native endpoint.
	ls := newLlamaSwap(t)
	h = NewHealth(HealthOptions{Providers: []config.Provider{ls.provider("chat", config.LoadAlways), ls.provider("tagger", config.LoadNever)}})
	if h.selectModel(context.Background(), []ModelKey{{"chat", "m"}}, []bool{false}) != -1 {
		t.Fatal("protected native backend loaded through another provider")
	}
}
func TestDiscoveryFreshGuardDeadlineAndErrors(t *testing.T) {
	reads := 0
	h := NewHealth(HealthOptions{Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
		reads++
		if reads == 1 {
			return selectionSnapshot(StateReady, StateUnloaded), nil
		}
		return selectionSnapshot(StateUnloaded, StateReady), nil
	})})
	i := h.selectModel(context.Background(), selectionKeys, []bool{false, false})
	if i != 0 || !errors.Is(h.dispatch(context.Background(), selectionKeys, []bool{false, false}, i), errSelectionChanged) {
		t.Fatal("stale ready authorized send")
	}
	reads = 0
	h = NewHealth(HealthOptions{DiscoveryTimeout: 10 * time.Millisecond, Discovery: discoveryFunc(func(ctx context.Context, _ []ModelKey) (Snapshot, error) {
		reads++
		<-ctx.Done()
		return selectionSnapshot(StateReady, StateReady), ctx.Err()
	}), BackendLoads: map[string]string{"gpu": config.LoadAlways}})
	if h.selectModel(context.Background(), selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("deadline authorized send")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.selectModel(ctx, selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("cancellation authorized send")
	}
	h = NewHealth(HealthOptions{Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
		return selectionSnapshot(StateUnloaded, StateUnloaded), errors.New("secret transport response")
	}), BackendLoads: map[string]string{"gpu": config.LoadAlways}})
	if h.selectModel(context.Background(), selectionKeys, []bool{false, false}) != -1 {
		t.Fatal("failed discovery authorized cold load")
	}
}
func TestNativeStatesAndMalformedInventories(t *testing.T) {
	for _, tt := range []struct {
		body string
		want int
	}{
		{`{"running":[{"model":"m1","state":"starting"}]}`, -1},
		{`{"running":[{"model":"m1","state":"stopping"}]}`, -1},
		{`{"running":[{"model":"m1"}]}`, -1},
		{`{"running":[{"model":"m1","state":"ready"}]}`, 0},
		{`{"running":[{"model":"m2","state":"ready"}]}`, 1},
		{`{"running":[]}`, 0},
		{`{"running":[],"RUNNING":[{"model":"other","state":"ready"}]}`, -1},
		{`{"running":[{"model":"m1","state":"ready"},{"model":"m1","state":"starting"}]}`, -1},
		{`{"running":[]} {}`, -1},
		{`{"running":[]}` + strings.Repeat(" ", 1<<20), -1},
	} {
		t.Run(tt.body[:min(50, len(tt.body))], func(t *testing.T) {
			ls := newLlamaSwap(t)
			ls.body = tt.body
			h := NewHealth(HealthOptions{Providers: []config.Provider{ls.provider("a", config.LoadAlways), ls.provider("b", config.LoadAlways)}})
			if got := h.selectModel(context.Background(), selectionKeys, []bool{false, false}); got != tt.want {
				t.Fatalf("got %d want %d", got, tt.want)
			}
		})
	}
}
func TestAgentSwitchAfterToolsPreservesResultsAndMetadata(t *testing.T) {
	e := newChainEnv(t, nil, airut.Turn{})
	state := selectionSnapshot(StateReady, StateUnloaded)
	e.agent.health = NewHealth(HealthOptions{Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) { return state, nil })})
	rolled := 0
	e.bot.Command("roll", "Roll", func(c *airut.Ctx) error {
		rolled++
		state = selectionSnapshot(StateUnloaded, StateReady)
		_, err := c.Reply("rolled once")
		return err
	}).AsTool()
	e.bot.Command("switch", "Switch", func(c *airut.Ctx) error {
		return c.Agent(airut.Turn{Prompt: "do it", Tools: e.bot.Tools("roll"), Budget: airut.Budget{Turns: 3}}).StreamTo(c)
	})
	e.a.play(call("c1", "roll", `{}`))
	e.b.play(say("from second"))
	if got := final(airuttest.Start(t, e.bot).Text("!switch")); got != "rolled once\nfrom second" && !strings.Contains(got, "from second") {
		t.Fatalf("answer %q", got)
	}
	if rolled != 1 || len(e.a.requests()) != 1 || len(e.b.requests()) != 1 {
		t.Fatalf("tool replay or wrong model: %d, %v, %v", rolled, e.a.requests(), e.b.requests())
	}
	req := e.b.fake.requests()[0]
	if req.Model != "m2" || req.MaxTokens != 512 {
		t.Fatalf("second metadata: model=%s cap=%d", req.Model, req.MaxTokens)
	}
	found := false
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if result, ok := c.(provider.ToolResultBlock); ok && result.CallID == "c1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("completed tool result missing after switch")
	}
}
func TestAgentFreshSelectionAfterFailureAndAtDispatch(t *testing.T) {
	for _, atDispatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed attempt", true: "dispatch race"}[atDispatch], func(t *testing.T) {
			e := newChainEnv(t, nil, airut.Turn{Prompt: "hello"})
			state := selectionSnapshot(StateReady, StateUnloaded)
			reads := 0
			e.agent.health = NewHealth(HealthOptions{Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
				reads++
				if atDispatch && reads >= 2 {
					state = selectionSnapshot(StateUnloaded, StateReady)
				}
				return state, nil
			})})
			e.a.fail(func(provider.Request) (<-chan provider.Event, error) {
				state = selectionSnapshot(StateUnloaded, StateReady)
				return nil, provider.NewHTTPError("a", 503, "", "")
			})
			e.b.play(say("second"))
			if got := final(airuttest.Start(t, e.bot).Text("!ask")); got != "second" {
				t.Fatalf("answer %q", got)
			}
			want := 1
			if atDispatch {
				want = 0
			}
			if len(e.a.requests()) != want || len(e.b.requests()) != 1 {
				t.Fatalf("physical sends: a=%v b=%v", e.a.requests(), e.b.requests())
			}
		})
	}
}
func TestJudgeReadySecondAndPerCandidateGuard(t *testing.T) {
	jc := newJudgeChain(t, nil, nil)
	// Judge identities differ from the agent fixture; translate only routing.
	jc.g.health = NewHealth(HealthOptions{Discovery: discoveryFunc(func(_ context.Context, keys []ModelKey) (Snapshot, error) {
		s := selectionSnapshot(StateUnloaded, StateReady)
		s.Routes = map[ModelKey][]Route{keys[0]: {{"gpu", "actual-1"}}, keys[1]: {{"gpu", "actual-2"}}}
		return s, nil
	})})
	var guarded []string
	jc.g.o.Guard = func(_ *airut.Ctx, m JudgeModel, j airut.Judgement) bool {
		guarded = append(guarded, m.Model)
		return true
	}
	jb := newJudgeBot(t, jc.g, chainJudgement)
	jb.a.Text("!judge")
	if len(jc.s1.requests()) != 0 || len(jc.s2.requests()) != 1 || len(guarded) != 1 || guarded[0] != jc.g.models[1].Model {
		t.Fatalf("wrong actual candidate guard: %v", guarded)
	}
}
func TestNativeDiscoveryAndInferenceShareConnections(t *testing.T) {
	var mu sync.Mutex
	peers := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		peers[r.RemoteAddr] = true
		mu.Unlock()
		if r.URL.Path == "/running" {
			w.Write([]byte(`{"running":[{"model":"m","state":"ready"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	pool := providerHTTP()
	defer pool.CloseIdleConnections()
	cfg := &config.Config{Providers: []config.Provider{{ID: "p", Kind: config.KindLlamaSwap, Load: config.LoadNever, BaseURL: srv.URL + "/v1", Models: []config.Model{{ID: "m"}}}}}
	clients, _ := ProvidersWithOptions(cfg, ProviderOptions{HTTPClients: map[string]*http.Client{"p": pool}})
	h := NewHealth(HealthOptions{Providers: cfg.Providers, HTTPClients: map[string]*http.Client{"p": pool}})
	for range 3 {
		if !h.available(context.Background(), "p", "m") {
			t.Fatal("not ready")
		}
		events, err := clients["p"].Stream(context.Background(), provider.Request{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		for range events {
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peers) != 1 {
		t.Fatalf("discovery/inference used %d connections", len(peers))
	}
}

// An actual OpenAI-compatible client has a silent retry layer. A 503 must
// leave that layer before a second physical POST, so selection and engine
// metadata both change rather than resending the captured first payload.
func TestPhysicalHTTPRetryReselectsModel(t *testing.T) {
	for _, construction := range []string{"providers", "open", "decorated"} {
		t.Run(construction, func(t *testing.T) { testPhysicalHTTPRetryReselectsModel(t, construction) })
	}
}

// beforeStream represents an application decorator that preserves Unwrap but
// does not itself expose the internal HTTP-boundary marker.
type beforeStream struct {
	provider.Client
	before func()
}

func (c beforeStream) Unwrap() provider.Client { return c.Client }
func (c beforeStream) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	if c.before != nil {
		c.before()
	}
	return c.Client.Stream(ctx, req)
}

func testPhysicalHTTPRetryReselectsModel(t *testing.T, construction string) {
	var mu sync.Mutex
	state := selectionSnapshot(StateReady, StateUnloaded)
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		models = append(models, req.Model)
		if req.Model == "m1" {
			state = selectionSnapshot(StateUnloaded, StateReady)
		}
		mu.Unlock()
		if req.Model == "m1" {
			w.WriteHeader(503)
			w.Write([]byte("secret prompt echoed by fake upstream"))
			return
		}
		if req.MaxTokens != 77 {
			t.Errorf("wrong second output cap %d", req.MaxTokens)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"second\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	pool := providerHTTP()
	defer pool.CloseIdleConnections()
	h := NewHealth(HealthOptions{Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) { mu.Lock(); defer mu.Unlock(); return state, nil })})
	cfg := &config.Config{Store: config.Store{Path: filepath.Join(t.TempDir(), "sessions.db")}, Agent: &config.Agent{Models: config.Chain{
		{Provider: "a", Model: "m1", Served: "m1", Capabilities: config.Capabilities{ContextWindow: 32000, MaxOutput: 1024}},
		{Provider: "b", Model: "m2", Served: "m2", Capabilities: config.Capabilities{ContextWindow: 32000, MaxOutput: 77}},
	}}}
	for _, entry := range cfg.Agent.Models {
		cfg.Providers = append(cfg.Providers, config.Provider{ID: entry.Provider, BaseURL: srv.URL,
			Models: []config.Model{{ID: entry.Model, Capabilities: entry.Capabilities}}})
	}
	pools := map[string]*http.Client{"a": pool, "b": pool}
	var logs logBuffer
	var ag *Agent
	var err error
	if construction == "open" {
		ag, err = Open(context.Background(), cfg, nil, logs.logger(), WithHealth(h), WithHTTPClients(pools))
		if err == nil {
			t.Cleanup(func() { ag.Close() })
		}
	} else {
		clients, cat := ProvidersWithOptions(cfg, ProviderOptions{HTTPClients: pools})
		if construction == "decorated" {
			for p, client := range clients {
				clients[p] = beforeStream{Client: client}
			}
		}
		ag, err = New(Options{Clients: clients, Catalog: cat, Models: cfg.Agent.Models, Health: h,
			Sessions: openDB(t, cfg.Store.Path), Logger: logs.logger()})
	}
	if err != nil {
		t.Fatal(err)
	}
	bot := airut.New(airut.Config{Agent: ag, Gate: allow, EditInterval: time.Nanosecond})
	bot.Command("ask", "Ask", func(c *airut.Ctx) error { return c.Agent(airut.Turn{Prompt: "hello"}).StreamTo(c) })
	if got := final(airuttest.Start(t, bot).Text("!ask")); got != "second" {
		t.Fatalf("answer %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(models, ",") != "m1,m2" {
		t.Fatalf("physical requests %v", models)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatal("raw upstream body logged")
	}
}

func TestJudgeStreamingHTTPReselects(t *testing.T) {
	for _, atDispatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP retry", true: "dispatch race"}[atDispatch], func(t *testing.T) {
			var mu sync.Mutex
			state := selectionSnapshot(StateReady, StateUnloaded)
			var models []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model     string `json:"model"`
					MaxTokens int    `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				models = append(models, req.Model)
				state = selectionSnapshot(StateUnloaded, StateReady)
				mu.Unlock()
				if req.Model == "m1" {
					w.WriteHeader(503)
					w.Write([]byte("secret prompt echoed by fake upstream"))
					return
				}
				if req.MaxTokens != 77 {
					t.Errorf("wrong judge output cap %d", req.MaxTokens)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": `{"decision":"no","target":0,"reason":"quiet"}`}}}})
				w.Write(append(append([]byte("data: "), chunk...), []byte("\n\ndata: [DONE]\n\n")...))
			}))
			defer srv.Close()
			pool := providerHTTP()
			defer pool.CloseIdleConnections()
			cfg := &config.Config{Providers: []config.Provider{{ID: "a", BaseURL: srv.URL}, {ID: "b", BaseURL: srv.URL}}}
			clients, _ := ProvidersWithOptions(cfg, ProviderOptions{HTTPClients: map[string]*http.Client{"a": pool, "b": pool}})
			clients["a"] = beforeStream{Client: clients["a"], before: func() {
				if atDispatch {
					mu.Lock()
					state = selectionSnapshot(StateUnloaded, StateReady)
					mu.Unlock()
				}
			}}
			var guarded []string
			var logs logBuffer
			g, err := NewJudge(JudgeOptions{
				Models:   []JudgeModel{{Provider: "a", Model: "m1", Client: clients["a"]}, {Provider: "b", Model: "m2", Client: clients["b"], MaxTokens: 77}},
				Personas: map[string]string{"p": "Decide."}, Logger: logs.logger(),
				Guard: func(_ *airut.Ctx, m JudgeModel, _ airut.Judgement) bool {
					guarded = append(guarded, m.Model)
					return true
				},
				Health: NewHealth(HealthOptions{Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
					mu.Lock()
					defer mu.Unlock()
					return state, nil
				})}),
			})
			if err != nil {
				t.Fatal(err)
			}
			jb := newJudgeBot(t, g, chainJudgement)
			jb.a.Text("!judge")
			if jb.errs[0] != nil || jb.got[0].Decision != airut.No {
				t.Fatalf("judge result %+v, %v", jb.got[0], jb.errs[0])
			}
			want := "m1,m2"
			if atDispatch {
				want = "m2"
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(models, ",") != want || strings.Join(guarded, ",") != "m1,m2" {
				t.Fatalf("physical requests %v, candidate guards %v", models, guarded)
			}
			if strings.Contains(logs.String()+jb.logs.String(), "secret") {
				t.Fatal("raw upstream body logged")
			}
		})
	}
}
func TestCandidateContextOutputAndImageMetadata(t *testing.T) {
	e := newChainEnv(t, nil, airut.Turn{Prompt: strings.Repeat("x", 1200)})
	e.agent.entryModels[0].ContextWindow = 256
	e.agent.entryModels[0].MaxOutput = 32
	e.agent.chain[0].Capabilities.ContextWindow = 256
	e.agent.chain[0].MaxOutput = 32
	e.agent.entryModels[1].MaxOutput = 0
	e.agent.chain[1].MaxOutput = 0
	e.b.play(say("fits"))
	if got := final(airuttest.Start(t, e.bot).Text("!ask")); got != "fits" {
		t.Fatalf("answer %q", got)
	}
	if len(e.a.requests()) != 0 {
		t.Fatal("oversize candidate received inference")
	}
	if req := e.b.fake.requests()[0]; req.MaxTokens != 0 {
		t.Fatalf("retained another candidate's output cap %d", req.MaxTokens)
	}
	// Image handling uses the newly selected model without changing the live
	// transcript, so a later vision candidate can still consume the image.
	input := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.ImageBlock{Data: []byte("bitmap")}}}}
	stripped := requestWithoutImages(input)
	if _, ok := stripped[0].Content[0].(provider.TextBlock); !ok {
		t.Fatal("image not guarded")
	}
	if _, ok := input[0].Content[0].(provider.ImageBlock); !ok {
		t.Fatal("live transcript image was overwritten")
	}
}
func TestApplicationAndNativeDiscoveryCoexist(t *testing.T) {
	ls := newLlamaSwap(t, "m2")
	h := NewHealth(HealthOptions{Providers: []config.Provider{ls.provider("b", config.LoadNever)}, DiscoveryProviders: []string{"a"}, Discovery: discoveryFunc(func(_ context.Context, keys []ModelKey) (Snapshot, error) {
		if len(keys) != 1 || keys[0].Provider != "a" {
			t.Error("adapter received native provider")
		}
		s := selectionSnapshot(StateUnknown, StateUnknown)
		delete(s.Routes, selectionKeys[1])
		return s, nil
	})})
	if got := h.selectModel(context.Background(), selectionKeys, []bool{false, false}); got != 1 {
		t.Fatalf("native second selected %d", got)
	}
}

func TestApplicationPoliciesApplyToLlamaSwap(t *testing.T) {
	for _, tt := range []struct {
		name, backend, provider string
		idle                    bool
		want                    int
	}{
		{"backend never", config.LoadNever, config.LoadAlways, true, -1},
		{"backend unlisted", "", config.LoadAlways, true, -1},
		{"backend requires idle", config.LoadWhenIdle, config.LoadAlways, false, -1},
		{"provider never", config.LoadAlways, config.LoadNever, true, -1},
		{"provider requires idle", config.LoadAlways, config.LoadWhenIdle, false, -1},
		{"both permit load", config.LoadAlways, config.LoadAlways, false, 0},
		{"both require idle", config.LoadWhenIdle, config.LoadWhenIdle, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := selectionSnapshot(StateUnloaded, StateUnloaded)
			b := s.Backends["gpu"]
			b.Idle = tt.idle
			s.Backends["gpu"] = b
			policies := map[string]string{}
			if tt.backend != "" {
				policies["gpu"] = tt.backend
			}
			h := NewHealth(HealthOptions{
				Providers: []config.Provider{{ID: "a", Kind: config.KindLlamaSwap, Load: tt.provider}},
				Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) { return s, nil }), BackendLoads: policies,
			})
			if got := h.selectModel(context.Background(), selectionKeys[:1], []bool{false}); got != tt.want {
				t.Fatalf("selected %d, want %d", got, tt.want)
			}
		})
	}
}

func TestOverlappingApplicationNativeObservations(t *testing.T) {
	for _, tt := range []struct {
		name        string
		application *BackendState
		running     []string
		want        int
	}{
		{"missing backend with native ready", nil, []string{"m"}, -1},
		{"missing models with native ready", &BackendState{}, []string{"m"}, -1},
		{"empty models with native ready", &BackendState{Models: map[string]ModelState{}}, []string{"m"}, -1},
		{"missing backend with native unloaded", nil, nil, -1},
		{"missing models with native unloaded", &BackendState{}, nil, -1},
		{"empty models with native unloaded", &BackendState{Models: map[string]ModelState{}}, nil, -1},
		{"missing served entry", &BackendState{Models: map[string]ModelState{"other": StateUnloaded}}, []string{"m"}, -1},
		{"agree ready", &BackendState{Models: map[string]ModelState{"m": StateReady}}, []string{"m"}, 0},
		{"agree unloaded", &BackendState{Models: map[string]ModelState{"m": StateUnloaded}, Idle: true}, nil, 0},
		{"conflict ready unloaded", &BackendState{Models: map[string]ModelState{"m": StateReady}}, nil, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ls := newLlamaSwap(t, tt.running...)
			id := ls.provider("native", config.LoadAlways).RunningURL()
			keys := []ModelKey{{"app", "m"}, {"native", "m"}}
			h := NewHealth(HealthOptions{
				Providers: []config.Provider{ls.provider("native", config.LoadAlways)}, DiscoveryProviders: []string{"app"},
				BackendLoads: map[string]string{id: config.LoadAlways},
				Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
					s := Snapshot{Routes: map[ModelKey][]Route{keys[0]: {{id, "m"}}}, Backends: map[string]BackendState{}}
					if tt.application != nil {
						s.Backends[id] = *tt.application
					}
					return s, nil
				}),
			})
			if got := h.selectModel(context.Background(), keys, []bool{false, false}); got != tt.want {
				t.Fatalf("selected %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSharedBackendPoliciesAlsoRestrictNativeAliases(t *testing.T) {
	for _, policy := range []string{"", config.LoadNever, config.LoadWhenIdle, config.LoadAlways} {
		t.Run("backend="+policy, func(t *testing.T) {
			ls := newLlamaSwap(t)
			id := ls.provider("native", config.LoadAlways).RunningURL()
			keys := []ModelKey{{"native", "m"}, {"app", "m"}}
			loads := map[string]string{}
			if policy != "" {
				loads[id] = policy
			}
			h := NewHealth(HealthOptions{
				Providers: []config.Provider{ls.provider("native", config.LoadAlways)}, DiscoveryProviders: []string{"app"}, BackendLoads: loads,
				Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
					return Snapshot{Routes: map[ModelKey][]Route{keys[1]: {{id, "m"}}}, Backends: map[string]BackendState{
						id: {Models: map[string]ModelState{"m": StateUnloaded}, Idle: false},
					}}, nil
				}),
			})
			want := -1
			if policy == config.LoadAlways {
				want = 0
			}
			if got := h.selectModel(context.Background(), keys, []bool{false, false}); got != want {
				t.Fatalf("selected %d, want %d", got, want)
			}
		})
	}
}

func TestOverlapReconcilesAllServedIdentities(t *testing.T) {
	ls := newLlamaSwap(t, "m2")
	id := ls.provider("native", config.LoadAlways).RunningURL()
	keys := []ModelKey{{"app", "m1"}, {"native", "m2"}}
	h := NewHealth(HealthOptions{
		Providers: []config.Provider{ls.provider("native", config.LoadAlways)}, DiscoveryProviders: []string{"app"},
		BackendLoads: map[string]string{id: config.LoadAlways},
		Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
			return Snapshot{Routes: map[ModelKey][]Route{keys[0]: {{id, "m1"}}}, Backends: map[string]BackendState{
				id: {Models: map[string]ModelState{"m1": StateUnloaded}},
			}}, nil
		}),
	})
	if h.selectModel(context.Background(), keys, []bool{false, false}) != -1 {
		t.Fatal("missing application observation for native served identity authorized inference")
	}
}

func TestApplicationUnknownCannotInheritUnmanagedAvailability(t *testing.T) {
	keys := []ModelKey{{"app", "m"}, {"unmanaged", "m"}}
	h := NewHealth(HealthOptions{DiscoveryProviders: []string{"app"},
		Discovery: discoveryFunc(func(context.Context, []ModelKey) (Snapshot, error) {
			return Snapshot{Routes: map[ModelKey][]Route{keys[0]: {{"provider:unmanaged", "m"}}}}, nil
		}),
	})
	if h.selectModel(context.Background(), keys, []bool{false, false}) != -1 {
		t.Fatal("unmanaged fallback replaced missing application observation")
	}
}
