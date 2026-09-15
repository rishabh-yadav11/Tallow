// Package routing implements model-alias resolution and two-layer selection
// (provider, then key), sticky session affinity, and ordered fallback.
package routing

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

// Router resolves a client-facing alias to a concrete provider/key and
// upstream model, honoring sticky affinity, hard budgets, and health state.
type Router struct {
	reg     *registry.Registry
	health  *health.Registry
	budgets map[string]*budget.KeyBudget      // "provider/key".
	pBudget map[string]*budget.ProviderBudget // "provider".
	sticky  *Sticky
	now     func() time.Time

	bmu  sync.RWMutex // guards budgets + pBudget against hot-reload writes.
	rrmu sync.Mutex
	rr   map[string]int // provider -> round-robin offset.
}

// NewRouter wires the router. stickyTTL <= 0 disables sticky affinity.
func NewRouter(reg *registry.Registry, h *health.Registry, stickyTTL time.Duration, now func() time.Time) *Router {
	if now == nil {
		now = time.Now
	}
	return &Router{
		reg:     reg,
		health:  h,
		budgets: map[string]*budget.KeyBudget{},
		pBudget: map[string]*budget.ProviderBudget{},
		sticky:  NewSticky(stickyTTL),
		now:     now,
		rr:      map[string]int{},
	}
}

// SetBudgets (re)installs budget trackers. Existing trackers are preserved by
// key so hot-reload does not reset live windows.
func (r *Router) SetBudgets(keys map[string]budget.KeyLimits, providers map[string]int) {
	r.bmu.Lock()
	defer r.bmu.Unlock()
	for id, l := range keys {
		if _, ok := r.budgets[id]; !ok {
			r.budgets[id] = budget.NewKeyBudget(l)
		}
	}
	// Drop budgets for keys no longer present.
	for id := range r.budgets {
		if _, ok := keys[id]; !ok {
			delete(r.budgets, id)
		}
	}
	for id, rpm := range providers {
		if _, ok := r.pBudget[id]; !ok {
			r.pBudget[id] = budget.NewProviderBudget(rpm)
		}
	}
}

// Selection is a routed, budget-reserved destination.
type Selection struct {
	Alias      string
	SessionKey string
	Provider   string
	Key        string
	Model      string // upstream model string.
	Target     model.Target
	Reason     string // routing path + why (for observability).

	BaseURL string
	Secret  string
	Timeout time.Duration

	kb *budget.KeyBudget
}

// Candidates are the ordered provider/model pairs for an alias, including
// provider-level fallback chains expanded inline.
type candidate struct {
	provider string
	model    string
	target   model.Target
	targetOK bool
}

func (r *Router) candidates(alias *model.Alias) []candidate {
	var out []candidate
	seen := map[string]bool{}
	for _, t := range alias.Targets {
		if !seen[t.Provider] {
			out = append(out, candidate{provider: t.Provider, model: t.Model, target: t, targetOK: true})
			seen[t.Provider] = true
		}
		// Expand provider fallback chain with the same upstream model.
		if p, ok := r.reg.Provider(t.Provider); ok {
			for _, f := range p.Fallback {
				if !seen[f] {
					out = append(out, candidate{provider: f, model: t.Model, target: t})
					seen[f] = true
				}
			}
		}
	}
	return out
}

