package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEarlierExampleLoadsUnchanged loads the example configuration as it
// stood before providers described their models' kinds and chains. A
// file written to it loads to the same values, and the judge's
// table-level output, slots, cache_prompt, and timeout still set them.
func TestEarlierExampleLoadsUnchanged(t *testing.T) {
	credentials(t, "proxy-api-key", "discord-token", "matrix-password", "matrix-recovery-key")
	c, err := Load("testdata/v0.5-example.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range c.Providers {
		if p.Kind != KindOpenAI || p.Load != "" || p.RunningURL() != "" {
			t.Errorf("provider %s: kind %q, load %q, running %q", p.ID, p.Kind, p.Load, p.RunningURL())
		}
		for _, m := range p.Models {
			if m.Served != m.ID || m.Base != "" {
				t.Errorf("model %s: served %q, base %q", m.ID, m.Served, m.Base)
			}
		}
	}
	a := c.Agent
	want := ChainEntry{Provider: "local", Model: "qwen3-32b", Served: "qwen3-32b",
		Capabilities: Capabilities{ContextWindow: 32768, MaxOutput: 8192, Reasoning: true}}
	if a.Model != (ModelRef{Provider: "local", Model: "qwen3-32b"}) || len(a.Models) != 1 || !chainEntryEqual(a.Models[0], want) {
		t.Errorf("agent: %+v %+v", a.Model, a.Models)
	}
	j := a.Judge
	if j.Model != a.Model || j.Output != "json_schema" || j.Slots != 2 || !j.CachePrompt || j.Timeout != 20*time.Second ||
		j.MaxInputTokens != 8000 || j.Concurrency != 2 || len(j.Models) != 1 {
		t.Errorf("judge: %+v", j)
	}
	want.Output, want.Slots, want.CachePrompt, want.Timeout, want.MaxInputTokens = "json_schema", 2, true, 20*time.Second, 8000
	if len(j.Models) == 1 && !chainEntryEqual(j.Models[0], want) {
		t.Errorf("judge entry = %+v, want %+v", j.Models[0], want)
	}
}

func chainEntryEqual(a, b ChainEntry) bool {
	ta, tb := a.Temperature, b.Temperature
	a.Temperature, b.Temperature = nil, nil
	return a == b && (ta == nil) == (tb == nil) && (ta == nil || *ta == *tb)
}

// withAgent is a file with a platform, the providers in providers, and
// the [agent] table body agent, which the caller completes.
func withAgent(providers, agent string) string {
	return minimal + providers + "\n[store]\npath = \"s.db\"\n[agent]\nbudget = { max_turns = 1, max_tokens = 1 }\n" + agent
}

