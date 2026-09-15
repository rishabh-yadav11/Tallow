// Package registry holds the live provider/alias snapshot. It is swapped
// atomically on config hot-reload so no request ever observes a half-applied
// configuration.
package registry

import (
	"sync"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// Registry is the concurrency-safe snapshot of providers and aliases.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]*model.Provider
	aliases   map[string]*model.Alias
}

// New builds a registry from resolved providers (key secrets already filled)
// and aliases.
func New(providers []model.Provider, aliases []model.Alias) *Registry {
	r := &Registry{
		providers: make(map[string]*model.Provider, len(providers)),
		aliases:   make(map[string]*model.Alias, len(aliases)),
	}
	r.Swap(providers, aliases)
	return r
}

// Swap replaces the entire snapshot atomically.
func (r *Registry) Swap(providers []model.Provider, aliases []model.Alias) {
	pm := make(map[string]*model.Provider, len(providers))
	for i := range providers {
		p := providers[i]
		pm[p.Name] = &p
	}
	am := make(map[string]*model.Alias, len(aliases))
	for i := range aliases {
		a := aliases[i]
		am[a.Name] = &a
	}
	r.mu.Lock()
	r.providers = pm
	r.aliases = am
	r.mu.Unlock()
}

// Provider returns a provider by name (the caller must not mutate it).
func (r *Registry) Provider(name string) (*model.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Alias returns an alias by name.
func (r *Registry) Alias(name string) (*model.Alias, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.aliases[name]
	return a, ok
}

// Providers returns a copy of all providers.
func (r *Registry) Providers() []model.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, *p)
	}
	return out
}

// Aliases returns a copy of all aliases.
func (r *Registry) Aliases() []model.Alias {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.Alias, 0, len(r.aliases))
	for _, a := range r.aliases {
		out = append(out, *a)
	}
	return out
}

// AliasNames returns the set of client-facing model names.
func (r *Registry) AliasNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.aliases))
	for n := range r.aliases {
		out = append(out, n)
	}
	return out
}
