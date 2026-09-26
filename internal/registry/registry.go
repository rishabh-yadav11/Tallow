// Package registry holds the live provider/alias snapshot. It is swapped
// atomically on config hot-reload so no request ever observes a half-applied
// configuration.
package registry

import (
	"sync"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// Registry is the concurrency-safe snapshot of providers and aliases.
//
// WHAT IS ATOMIC: Swap replaces the provider and alias maps under one lock, so
// a reader never observes a half-built snapshot - it sees the whole old
// generation or the whole new one.
//
// WHAT IS NOT ATOMIC: a registry swap is NOT atomic with respect to anything
// derived from the snapshot by another object. The router, in particular, keeps
// its budget map under its own lock, and it reads both. The invariant
// "every routable key has a budget entry" therefore spans two locks and cannot
// be made atomic by this type alone. Audit finding C4 is exactly that gap, and
// it is closed by two things together: internal/app publishes budgets BEFORE
// swapping the registry (so a key is never routable before its budget exists),
// and internal/routing fails closed when a routable key has no budget rather
// than treating the gap as unlimited.
//
// Do not read "swapped atomically" as covering the budget map, and do not add a
// new derived map to the router without considering this boundary.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]*model.Provider
	aliases   map[string]*model.Alias
	// version increments on every Swap. It lets readers that memoize data
	// derived from the snapshot (routing's candidate cache) detect a change
	// with a single integer compare instead of deep-comparing the snapshot.
	version uint64
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
	r.version++
	r.mu.Unlock()
}

// Version reports the current snapshot generation. Any value derived from the
// snapshot (and cached by a caller) is stale once this number changes.
func (r *Registry) Version() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
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
