package config

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Provider kinds. Every kind speaks the OpenAI-compatible API; a kind
// adds what airut can ask the server beyond it.
const (
	// KindOpenAI is a plain OpenAI-compatible endpoint, and the default.
	KindOpenAI = "openai"
	// KindLlamaSwap is llama-swap, which loads one model at a time and
	// reports the loaded ones at RunningURL.
	KindLlamaSwap = "llama-swap"
)

// Load policies: what a llama-swap provider does with a model its
// /running list does not show.
const (
	// LoadNever treats the model as unavailable, and is the default.
	LoadNever = "never"
	// LoadWhenIdle uses the model only when no model is loaded, so
	// loading it evicts nothing.
	LoadWhenIdle = "when_idle"
	// LoadAlways uses the model regardless, and loading it evicts
	// whatever is loaded.
	LoadAlways = "always"
)

// Output modes: how a call holds a reply to a JSON shape. Empty means
// the caller's default.
const (
	OutputResponseFormat = "response_format"
	OutputJSONSchema     = "json_schema"
	OutputGrammar        = "grammar"
	OutputNone           = "none"
)

// Provider is one OpenAI-compatible endpoint.
type Provider struct {
	// ID names the provider; models are referred to as ID/model.
	ID      string
	BaseURL string
	// Kind is KindOpenAI or KindLlamaSwap.
	Kind string
	// Load is the llama-swap load policy, one of the Load constants.
	// It is empty for any other kind.
	Load string
	// APIKey is zero for a keyless endpoint, such as a local server.
	APIKey Secret
	Models []Model
}

