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