// Select routes alias to a provider/key, consuming budgets. skip is a set of
// "provider/key" (or bare "provider") ids already failed in this attempt chain,
// so retries avoid them.
func (r *Router) Select(alias, sessionKey string, skip map[string]bool) (*Selection, error) {
	now := r.now()
	a, ok := r.reg.Alias(alias)
	if !ok {
		return nil, fmt.Errorf("unknown alias %q", alias)
	}

	var reasons []string

	// Sticky affinity first: keep session on its provider/key for cache reuse.
	if sessionKey != "" {
		if p, k, tgt, ok := r.sticky.Get(sessionKey, now); ok && !skip[p+"/"+k] && !skip[p] {
			// Health gate: an unhealthy key/provider must not be re-pinned.
			if r.health.Provider(p).Allow(now) && r.health.Key(p, k).Allow(now) {
				if sel, err := r.acquire(p, k, tgt.Model, tgt, now); err == nil {
					if sel.BaseURL != "" {
						sel.Alias = alias
						sel.SessionKey = sessionKey
						sel.Model = tgt.Model
						sel.Target = tgt
						sel.Reason = finalReason(reasons, "sticky:"+p+"/"+k)
						r.sticky.Put(sessionKey, p, k, tgt)
						return sel, nil
					}
					// Provider/key removed from registry on hot-reload: the pin
					// is stale. Forget it and route normally.
					reasons = append(reasons, "sticky:"+p+"/"+k+":removed")
					r.sticky.Forget(sessionKey)
				}
			}
			reasons = append(reasons, "sticky:"+p+"/"+k+":stale")
			r.sticky.Forget(sessionKey)
		}
	}

	for _, c := range r.candidates(a) {
		provID := "provider:" + c.provider
		if skip[c.provider] {
			reasons = append(reasons, provID+":skip")
			continue
		}
		b := r.health.Provider(c.provider)
		if !b.Allow(now) {
			reasons = append(reasons, provID+":breaker_open")
			continue
		}
		// Provider-level RPM is consumed only after a key is secured.
		r.bmu.RLock()
		pb := r.pBudget[c.provider]
		r.bmu.RUnlock()
		if pb != nil && pb.Remaining(now) == 0 {
			reasons = append(reasons, provID+":provider_rpm")
			continue
		}
		sel, err := r.acquireKey(c.provider, now, &reasons, skip)
		if err != nil {
			reasons = append(reasons, provID+":no_key")
			continue
		}
		if sel.BaseURL == "" {
			// Provider was removed from the registry on hot-reload; skip this
			// candidate rather than forwarding to an empty upstream URL.
			sel.kb.Release(0)
			reasons = append(reasons, "provider:"+c.provider+":removed")
			continue
		}
		if pb != nil && !pb.Allow(now) {
			sel.kb.Release(0)
			reasons = append(reasons, provID+":provider_rpm_race")
			continue
		}
		sel.Target = c.target
		sel.Model = c.model
		sel.Alias = alias
		sel.SessionKey = sessionKey
		sel.Reason = finalReason(reasons, provID+"/key:"+sel.Key)
		if sessionKey != "" {
			r.sticky.Put(sessionKey, c.provider, sel.Key, c.target)
		}
		return sel, nil
	}

	return nil, fmt.Errorf("no route for %q (reasons: %s)", alias, strings.Join(reasons, "; "))
}

// finalReason renders the routing trail: skipped candidates, then the choice.
func finalReason(trail []string, final string) string {
	if len(trail) == 0 {
		return final
	}
	return strings.Join(trail, "; ") + " -> " + final
}

// acquire reserves a specific provider/key (used by sticky path).
func (r *Router) acquire(provider, key, model string, target model.Target, now time.Time) (*Selection, error) {
	id := provider + "/" + key
	r.bmu.RLock()
	kb := r.budgets[id]
	r.bmu.RUnlock()
	if kb != nil {
		if ok, _ := kb.Acquire(now); !ok {
			return nil, fmt.Errorf("budget exhausted")
		}
	}
	sel := &Selection{Provider: provider, Key: key, Model: model, Target: target, kb: kb}
	fillDest(r.reg, sel)
	return sel, nil
}

// fillDest resolves base URL, key secret, and timeout onto a selection.
func fillDest(reg *registry.Registry, sel *Selection) {
	if p, ok := reg.Provider(sel.Provider); ok {
		sel.BaseURL = p.BaseURL
		sel.Timeout = p.Timeout
		for _, k := range p.Keys {
			if k.ID == sel.Key {
				sel.Secret = k.Secret
				break
			}
		}
	}
}

// acquireKey selects a key within a provider (round-robin), consuming its
// budget on success.
func (r *Router) acquireKey(provider string, now time.Time, reasons *[]string, skip map[string]bool) (*Selection, error) {
	p, ok := r.reg.Provider(provider)
	if !ok || len(p.Keys) == 0 {
		return nil, fmt.Errorf("no keys")
	}
	n := len(p.Keys)

	r.rrmu.Lock()
	start := r.rr[provider] % n
	r.rr[provider] = start + 1
	r.rrmu.Unlock()

	for i := 0; i < n; i++ {
		k := p.Keys[(start+i)%n]
		if skip[provider+"/"+k.ID] {
			*reasons = append(*reasons, "key:"+k.ID+":skip")
			continue
		}
		bk := r.health.Key(provider, k.ID)
		if !bk.Allow(now) {
			*reasons = append(*reasons, "key:"+k.ID+":breaker_open")
			continue
		}
		id := provider + "/" + k.ID
		r.bmu.RLock()
		kb := r.budgets[id]
		r.bmu.RUnlock()
		if kb != nil {
			if ok, reason := kb.Acquire(now); !ok {
				*reasons = append(*reasons, "key:"+k.ID+":"+reason)
				continue
			}
		}
		sel := &Selection{Provider: provider, Key: k.ID, kb: kb}
		fillDest(r.reg, sel)
		return sel, nil
	}
	return nil, fmt.Errorf("no usable key")
}

