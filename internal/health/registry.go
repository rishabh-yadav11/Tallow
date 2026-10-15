package health

import (
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

type Registry struct {
	mu    sync.Mutex
	provs map[string]*Breaker
	keys  map[string]*Breaker
}

func NewRegistry() *Registry {
	return &Registry{provs: map[string]*Breaker{}, keys: map[string]*Breaker{}}
}

func (r *Registry) Provider(name string) *Breaker {
	return r.get("p:"+name, &r.provs)
}

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