// RunningURL is where a llama-swap provider lists its loaded models:
// BaseURL without a trailing "/" or "/v1", with "/running" added. It is
// empty for any other kind.
func (p Provider) RunningURL() string {
	if p.Kind != KindLlamaSwap {
		return ""
	}
	base := strings.TrimRight(p.BaseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	return strings.TrimRight(base, "/") + "/running"
}

// Model returns the model the provider lists under id.
func (p Provider) Model(id string) (Model, bool) {
	i := slices.IndexFunc(p.Models, func(m Model) bool { return m.ID == id })
	if i < 0 {
		return Model{}, false
	}
	return p.Models[i], true
}

// Capabilities describe a model and how a call to it is made. A zero
// field is unknown, or leaves the setting to the caller's default.
type Capabilities struct {
	// ContextWindow and MaxOutput are in tokens. ContextWindow is the
	// context one request gets: on a llama.cpp server with parallel
	// slots, one slot's share.
	ContextWindow int
	MaxOutput     int
	// Reasoning marks a model that thinks before it answers.
	Reasoning bool
	// Output is one of the Output constants, or empty.
	Output string
	// Timeout bounds one call.
	Timeout time.Duration
	// Slots, if positive, is how many llama.cpp slots a caller may pin
	// requests to; CachePrompt sends llama.cpp's cache_prompt.
	Slots       int
	CachePrompt bool
	// Temperature is nil to leave it to the caller.
	Temperature *float64
}

// Model is one model a provider lists.
type Model struct {
	// ID is what use sites name, as provider/ID.
	ID string
	// Served is the model id sent to the server. Two entries may serve
	// the same model with different capabilities, as variants.
	Served string
	// Base is the entry ID inherits its unset fields from, or empty.
	Base string
	// Capabilities are the effective ones, after Base.
	Capabilities
}

// ModelRef names a model by its provider.
type ModelRef struct {
	Provider string
	Model    string
}

func (r ModelRef) String() string { return r.Provider + "/" + r.Model }

// Chain is the models a use site tries, in order. Load guarantees a
// chain from a use site has at least one entry.
type Chain []ChainEntry

// ChainEntry is one model in a Chain, as the use site sees it.
type ChainEntry struct {
	Provider string
	// Model is the entry's id in its provider, the alias logs name.
	Model string
	// Served is the model id sent to the server. Availability and
	// health are a property of the served model, not the alias.
	Served string
	// Capabilities are the effective ones: the model's, after its base,
	// overridden by the use site's.
	Capabilities
	// MaxInputTokens caps a judge call's whole request on this entry:
	// the use site's value, or else ContextWindow. Zero caps nothing.
	// Only the judge's chain sets it.
	MaxInputTokens int
}

// Ref names the entry's model.
func (e ChainEntry) Ref() ModelRef { return ModelRef{Provider: e.Provider, Model: e.Model} }

// Provider returns the provider with id.
func (c *Config) Provider(id string) (Provider, bool) {
	i := slices.IndexFunc(c.Providers, func(p Provider) bool { return p.ID == id })
	if i < 0 {
		return Provider{}, false
	}
	return c.Providers[i], true
}

// The file's shape for capabilities. Every field is a pointer, so that
// an unset field inherits and a field set to zero overrides.
type fileCaps struct {
	ContextWindow *int     `toml:"context_window"`
	MaxOutput     *int     `toml:"max_output"`
	Reasoning     *bool    `toml:"reasoning"`
	Output        *string  `toml:"output"`
	Timeout       *string  `toml:"timeout"`
	Slots         *int     `toml:"slots"`
	CachePrompt   *bool    `toml:"cache_prompt"`
	Temperature   *float64 `toml:"temperature"`
}

type fileProvider struct {
	BaseURL string      `toml:"base_url"`
	Kind    string      `toml:"kind"`
	Load    string      `toml:"load"`
	APIKey  any         `toml:"api_key"`
	Models  []fileModel `toml:"models"`
}

type fileModel struct {
	ID    string `toml:"id"`
	Model string `toml:"model"`
	Base  string `toml:"base"`
	fileCaps
}

// fileUse is a table entry of a use site's models list. It is decoded
// by decodeStrict, as the list holds strings and tables alike.
type fileUse struct {
	Model string `toml:"model"`
	fileCaps
	MaxInput *int `toml:"max_input_tokens"`
}

// maxTemperature is the top of OpenAI's temperature range.
const maxTemperature = 2

// checkCaps reports what is wrong with f, at key.
func checkCaps(p *problems, key string, f fileCaps) {
	for _, c := range []struct {
		name string
		n    *int
	}{{"context_window", f.ContextWindow}, {"max_output", f.MaxOutput}, {"slots", f.Slots}} {
		if c.n != nil && *c.n < 0 {
			p.add(key+"."+c.name, "cannot be negative")
		}
	}
	if f.Output != nil {
		switch *f.Output {
		case "", OutputResponseFormat, OutputJSONSchema, OutputGrammar, OutputNone:
		default:
			p.add(key+".output", `must be "response_format", "json_schema", "grammar", or "none"`)
		}
	}
	if f.Timeout != nil && *f.Timeout != "" {
		duration(p, key+".timeout", *f.Timeout)
	}
	if t := f.Temperature; t != nil && (*t < 0 || *t > maxTemperature) {
		p.addf(key+".temperature", "must be 0 to %d", maxTemperature)
	}
}

// apply sets each field of c that f sets. An empty output or timeout
// sets nothing, as it always meant the default. f has been checked, so
// a bad timeout is skipped rather than reported twice.
func (f fileCaps) apply(c *Capabilities) {
	if f.ContextWindow != nil {
		c.ContextWindow = *f.ContextWindow
	}
	if f.MaxOutput != nil {
		c.MaxOutput = *f.MaxOutput
	}
	if f.Reasoning != nil {
		c.Reasoning = *f.Reasoning
	}
	if f.Output != nil && *f.Output != "" {
		c.Output = *f.Output
	}
	if f.Timeout != nil {
		if d, err := time.ParseDuration(*f.Timeout); err == nil && d > 0 {
			c.Timeout = d
		}
	}
	if f.Slots != nil {
		c.Slots = *f.Slots
	}
	if f.CachePrompt != nil {
		c.CachePrompt = *f.CachePrompt
	}
	if f.Temperature != nil {
		t := *f.Temperature
		c.Temperature = &t
	}
}

func buildProvider(p *problems, id string, f fileProvider, src *secretSource) Provider {
	// The id is written as a key path writes it, so that a refused id
	// with a dot in it cannot share an api_key's binding with another.
	key := childKey("providers", id)
	pr := Provider{ID: id, BaseURL: f.BaseURL, Kind: f.Kind}
	if !providerIDRE.MatchString(id) {
		p.addf(key, "a provider id is lowercase letters, digits, - and _, starting with a letter or digit")
	}
	checkURL(p, key+".base_url", f.BaseURL)
	switch f.Kind {
	case "":
		pr.Kind = KindOpenAI
	case KindOpenAI, KindLlamaSwap:
	default:
		p.add(key+".kind", `must be "openai" or "llama-swap"`)
	}
	switch {
	case pr.Kind == KindLlamaSwap && f.Load == "":
		pr.Load = LoadNever
	case f.Load == "":
	case pr.Kind != KindLlamaSwap:
		p.add(key+".load", `applies only to a provider of kind "llama-swap"`)
	case f.Load == LoadNever, f.Load == LoadWhenIdle, f.Load == LoadAlways:
		pr.Load = f.Load
	default:
		p.add(key+".load", `must be "never", "when_idle", or "always"`)
	}
	if f.APIKey != nil {
		pr.APIKey = resolveSecret(p, key+".api_key", f.APIKey, src)
	}
	if len(f.Models) == 0 {
		p.add(key+".models", "a provider needs at least one model")
	}
	pr.Models = buildModels(p, key, f.Models)
	return pr
}

// buildModels checks a provider's models and resolves each one's base.
func buildModels(p *problems, key string, fs []fileModel) []Model {
	index := map[string]int{}
	for i, m := range fs {
		mkey := fmt.Sprintf("%s.models.%d", key, i)
		switch _, seen := index[m.ID]; {
		case m.ID == "":
			p.add(mkey+".id", "required")
		case seen:
			p.add(mkey+".id", "repeats an earlier model's id")
		default:
			index[m.ID] = i
		}
		checkCaps(p, mkey, m.fileCaps)
	}
	for i, m := range fs {
		if m.Base == "" {
			continue
		}
		if _, ok := index[m.Base]; !ok {
			p.add(fmt.Sprintf("%s.models.%d.base", key, i), "names a model this provider does not list")
		}
	}

	// Resolve each entry by walking its bases to the root. A walk that
	// returns to an entry it passed is a cycle, reported at each entry
	// on it.
	out := make([]Model, len(fs))
	inCycle := map[int]bool{}
	for i, m := range fs {
		var path []int
		for j := i; ; {
			if k := slices.Index(path, j); k >= 0 {
				for _, c := range path[k:] {
					inCycle[c] = true
				}
				path = nil
				break
			}
			path = append(path, j)
			b, ok := index[fs[j].Base]
			if fs[j].Base == "" || !ok {
				break
			}
			j = b
		}
		res := Model{ID: m.ID, Base: m.Base}
		// The root serves its own id unless it names another, and each
		// variant inherits what its root serves.
		if len(path) > 0 {
			res.Served = fs[path[len(path)-1]].ID
		}
		// The root first, so each nearer entry overrides it.
		for k := len(path) - 1; k >= 0; k-- {
			e := fs[path[k]]
			e.fileCaps.apply(&res.Capabilities)
			if e.Model != "" {
				res.Served = e.Model
			}
		}
		if res.Served == "" {
			res.Served = m.ID
		}
		out[i] = res
	}
	for i := range fs {
		if inCycle[i] {
			p.add(fmt.Sprintf("%s.models.%d.base", key, i), "leads back to this model; bases cannot form a cycle")
		}
	}
	return out
}

// lookup finds the model ref names, reporting at key what is missing.
func lookup(p *problems, key string, ref ModelRef, providers []Provider) (Model, bool) {
	i := slices.IndexFunc(providers, func(pr Provider) bool { return pr.ID == ref.Provider })
	if i < 0 {
		p.add(key, "names a provider that is not configured")
		return Model{}, false
	}
	m, ok := providers[i].Model(ref.Model)
	if !ok {
		p.add(key, "names a model its provider does not list")
	}
	return m, ok
}

// parseRef reads a model named as "provider/model" at key.
func parseRef(p *problems, key, raw string) (ModelRef, bool) {
	provider, model, ok := strings.Cut(raw, "/")
	switch {
	case raw == "":
		p.add(key, `required: a model, as "provider/model"`)
		return ModelRef{}, false
	case !ok || provider == "" || model == "":
		p.add(key, `a model is named as "provider/model"`)
		return ModelRef{}, false
	}
	return ModelRef{Provider: provider, Model: model}, true
}

// useSite is what buildChain needs to know of where a chain is used.
type useSite struct {
	// key is the use site's table, as "agent.judge".
	key string
	// caps are the table's own overrides, which every entry takes
	// before its own.
	caps fileCaps
	// maxInput is the table's max_input_tokens, or nil.
	maxInput *int
	// judge says whether entries may set max_input_tokens, and whether
	// MaxInputTokens is resolved.
	judge bool
}

// buildChain reads a use site's model or models, of which the file sets
// exactly one, into its chain. Each entry's capabilities take the
// model's, then the use site's table, then the entry's own table.
func buildChain(p *problems, u useSite, model string, models any, providers []Provider) Chain {
	type entry struct {
		key string
		ref ModelRef
		own fileUse
	}
	var entries []entry
	switch {
	case model != "" && models != nil:
		p.add(u.key+".models", "set model or models, not both")
		return nil
	case models == nil:
		ref, ok := parseRef(p, u.key+".model", model)
		if !ok {
			return nil
		}
		entries = append(entries, entry{key: u.key + ".model", ref: ref})
	default:
		list, ok := models.([]any)
		if !ok {
			p.add(u.key+".models", `the value is not an array of models, each "provider/model" or a table`)
			return nil
		}
		if len(list) == 0 {
			p.add(u.key+".models", "lists no model")
			return nil
		}
		for i, item := range list {
			key := fmt.Sprintf("%s.models.%d", u.key, i)
			var e entry
			switch v := item.(type) {
			case string:
				e.key = key
				e.ref, ok = parseRef(p, key, v)
			case map[string]any:
				e.key = key + ".model"
				if !decodeStrict(p, key, v, &e.own) {
					continue
				}
				checkCaps(p, key, e.own.fileCaps)
				if e.own.MaxInput != nil {
					if !u.judge {
						p.add(key+".max_input_tokens", "applies only to the judge")
					} else if *e.own.MaxInput < 0 {
						p.add(key+".max_input_tokens", "must not be negative")
					}
				}
				e.ref, ok = parseRef(p, e.key, e.own.Model)
			default:
				p.add(key, `the value is not "provider/model" or a table`)
				ok = false
			}
			if ok {
				entries = append(entries, e)
			}
		}
	}

	var chain Chain
	for _, e := range entries {
		m, ok := lookup(p, e.key, e.ref, providers)
		if !ok {
			continue
		}
		c := ChainEntry{Provider: e.ref.Provider, Model: m.ID, Served: m.Served, Capabilities: m.Capabilities}
		u.caps.apply(&c.Capabilities)
		e.own.fileCaps.apply(&c.Capabilities)
		if u.judge {
			switch {
			case e.own.MaxInput != nil:
				c.MaxInputTokens = max(*e.own.MaxInput, 0)
			case u.maxInput != nil:
				c.MaxInputTokens = max(*u.maxInput, 0)
			default:
				c.MaxInputTokens = c.ContextWindow
			}
		}
		chain = append(chain, c)
	}
	return chain
}

// decodeStrict decodes raw, the table at key, into out, a pointer to a
// struct with toml tags, refusing keys the struct does not name. It
// re-encodes raw and decodes it, so a value converts as it would in the
// file. Problems name keys under key and never quote a value.
func decodeStrict(p *problems, key string, raw map[string]any, out any) bool {
	src, err := toml.Marshal(raw)
	if err == nil {
		err = toml.NewDecoder(bytes.NewReader(src)).DisallowUnknownFields().Decode(out)
	}
	if err == nil {
		return true
	}
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		for _, e := range strict.Errors {
			p.add(key+"."+strings.Join(e.Key(), "."), "unknown key; docs/config.example.toml lists every key")
		}
		return false
	}
	var dec *toml.DecodeError
	if errors.As(err, &dec) && len(dec.Key()) > 0 {
		k := strings.Join(dec.Key(), ".")
		if want := expectsIn(reflect.TypeOf(out), k); want != "" {
			p.add(key+"."+k, "the value is not "+want)
		} else {
			p.add(key+"."+k, "the value is not one this key accepts")
		}
		return false
	}
	p.add(key, "the value is not a table this key accepts")
	return false
}