func TestProviderKinds(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	for _, c := range []struct {
		base, kind, load      string
		wantKind, wantLoad    string
		wantRunning, wantProb string
	}{
		{base: "http://h.example:8080/v1", wantKind: KindOpenAI},
		{base: "http://h.example:8080/v1", kind: "openai", wantKind: KindOpenAI},
		{base: "http://h.example:8080/v1", kind: "llama-swap", wantKind: KindLlamaSwap, wantLoad: LoadNever, wantRunning: "http://h.example:8080/running"},
		{base: "http://h.example:8080/v1/", kind: "llama-swap", load: "when_idle", wantKind: KindLlamaSwap, wantLoad: LoadWhenIdle, wantRunning: "http://h.example:8080/running"},
		{base: "http://h.example:8080", kind: "llama-swap", load: "always", wantKind: KindLlamaSwap, wantLoad: LoadAlways, wantRunning: "http://h.example:8080/running"},
		{base: "https://h.example/swap/v1", kind: "llama-swap", load: "never", wantKind: KindLlamaSwap, wantLoad: LoadNever, wantRunning: "https://h.example/swap/running"},
		{base: "https://h.example/swap/", kind: "llama-swap", wantKind: KindLlamaSwap, wantLoad: LoadNever, wantRunning: "https://h.example/swap/running"},
		{base: "http://h.example/v1", kind: "ollama", wantProb: "providers.p.kind"},
		{base: "http://h.example/v1", load: "when_idle", wantProb: "providers.p.load"},
		{base: "http://h.example/v1", kind: "openai", load: "never", wantProb: "providers.p.load"},
		{base: "http://h.example/v1", kind: "llama-swap", load: "sometimes", wantProb: "providers.p.load"},
	} {
		src := minimal + "[providers.p]\nbase_url = \"" + c.base + "\"\nmodels = [{ id = \"m\" }]\n"
		if c.kind != "" {
			src += "kind = \"" + c.kind + "\"\n"
		}
		if c.load != "" {
			src += "load = \"" + c.load + "\"\n"
		}
		cfg, err := load(t, src)
		if c.wantProb != "" {
			if got := problemKeys(t, err); !slices.Equal(got, []string{c.wantProb}) {
				t.Errorf("%+v: keys = %q", c, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: %v", c, err)
			continue
		}
		p, ok := cfg.Provider("p")
		if !ok || p.Kind != c.wantKind || p.Load != c.wantLoad || p.RunningURL() != c.wantRunning {
			t.Errorf("%+v: got kind %q, load %q, running %q", c, p.Kind, p.Load, p.RunningURL())
		}
	}
	if _, err := load(t, minimal+"[providers.p]\nbase_url = \"http://h.example\"\nmodels = [{ id = \"m\" }]\n"); err != nil {
		t.Fatal(err)
	}
}

func TestModelBases(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	c, err := load(t, minimal+`
[providers.p]
base_url = "http://h.example/v1"
models = [
  { id = "judge-fast", base = "judge", timeout = "5s", cache_prompt = false },
  { id = "gemma", model = "gemma-4-12B", context_window = 16384, max_output = 4096, reasoning = true, output = "json_schema", slots = 2, cache_prompt = true, temperature = 0.7 },
  { id = "judge", base = "gemma", reasoning = false, temperature = 0, timeout = "20s" },
  { id = "renamed", base = "gemma", model = "gemma-4-12B-q8" },
  { id = "plain" },
]
`)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers[0]
	get := func(id string) Model {
		m, ok := p.Model(id)
		if !ok {
			t.Fatalf("no model %s", id)
		}
		return m
	}
	gemma := Capabilities{ContextWindow: 16384, MaxOutput: 4096, Reasoning: true, Output: "json_schema", Slots: 2, CachePrompt: true}
	for _, w := range []struct {
		id, served, base string
		caps             Capabilities
		temp             float64
	}{
		{"gemma", "gemma-4-12B", "", gemma, 0.7},
		{"judge", "gemma-4-12B", "gemma", func() Capabilities { c := gemma; c.Reasoning = false; c.Timeout = 20 * time.Second; return c }(), 0},
		{"judge-fast", "gemma-4-12B", "judge", func() Capabilities {
			c := gemma
			c.Reasoning, c.Timeout, c.CachePrompt = false, 5*time.Second, false
			return c
		}(), 0},
		{"renamed", "gemma-4-12B-q8", "gemma", gemma, 0.7},
	} {
		m := get(w.id)
		got := m.Capabilities
		if m.Served != w.served || m.Base != w.base || got.Temperature == nil || *got.Temperature != w.temp {
			t.Errorf("%s: served %q, base %q, temperature %v", w.id, m.Served, m.Base, got.Temperature)
			continue
		}
		got.Temperature = nil
		if got != w.caps {
			t.Errorf("%s: caps = %+v, want %+v", w.id, got, w.caps)
		}
	}
	if m := get("plain"); m.Served != "plain" || m.Capabilities != (Capabilities{}) {
		t.Errorf("plain = %+v", m)
	}
	// A variant's temperature is its own copy.
	if get("judge").Temperature == get("gemma").Temperature {
		t.Error("variants share a temperature")
	}
}

func TestModelProblems(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	for _, c := range []struct {
		name, models string
		keys         []string
	}{
		{"a missing base", `[{ id = "a", base = "nope" }]`, []string{"providers.p.models.0.base"}},
		{"a base on itself", `[{ id = "a", base = "a" }]`, []string{"providers.p.models.0.base"}},
		{"a cycle", `[{ id = "a", base = "b" }, { id = "b", base = "c" }, { id = "c", base = "a" }, { id = "d", base = "a" }, { id = "e" }]`,
			[]string{"providers.p.models.0.base", "providers.p.models.1.base", "providers.p.models.2.base"}},
		{"a base in another provider", `[{ id = "a", base = "q/m" }]`, []string{"providers.p.models.0.base"}},
		{"bad capabilities", `[{ id = "a", context_window = -1, max_output = -1, slots = -1, output = "xml", timeout = "soon", temperature = 2.5 }, { id = "b", temperature = -0.1 }]`,
			[]string{"providers.p.models.0.context_window", "providers.p.models.0.max_output", "providers.p.models.0.output",
				"providers.p.models.0.slots", "providers.p.models.0.temperature", "providers.p.models.0.timeout", "providers.p.models.1.temperature"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, minimal+"[providers.p]\nbase_url = \"http://h.example/v1\"\nmodels = "+c.models+"\n")
			if got := problemKeys(t, err); !slices.Equal(got, c.keys) {
				t.Errorf("keys = %q, want %q\n%v", got, c.keys, err)
			}
		})
	}
	// A wrong type is a decode error, reported at its key, which go-toml
	// gives without the index, through the embedded capabilities.
	_, err := load(t, minimal+"[providers.p]\nbase_url = \"http://h.example/v1\"\n[[providers.p.models]]\nid = \"a\"\ntemperature = \"warm\"\n")
	if got := problemKeys(t, err); !slices.Equal(got, []string{"providers.p.models.temperature"}) || !strings.Contains(err.Error(), "the value is not a number") {
		t.Errorf("err = %v", err)
	}
}

const chainProviders = `
[providers.swap]
base_url = "http://swap.example/v1"
kind = "llama-swap"
models = [
  { id = "gemma", model = "gemma-4-12B", context_window = 16384, output = "json_schema", slots = 4, timeout = "10s" },
  { id = "gemma-judge", base = "gemma", context_window = 8192, temperature = 0 },
]
[providers.cpa]
base_url = "https://cpa.example/v1"
models = [{ id = "humanlike-2.0", context_window = 8192, output = "none" }, { id = "unsized" }]
`

func TestChains(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	c, err := load(t, withAgent(chainProviders, `
models = ["swap/gemma", { model = "cpa/humanlike-2.0", max_output = 512, timeout = "1m" }]
[agent.judge]
models = [
  "swap/gemma-judge",
  { model = "cpa/humanlike-2.0", output = "json_schema", slots = 0, max_input_tokens = 6000 },
  "cpa/unsized",
]
slots = 2
cache_prompt = true
`))
	if err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	for _, w := range []struct {
		name string
		got  Chain
		want Chain
	}{
		{"agent", c.Agent.Models, Chain{
			{Provider: "swap", Model: "gemma", Served: "gemma-4-12B",
				Capabilities: Capabilities{ContextWindow: 16384, Output: "json_schema", Slots: 4, Timeout: 10 * time.Second}},
			{Provider: "cpa", Model: "humanlike-2.0", Served: "humanlike-2.0",
				Capabilities: Capabilities{ContextWindow: 8192, MaxOutput: 512, Output: "none", Timeout: time.Minute}},
		}},
		{"judge", c.Agent.Judge.Models, Chain{
			// The table's slots and cache_prompt override the model's.
			{Provider: "swap", Model: "gemma-judge", Served: "gemma-4-12B", MaxInputTokens: 8192,
				Capabilities: Capabilities{ContextWindow: 8192, Output: "json_schema", Slots: 2, CachePrompt: true, Timeout: 10 * time.Second, Temperature: &zero}},
			// The entry's own keys override the table's.
			{Provider: "cpa", Model: "humanlike-2.0", Served: "humanlike-2.0", MaxInputTokens: 6000,
				Capabilities: Capabilities{ContextWindow: 8192, Output: "json_schema", Slots: 0, CachePrompt: true}},
			// No context_window and no max_input_tokens caps nothing.
			{Provider: "cpa", Model: "unsized", Served: "unsized", Capabilities: Capabilities{Slots: 2, CachePrompt: true}},
		}},
	} {
		if len(w.got) != len(w.want) {
			t.Errorf("%s: %d entries, want %d: %+v", w.name, len(w.got), len(w.want), w.got)
			continue
		}
		for i := range w.want {
			if !chainEntryEqual(w.got[i], w.want[i]) {
				t.Errorf("%s.%d = %+v, want %+v", w.name, i, w.got[i], w.want[i])
			}
		}
	}
	a := c.Agent
	if a.Model != (ModelRef{Provider: "swap", Model: "gemma"}) || a.Models[0].Ref() != a.Model {
		t.Errorf("agent model = %v", a.Model)
	}
	j := a.Judge
	if j.Model.String() != "swap/gemma-judge" || j.Output != "json_schema" || j.Slots != 2 || !j.CachePrompt ||
		j.Timeout != 10*time.Second || j.MaxInputTokens != 8192 {
		t.Errorf("judge's first-entry fields = %+v", j)
	}

	// A table-level max_input_tokens overrides every entry's window, and
	// an entry's overrides the table's.
	c, err = load(t, withAgent(chainProviders, `model = "swap/gemma"
[agent.judge]
model = "swap/gemma"
max_input_tokens = 4000
`))
	if err != nil || c.Agent.Judge.MaxInputTokens != 4000 || c.Agent.Judge.Models[0].MaxInputTokens != 4000 {
		t.Errorf("table max_input_tokens: %v %+v", err, c.Agent.Judge)
	}
	c, err = load(t, withAgent(chainProviders, `model = "swap/gemma"
[agent.judge]
model = "swap/gemma"
max_input_tokens = 0
`))
	if err != nil || c.Agent.Judge.MaxInputTokens != 0 {
		t.Errorf("max_input_tokens = 0 caps nothing: %v %+v", err, c.Agent.Judge)
	}
	// Empty output and timeout mean the default, as they always have.
	c, err = load(t, withAgent(chainProviders, `model = "swap/gemma"
[agent.judge]
model = "cpa/humanlike-2.0"
output = ""
timeout = ""
`))
	if err != nil || c.Agent.Judge.Output != "none" || c.Agent.Judge.Timeout != 0 {
		t.Errorf("empty output and timeout: %v %+v", err, c.Agent.Judge)
	}
}

func TestChainProblems(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	for _, c := range []struct {
		name, agent string
		keys        []string
	}{
		{"both", "model = \"swap/gemma\"\nmodels = [\"swap/gemma\"]\n", []string{"agent.models"}},
		{"neither", "", []string{"agent.model"}},
		{"an empty chain", "models = []\n", []string{"agent.models"}},
		{"not an array", "models = \"swap/gemma\"\n", []string{"agent.models"}},
		{"bad entries", `models = ["gemma", 3, "swap/none", "nowhere/gemma", { timeout = "1s" }, { model = "swap/gemma", colour = "blue", timeout = "soon", max_input_tokens = 10 }]` + "\n",
			[]string{"agent.models.0", "agent.models.1", "agent.models.2", "agent.models.3", "agent.models.4.model",
				"agent.models.5.colour"}},
		{"bad overrides", `models = [{ model = "swap/gemma", timeout = "soon", slots = -1, max_input_tokens = 10 }]` + "\n",
			[]string{"agent.models.0.max_input_tokens", "agent.models.0.slots", "agent.models.0.timeout"}},
		{"a wrong type in an entry", `models = [{ model = "swap/gemma", slots = "two" }]` + "\n", []string{"agent.models.0.slots"}},
		{"a bad judge chain", "model = \"swap/gemma\"\n[agent.judge]\nmodel = \"swap/gemma\"\nmodels = [\"cpa/unsized\"]\n", []string{"agent.judge.models"}},
		{"bad judge entries", "model = \"swap/gemma\"\n[agent.judge]\nmodels = [{ model = \"cpa/unsized\", max_input_tokens = -1, output = \"xml\" }, \"cpa/missing\"]\n",
			[]string{"agent.judge.models.0.max_input_tokens", "agent.judge.models.0.output", "agent.judge.models.1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, withAgent(chainProviders, c.agent))
			if got := problemKeys(t, err); !slices.Equal(got, c.keys) {
				t.Errorf("keys = %q, want %q\n%v", got, c.keys, err)
			}
		})
	}
	// Messages name keys, never values, wherever a secret is pasted.
	_, err := load(t, withAgent(chainProviders, `models = ["swap/`+sentinel+`", { model = "`+sentinel+`" }, { model = "swap/gemma", `+
		`output = "`+sentinel+`", timeout = "`+sentinel+`", slots = "`+sentinel+`" }, { model = "swap/gemma", x_`+sentinel+` = "`+sentinel+`" }]`+"\n"))
	if err == nil {
		t.Fatal("no error")
	}
	if msg := err.Error(); strings.Count(msg, sentinel) != 1 || !strings.Contains(msg, "agent.models.3.x_"+sentinel) {
		// An unknown key is named, as everywhere in the file; its value
		// never is.
		t.Errorf("the error quotes a value:\n%v", err)
	}
}

// A variant of a base that names no served model serves the base's id,
// never its own alias.
func TestVariantServesItsBasesID(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	c, err := load(t, minimal+`
[providers.p]
base_url = "http://h.example/v1"
models = [
  { id = "real" },
  { id = "judge", base = "real", timeout = "8s" },
  { id = "judge-fast", base = "judge" },
]
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"real", "judge", "judge-fast"} {
		if m, ok := c.Providers[0].Model(id); !ok || m.Served != "real" {
			t.Errorf("%s serves %q, want real", id, m.Served)
		}
	}
}
