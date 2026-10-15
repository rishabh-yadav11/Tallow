package registry

import (
	"sync"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

type Registry struct {
	mu        sync.RWMutex
	providers map[string]*model.Provider
	aliases   map[string]*model.Alias

	version uint64
}

func New(providers []model.Provider, aliases []model.Alias) *Registry {
	r := &Registry{
		providers: make(map[string]*model.Provider, len(providers)),
		aliases:   make(map[string]*model.Alias, len(aliases)),
	}
	r.Swap(providers, aliases)
	return r
}

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

func (r *Registry) Version() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
}

func (r *Registry) Provider(name string) (*model.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

func (r *Registry) Alias(name string) (*model.Alias, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.aliases[name]
	return a, ok
}

func (r *Registry) Providers() []model.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, *p)
	}
	return out
}

func (r *Registry) Aliases() []model.Alias {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.Alias, 0, len(r.aliases))
	for _, a := range r.aliases {
		out = append(out, *a)
	}
	return out
}

func (r *Registry) AliasNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.aliases))
	for n := range r.aliases {
		out = append(out, n)
	}
	return out
}
