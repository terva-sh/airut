// Package agent runs airut's agent turns on terva's engine. It is the one
// package in airut that imports terva.
package agent

import (
	"terva.sh/terva/packages/provider"

	"terva.sh/airut/config"
)

// Providers builds a terva client for each provider in c, named by the
// provider's id, and a model catalog holding every model c lists. Each
// client reads model metadata (output limits, reasoning) through that
// catalog. It makes no network request.
//
// The catalog lists a model by its served id, the id a request carries.
// Where variants serve one model, the first listed describes it.
func Providers(c *config.Config) (map[string]provider.Client, *provider.Registry) {
	reg := provider.NewRegistry()
	clients := make(map[string]provider.Client, len(c.Providers))
	for _, p := range c.Providers {
		seen := map[string]bool{}
		for _, m := range p.Models {
			served := m.Served
			if served == "" {
				served = m.ID
			}
			if !seen[served] {
				seen[served] = true
				reg.RegisterExtraModel(catalogModel(p.ID, served, m.Capabilities))
			}
		}
		clients[p.ID] = provider.WithCatalog(provider.NewOpenAICompatibleAs(p.ID, p.APIKey.Value(), p.BaseURL), reg)
	}
	return clients, reg
}

// catalogModel describes a served model to terva.
// An unset image_input leaves terva's default, which accepts images.
func catalogModel(providerID, served string, c config.Capabilities) provider.Model {
	m := provider.Model{
		Provider:      providerID,
		ID:            served,
		DisplayName:   served,
		ContextWindow: c.ContextWindow,
		MaxOutput:     c.MaxOutput,
		Reasoning:     c.Reasoning,
	}
	if c.ImageInput != nil {
		m.Caps = map[provider.Capability]bool{provider.CapImageInput: *c.ImageInput}
	}
	return m
}
