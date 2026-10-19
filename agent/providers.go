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
func Providers(c *config.Config) (map[string]provider.Client, *provider.Registry) {
	reg := provider.NewRegistry()
	clients := make(map[string]provider.Client, len(c.Providers))
	for _, p := range c.Providers {
		for _, m := range p.Models {
			reg.RegisterExtraModel(provider.Model{
				Provider:      p.ID,
				ID:            m.ID,
				DisplayName:   m.ID,
				ContextWindow: m.ContextWindow,
				MaxOutput:     m.MaxOutput,
				Reasoning:     m.Reasoning,
			})
		}
		clients[p.ID] = provider.WithCatalog(provider.NewOpenAICompatibleAs(p.ID, p.APIKey.Value(), p.BaseURL), reg)
	}
	return clients, reg
}
