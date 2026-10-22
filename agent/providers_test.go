package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// endpoint is a fake OpenAI-compatible server that records the
// Authorization header of each request and fails it.
func endpoint(t *testing.T) (url string, auth chan string) {
	t.Helper()
	auth = make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth <- r.Header.Get("Authorization")
		http.Error(w, "fake", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", auth
}

func TestProviders(t *testing.T) {
	keyed, keyedAuth := endpoint(t)
	keyless, keylessAuth := endpoint(t)
	t.Setenv("AIRUT_TEST_KEY", "sk-test")
	path := filepath.Join(t.TempDir(), "airut.toml")
	src := `
[providers.proxy]
base_url = "` + keyed + `"
api_key = { env = "AIRUT_TEST_KEY" }
models = [{ id = "big", context_window = 200000, max_output = 32000, reasoning = true }]

[providers.local]
base_url = "` + keyless + `"
models = [{ id = "Qwen/Qwen3-32B" }]

[discord]
token = { env = "AIRUT_TEST_KEY" }
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	clients, catalog := Providers(cfg)

	if len(clients) != 2 || clients["proxy"].Name() != "proxy" || clients["local"].Name() != "local" {
		t.Fatalf("clients = %v", clients)
	}
	m, err := catalog.FindModel("proxy", "big")
	if err != nil || m.ContextWindow != 200000 || m.MaxOutput != 32000 || !m.Reasoning {
		t.Fatalf("FindModel(proxy, big) = %+v, %v", m, err)
	}
	if _, err := catalog.FindModel("local", "Qwen/Qwen3-32B"); err != nil {
		t.Fatalf("a model id with a slash: %v", err)
	}
	if _, err := catalog.FindModel("local", "big"); err == nil {
		t.Fatal("a model was found under the wrong provider")
	}

	// Each client talks to its own endpoint, with its own key or none.
	for name, c := range map[string]struct {
		model string
		auth  chan string
		want  string
	}{
		"proxy": {"big", keyedAuth, "sk-test"},
		"local": {"Qwen/Qwen3-32B", keylessAuth, ""},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		events, err := clients[name].Stream(ctx, providerRequest(c.model))
		if err == nil {
			for range events {
			}
		}
		cancel()
		select {
		case got := <-c.auth:
			// A keyless client sends an empty bearer token.
			if token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer")); token != c.want {
				t.Errorf("%s sent Authorization %q, want token %q", name, got, c.want)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s never reached its endpoint (Stream: %v)", name, err)
		}
	}
}

func providerRequest(model string) provider.Request {
	return provider.Request{Model: model, MaxTokens: 16, Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hi"}}}}}
}

// TestOpenTakesTheHistoryDefaults checks that agent.Open hands every
// [agent.history] key to the agent.
func TestOpenTakesTheHistoryDefaults(t *testing.T) {
	t.Setenv("AIRUT_TEST_KEY", "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "airut.toml")
	src := `
[providers.local]
base_url = "http://127.0.0.1:1/v1"
models = [{ id = "m" }]

[agent]
model = "local/m"
budget = { max_turns = 4, max_tokens = 1000 }
history = { max_messages = 5, max_tokens = 600, max_age = "2h", compaction = "drop", room_messages = 7, follow_within = "3m" }

[store]
path = "sessions.db"

[discord]
token = { env = "AIRUT_TEST_KEY" }
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	want := airut.History{MaxMessages: 5, MaxTokens: 600, MaxAge: 2 * time.Hour, Compaction: airut.CompactDrop, RoomMessages: 7, FollowWithin: 3 * time.Minute}
	if a.history != want {
		t.Errorf("the agent's history = %+v, want %+v", a.history, want)
	}
}

// TestServedModels checks that a model's served id, not the alias the
// file names it by, is what the catalog lists and what a request
// carries, and that the agent's catalog describes the model as the
// agent's first entry does.
func TestServedModels(t *testing.T) {
	t.Setenv("AIRUT_TEST_KEY", "token")
	dir := t.TempDir()
	path := filepath.Join(dir, "airut.toml")
	src := `
[providers.local]
base_url = "http://127.0.0.1:1/v1"
models = [
  { id = "fast", model = "Real-Model-Q4", context_window = 32768, max_output = 4096 },
  { id = "small", base = "fast", context_window = 8192 },
]

[agent]
model = "local/small"
budget = { max_turns = 4, max_tokens = 1000 }

[agent.judge]
models = ["local/small", "local/fast"]

[store]
path = "sessions.db"

[discord]
token = { env = "AIRUT_TEST_KEY" }
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, catalog := Providers(cfg)
	if m, err := catalog.FindModel("local", "Real-Model-Q4"); err != nil || m.ContextWindow != 32768 {
		t.Errorf("FindModel(served) = %+v, %v; want the first entry that serves it", m, err)
	}
	if _, err := catalog.FindModel("local", "fast"); err == nil {
		t.Error("the catalog lists an alias")
	}

	a, err := Open(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(a.chain) != 1 || a.chain[0].Served != "Real-Model-Q4" || a.chain[0].Model != "small" {
		t.Errorf("the agent's chain = %+v, want the alias serving its root's id", a.chain)
	}
	if m, err := a.catalog.FindModel("local", "Real-Model-Q4"); err != nil || m.ContextWindow != 8192 || m.MaxOutput != 4096 {
		t.Errorf("the agent's catalog = %+v, %v; want the variant's window", m, err)
	}

	j, err := OpenJudge(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if j.o.Model != "Real-Model-Q4" || j.o.MaxInputTokens != 8192 {
		t.Errorf("the judge runs %q capped at %d, want the served id capped at the variant's window", j.o.Model, j.o.MaxInputTokens)
	}
	if len(j.models) != 2 || j.models[0].Alias != "small" || j.models[1].Alias != "fast" || j.models[1].MaxInputTokens != 32768 {
		t.Errorf("the judge's chain = %+v", j.models)
	}
}
