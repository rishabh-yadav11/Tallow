package health

import (
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// Registry owns circuit breakers keyed by provider and by provider/key. It is
// shared between the proactive health checker and the request path so reactive
// failures and proactive probes drive the same state.
type Registry struct {
	mu    sync.Mutex
	provs map[string]*Breaker
	keys  map[string]*Breaker // "provider/key".
}

// NewRegistry builds an empty breaker registry.
func NewRegistry() *Registry {
	return &Registry{provs: map[string]*Breaker{}, keys: map[string]*Breaker{}}
}

// Provider returns (creating if needed) the breaker for a provider.
func (r *Registry) Provider(name string) *Breaker {
	return r.get("p:"+name, &r.provs)
}

// Key returns (creating if needed) the breaker for provider/key.
func (r *Registry) Key(provider, key string) *Breaker {
	return r.get(provider+"/"+key, &r.keys)
}

func (r *Registry) get(id string, m *map[string]*Breaker) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := (*m)[id]; ok {
		return b
	}
	b := NewBreaker(3, 30*time.Second)
	(*m)[id] = b
	return b
}

// Prune drops breakers for providers/keys no longer present, so hot-reload
// cannot leak breakers or leave stale open/half-open state behind.
func (r *Registry) Prune(providers []model.Provider) {
	keepP := map[string]bool{}
	keepK := map[string]bool{}
	for _, p := range providers {
		keepP["p:"+p.Name] = true
		for _, k := range p.Keys {
			keepK[p.Name+"/"+k.ID] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.provs {
		if !keepP[id] {
			delete(r.provs, id)
		}
	}
	for id := range r.keys {
		if !keepK[id] {
			delete(r.keys, id)
		}
	}
}