// Release returns the reserved in-flight slot and accumulates cost.
func (r *Router) Release(sel *Selection, costCents int64) {
	if sel == nil || sel.kb == nil {
		return
	}
	sel.kb.Release(costCents)
}

// RecordSuccess closes the relevant breakers and re-affirms sticky.
func (r *Router) RecordSuccess(sel *Selection) {
	now := r.now()
	r.health.Provider(sel.Provider).RecordSuccess(now)
	r.health.Key(sel.Provider, sel.Key).RecordSuccess(now)
}

// RecordFailure accounts a failure toward breaker opening.
func (r *Router) RecordFailure(sel *Selection) {
	now := r.now()
	r.health.Provider(sel.Provider).RecordFailure(now)
	r.health.Key(sel.Provider, sel.Key).RecordFailure(now)
	if sel.SessionKey != "" {
		r.sticky.Forget(sel.SessionKey)
	}
}

// ---------- Sticky ----------

// Sticky maps session keys to a pinned provider/key/target with idle expiry.
type Sticky struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]stickyEntry
}

type stickyEntry struct {
	provider, key string
	target        model.Target
	at            time.Time
}

// NewSticky builds a sticky store. ttl <= 0 disables expiry (always sticky).
func NewSticky(ttl time.Duration) *Sticky {
	return &Sticky{ttl: ttl, m: map[string]stickyEntry{}}
}

// Get returns a live (non-idle-expired) pin.
func (s *Sticky) Get(sessionKey string, now time.Time) (string, string, model.Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[sessionKey]
	if !ok {
		return "", "", model.Target{}, false
	}
	if s.ttl > 0 && now.Sub(e.at) > s.ttl {
		delete(s.m, sessionKey)
		return "", "", model.Target{}, false
	}
	e.at = now
	s.m[sessionKey] = e
	return e.provider, e.key, e.target, true
}

// Put records a pin.
func (s *Sticky) Put(sessionKey, provider, key string, target model.Target) {
	s.mu.Lock()
	s.m[sessionKey] = stickyEntry{provider: provider, key: key, target: target, at: time.Now()}
	s.mu.Unlock()
}

// Forget drops a pin.
func (s *Sticky) Forget(sessionKey string) {
	s.mu.Lock()
	delete(s.m, sessionKey)
	s.mu.Unlock()
}

// BudgetStatus is a live per-key budget view for the admin API/TUI.
type BudgetStatus struct {
	Provider     string `json:"provider"`
	Key          string `json:"key"`
	RPMRemaining int    `json:"rpm_remaining"`
	Inflight     int    `json:"inflight"`
	MaxInflight  int    `json:"max_inflight"`
	CostCents    int64  `json:"cost_cents"`
	CostLimit    int64  `json:"cost_limit_cents"`
}

// Budgets returns live status for every tracked key budget.
func (r *Router) Budgets(now time.Time) []BudgetStatus {
	r.bmu.RLock()
	defer r.bmu.RUnlock()
	var out []BudgetStatus
	for id, kb := range r.budgets {
		prov, key := splitID(id)
		s := kb.Snapshot(now)
		out = append(out, BudgetStatus{
			Provider:     prov,
			Key:          key,
			RPMRemaining: s.RPMRemaining,
			Inflight:     s.Inflight,
			MaxInflight:  s.MaxInflight,
			CostCents:    s.CostCents,
			CostLimit:    s.CostLimitCents,
		})
	}
	return out
}

// ProviderRPM reports remaining provider-level RPM headroom; -1 = no cap.
func (r *Router) ProviderRPM(provider string, now time.Time) int {
	r.bmu.RLock()
	pb, ok := r.pBudget[provider]
	r.bmu.RUnlock()
	if ok {
		return pb.Remaining(now)
	}
	return -1
}

func splitID(id string) (string, string) {
	i := strings.Index(id, "/")
	if i < 0 {
		return id, ""
	}
	return id[:i], id[i+1:]
}
