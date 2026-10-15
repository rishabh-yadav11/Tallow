package routing

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/money"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

type Strategy string

const (
	StrategyRoundRobin Strategy = ""

	StrategyWeightedRoundRobin Strategy = "weighted-round-robin"

	StrategyFillFirst Strategy = "fill-first"
)

type SessionAffinity struct {
	Enabled bool

	TTL time.Duration

	RebindOn string

	Subagents bool
}

type Router struct {
	reg      *registry.Registry
	health   *health.Registry
	budgets  map[string]*budget.KeyBudget
	pBudget  map[string]*budget.ProviderBudget
	sticky   *Sticky
	now      func() time.Time
	strategy Strategy
	excluder func(provider, key string) bool

	affEnabled   bool
	affRebindAny bool
	affSubagents bool

	bmu  sync.RWMutex
	rrmu sync.Mutex
	rr   map[string]int

	cmu     sync.Mutex
	candVer uint64
	candMap map[string][]candidate
}

func NewRouter(reg *registry.Registry, h *health.Registry, stickyTTL time.Duration, now func() time.Time) *Router {
	return NewRouterWithOpts(reg, h, stickyTTL, now, StrategyRoundRobin)
}

func NewRouterWithOpts(reg *registry.Registry, h *health.Registry, stickyTTL time.Duration, now func() time.Time, strategy Strategy) *Router {
	if now == nil {
		now = time.Now
	}
	return &Router{
		reg:      reg,
		health:   h,
		budgets:  map[string]*budget.KeyBudget{},
		pBudget:  map[string]*budget.ProviderBudget{},
		sticky:   NewSticky(stickyTTL, now),
		now:      now,
		strategy: strategy,
		rr:       map[string]int{},

		candMap: map[string][]candidate{},
	}
}

func (r *Router) SetExcluder(f func(provider, key string) bool) {
	r.excluder = f
}

func (r *Router) SetSessionAffinity(cfg SessionAffinity) {
	r.affEnabled = cfg.Enabled
	r.affRebindAny = cfg.RebindOn == "any"
	r.affSubagents = cfg.Subagents && cfg.Enabled
	if cfg.Enabled && cfg.TTL > 0 {
		r.sticky.SetTTL(cfg.TTL)
	}
}

func (r *Router) SetBudgets(keys map[string]budget.KeyLimits, providers map[string]int) {
	r.bmu.Lock()
	defer r.bmu.Unlock()
	for id, l := range keys {
		if kb, ok := r.budgets[id]; ok {
			kb.SetLimits(l)
		} else {
			r.budgets[id] = budget.NewKeyBudget(l)
		}
	}

	for id := range r.budgets {
		if _, ok := keys[id]; !ok {
			delete(r.budgets, id)
		}
	}
	for id, rpm := range providers {
		if pb, ok := r.pBudget[id]; ok {
			pb.SetRPM(rpm)
		} else {
			r.pBudget[id] = budget.NewProviderBudget(rpm)
		}
	}

	for id := range r.pBudget {
		if _, ok := providers[id]; !ok {
			delete(r.pBudget, id)
		}
	}

	r.rrmu.Lock()
	for id := range r.rr {
		if _, ok := providers[id]; !ok {
			delete(r.rr, id)
		}
	}
	r.rrmu.Unlock()

	r.cmu.Lock()
	r.candMap = map[string][]candidate{}
	r.candVer = 0
	r.cmu.Unlock()
}

func (r *Router) BudgetFor(id string) (*budget.KeyBudget, bool) {
	r.bmu.RLock()
	defer r.bmu.RUnlock()
	kb, ok := r.budgets[id]
	return kb, ok
}

var errStaleSelection = errors.New("selection target no longer in registry")

var errNoBudget = errors.New("no budget installed for key")

var ErrNoRoute = errors.New("no route available")

type RouteError struct {
	Alias   string
	Reasons []string

	Limited bool

	RetryAfter time.Duration
}

func (e *RouteError) Error() string {
	return fmt.Sprintf("no route for %q (reasons: %s)", e.Alias, strings.Join(e.Reasons, "; "))
}

func (e *RouteError) Unwrap() error { return ErrNoRoute }

type Selection struct {
	Alias      string
	SessionKey string
	Provider   string
	Key        string
	Model      string
	Target     model.Target
	Reason     string

	admittedAt time.Time

	BaseURL string
	Secret  string
	Timeout time.Duration

	Headers map[string]string

	ProxyURL string

	kb *budget.KeyBudget

	pb    *budget.ProviderBudget
	pbSet bool
}

func (sel *Selection) claimProviderRPM(pb *budget.ProviderBudget) bool {
	if sel == nil || sel.pbSet {
		return false
	}
	sel.pb, sel.pbSet = pb, true
	return true
}

type candidate struct {
	provider string
	model    string
	target   model.Target
}

const maxFallbackDepth = 8

func (r *Router) candidates(alias *model.Alias) []candidate {
	var out []candidate
	seen := map[string]bool{}

	excluded := func(provider, modelName string) bool {
		p, ok := r.reg.Provider(provider)
		if !ok {

			return false
		}
		if p.Disabled {
			return true
		}
		return model.ExcludedModel(modelName, p.ExcludedModels) ||
			model.ExcludedModel(modelName, alias.ExcludedModels)
	}

	type pending struct {
		provider string
		model    string
		target   model.Target
		depth    int
	}
	var queue []pending
	for _, t := range alias.Targets {
		if excluded(t.Provider, t.Model) {
			continue
		}
		if !seen[t.Provider] {
			out = append(out, candidate{provider: t.Provider, model: t.Model, target: t})
			seen[t.Provider] = true
		}
		queue = append(queue, pending{provider: t.Provider, model: t.Model, target: t})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxFallbackDepth {
			continue
		}
		p, ok := r.reg.Provider(cur.provider)
		if !ok {
			continue
		}

		for _, f := range p.Fallback {
			if excluded(f, cur.model) {
				continue
			}
			if !seen[f] {
				out = append(out, candidate{provider: f, model: cur.model, target: cur.target})
				seen[f] = true
				queue = append(queue, pending{provider: f, model: cur.model, target: cur.target, depth: cur.depth + 1})
			}
		}
	}
	return out
}

func (r *Router) cachedCandidates(aliasName string, a *model.Alias) []candidate {
	ver := r.reg.Version()

	r.cmu.Lock()
	defer r.cmu.Unlock()
	if r.candVer == ver {
		if c, ok := r.candMap[aliasName]; ok {
			return c
		}
	}

	built := r.candidates(a)
	if r.candVer != ver {

		r.candMap = make(map[string][]candidate, len(r.candMap)+1)
		r.candVer = ver
	}
	r.candMap[aliasName] = built
	return built
}

func (r *Router) Select(alias, sessionKey string, skip map[string]bool) (*Selection, error) {
	now := r.now()
	a, ok := r.reg.Alias(alias)
	if !ok {
		return nil, fmt.Errorf("unknown alias %q", alias)
	}

	var reasons []string

	var retryAfter time.Duration

	if sessionKey != "" {
		pinKey := sessionKey
		p, k, tgt, ok := r.sticky.Get(pinKey, now)
		if !ok && r.affSubagents {
			if parent := r.sticky.ParentPrefix(sessionKey, now); parent != "" {
				pinKey = parent
				p, k, tgt, ok = r.sticky.Get(pinKey, now)
			}
		}
		if ok && !skip[p+"/"+k] && !skip[p] {

			if r.health.Provider(p).Allow(now) && r.health.Key(p, k).Allow(now) {
				sel, err := r.acquire(p, k, tgt.Model, tgt, now)
				switch {
				case err == nil:
					sel.Alias = alias
					sel.SessionKey = sessionKey
					sel.Model = tgt.Model
					sel.Target = tgt
					sel.Reason = finalReason(reasons, "sticky:"+p+"/"+k)

					r.sticky.Put(sessionKey, p, k, tgt)
					return sel, nil
				case errors.Is(err, errStaleSelection), errors.Is(err, errNoBudget):

					r.releaseSelection(sel)
					r.sticky.Forget(sessionKey)
					reasons = append(reasons, "sticky:"+p+"/"+k+":removed")
				default:

					r.releaseSelection(sel)
					if !r.affRebindAny {
						r.sticky.Forget(sessionKey)
					}
					reasons = append(reasons, "sticky:"+p+"/"+k+":exhausted")
				}
			} else {
				reasons = append(reasons, "sticky:"+p+"/"+k+":stale")
				r.sticky.Forget(sessionKey)
			}
		}
	}

	for _, c := range r.cachedCandidates(alias, a) {
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

		r.bmu.RLock()
		pb := r.pBudget[c.provider]
		r.bmu.RUnlock()
		if pb != nil && pb.Remaining(now) == 0 {
			reasons = append(reasons, provID+":provider_rpm")

			if pb != nil {
				if reset := pb.RPMResetsAt(now); retryAfter == 0 || reset.Sub(now) < retryAfter {
					retryAfter = reset.Sub(now)
				}
			}
			continue
		}
		sel, err := r.acquireKey(c.provider, now, &reasons, skip)
		if err != nil {

			r.releaseSelection(sel)
			if errors.Is(err, errStaleSelection) {
				reasons = append(reasons, "provider:"+c.provider+":removed")
			} else {
				reasons = append(reasons, provID+":no_key")
			}
			continue
		}

		if sel.BaseURL == "" {

			r.releaseSelection(sel)
			reasons = append(reasons, "provider:"+c.provider+":removed")
			continue
		}
		if pb != nil && !pb.Allow(now) {

			r.releaseSelection(sel)
			reasons = append(reasons, provID+":provider_rpm_race")
			continue
		}

		sel.claimProviderRPM(pb)
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

	return nil, newRouteErrorWithRetry(alias, reasons, retryAfter)
}

var limitReasons = map[string]bool{

	"provider_rpm":      true,
	"provider_rpm_race": true,

	"key_rpm":            true,
	"key_quota":          true,
	"key_max_concurrent": true,
	"key_cost_limit":     true,

	"exhausted": true,
}

func newRouteError(alias string, reasons []string) *RouteError {
	return newRouteErrorWithRetry(alias, reasons, 0)
}

func newRouteErrorWithRetry(alias string, reasons []string, retryAfter time.Duration) *RouteError {
	limited := len(reasons) > 0
	for _, r := range reasons {

		cause := r
		if i := strings.LastIndex(r, ":"); i >= 0 {
			cause = r[i+1:]
		}
		if !limitReasons[cause] {
			limited = false
		}
	}
	if !limited {

		retryAfter = 0
	}
	return &RouteError{Alias: alias, Reasons: reasons, Limited: limited, RetryAfter: retryAfter}
}

func finalReason(trail []string, final string) string {
	if len(trail) == 0 {
		return final
	}
	return strings.Join(trail, "; ") + " -> " + final
}

func (r *Router) releaseSelection(sel *Selection) {
	if sel == nil {
		return
	}
	if sel.kb != nil {
		sel.kb.Release(0)
	}
	if sel.pb != nil && sel.pbSet {

		sel.pb.Release(sel.accountedAt(r.now()))
	}
	sel.pb, sel.pbSet, sel.kb = nil, false, nil
}

func (r *Router) acquire(provider, key, model string, target model.Target, now time.Time) (*Selection, error) {
	id := provider + "/" + key
	r.bmu.RLock()
	kb := r.budgets[id]
	pb := r.pBudget[provider]
	r.bmu.RUnlock()

	if kb == nil {
		return nil, fmt.Errorf("%w: %s", errNoBudget, id)
	}
	if ok, _ := kb.Acquire(now); !ok {
		return nil, fmt.Errorf("budget exhausted")
	}
	if pb != nil && !pb.Allow(now) {

		kb.Release(0)
		return nil, fmt.Errorf("provider rpm exhausted")
	}
	sel := &Selection{Provider: provider, Key: key, Model: model, Target: target, kb: kb, admittedAt: now}

	sel.claimProviderRPM(pb)
	fillDest(r.reg, sel)
	if sel.BaseURL == "" {

		r.releaseSelection(sel)
		return sel, errStaleSelection
	}
	return sel, nil
}

func fillDest(reg *registry.Registry, sel *Selection) {
	if p, ok := reg.Provider(sel.Provider); ok {
		sel.BaseURL = p.BaseURL
		sel.Timeout = p.Timeout
		sel.ProxyURL = p.ProxyURL
		for _, k := range p.Keys {
			if k.ID == sel.Key {
				sel.Secret = k.Secret

				if len(k.Headers) > 0 {
					sel.Headers = k.Headers
				} else {
					sel.Headers = p.Headers
				}
				if len(sel.Headers) == 0 {

					sel.Headers = nil
				}
				if k.ProxyURL != "" {
					sel.ProxyURL = k.ProxyURL
				}
				break
			}
		}
	}
}

func (r *Router) acquireKey(provider string, now time.Time, reasons *[]string, skip map[string]bool) (*Selection, error) {
	switch r.strategy {
	case StrategyWeightedRoundRobin:
		return r.weightedAcquire(provider, now, reasons, skip)
	case StrategyFillFirst:
		return r.fillFirstAcquire(provider, now, reasons, skip)
	default:
		return r.roundRobinAcquire(provider, now, reasons, skip)
	}
}

func effWeight(w int) int {
	if w <= 0 {
		return 1
	}
	return w
}

func (r *Router) tryKey(provider string, k model.Key, now time.Time, reasons *[]string, skip map[string]bool) (*Selection, error) {
	if skip[provider+"/"+k.ID] {
		*reasons = append(*reasons, "key:"+k.ID+":skip")
		return nil, nil
	}
	if r.excluder != nil && r.excluder(provider, k.ID) {
		*reasons = append(*reasons, "key:"+k.ID+":excluded")
		return nil, nil
	}
	bk := r.health.Key(provider, k.ID)
	if !bk.Allow(now) {
		*reasons = append(*reasons, "key:"+k.ID+":breaker_open")
		return nil, nil
	}
	id := provider + "/" + k.ID
	r.bmu.RLock()
	kb := r.budgets[id]
	r.bmu.RUnlock()

	if kb == nil {
		*reasons = append(*reasons, "key:"+k.ID+":no_budget")
		return nil, nil
	}
	if ok, reason := kb.Acquire(now); !ok {
		*reasons = append(*reasons, "key:"+k.ID+":"+reason)
		return nil, nil
	}
	sel := &Selection{Provider: provider, Key: k.ID, kb: kb, admittedAt: now}
	fillDest(r.reg, sel)
	if sel.BaseURL == "" {

		kb.Release(0)
		sel.kb = nil
		return sel, errStaleSelection
	}
	return sel, nil
}

func (r *Router) roundRobinAcquire(provider string, now time.Time, reasons *[]string, skip map[string]bool) (*Selection, error) {
	p, ok := r.reg.Provider(provider)
	if !ok || len(p.Keys) == 0 {
		return nil, fmt.Errorf("no keys")
	}
	n := len(p.Keys)

	r.rrmu.Lock()
	start := r.rr[provider] % n
	r.rr[provider] = start + 1
	r.rrmu.Unlock()

	for i := range n {
		sel, err := r.tryKey(provider, p.Keys[(start+i)%n], now, reasons, skip)
		if err != nil {
			return sel, err
		}
		if sel != nil {
			return sel, nil
		}
	}
	return nil, fmt.Errorf("no usable key")
}

func (r *Router) weightedAcquire(provider string, now time.Time, reasons *[]string, skip map[string]bool) (*Selection, error) {
	p, ok := r.reg.Provider(provider)
	if !ok || len(p.Keys) == 0 {
		return nil, fmt.Errorf("no keys")
	}
	n := len(p.Keys)
	total := 0
	for _, k := range p.Keys {
		total += effWeight(k.Weight)
	}

	for range n * 8 {
		pick := rand.IntN(total)
		for _, k := range p.Keys {
			pick -= effWeight(k.Weight)
			if pick < 0 {
				sel, err := r.tryKey(provider, k, now, reasons, skip)
				if err != nil {
					return sel, err
				}
				if sel != nil {
					return sel, nil
				}
				break
			}
		}
	}
	return nil, fmt.Errorf("no usable key")
}

func (r *Router) fillFirstAcquire(provider string, now time.Time, reasons *[]string, skip map[string]bool) (*Selection, error) {
	p, ok := r.reg.Provider(provider)
	if !ok || len(p.Keys) == 0 {
		return nil, fmt.Errorf("no keys")
	}
	ordered := make([]model.Key, len(p.Keys))
	copy(ordered, p.Keys)

	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority > ordered[j].Priority
		}
		return effWeight(ordered[i].Weight) > effWeight(ordered[j].Weight)
	})
	for _, k := range ordered {
		sel, err := r.tryKey(provider, k, now, reasons, skip)
		if err != nil {
			return sel, err
		}
		if sel != nil {
			return sel, nil
		}
	}
	return nil, fmt.Errorf("no usable key")
}

func (r *Router) Release(sel *Selection, costMicros int64) {
	if sel == nil {
		return
	}
	if sel.kb != nil {
		sel.kb.Release(costMicros)

		sel.kb = nil
	}
	sel.pb, sel.pbSet = nil, false
}

func (r *Router) KeyUsage(provider, key string) (inflight int, costMicros int64) {
	r.bmu.RLock()
	kb := r.budgets[provider+"/"+key]
	r.bmu.RUnlock()
	if kb == nil {
		return 0, 0
	}
	s := kb.Snapshot(r.now())
	return s.Inflight, s.CostMicros
}

func (r *Router) InFlight(provider, key string) int {
	inflight, _ := r.KeyUsage(provider, key)
	return inflight
}

func (r *Router) RecordSuccess(sel *Selection) {
	r.health.Provider(sel.Provider).RecordSuccess(sel.accountedAt(r.now()))
	r.health.Key(sel.Provider, sel.Key).RecordSuccess(sel.accountedAt(r.now()))
}

func (r *Router) RecordFailure(sel *Selection) {
	at := sel.accountedAt(r.now())
	r.health.Provider(sel.Provider).RecordFailure(at)
	r.health.Key(sel.Provider, sel.Key).RecordFailure(at)
	if sel.SessionKey != "" {
		r.sticky.Forget(sel.SessionKey)
	}
}

func (s *Selection) accountedAt(now time.Time) time.Time {
	if s.admittedAt.IsZero() {
		return now
	}
	return s.admittedAt
}

type Sticky struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]stickyEntry

	opsSinceSweep int
}

const (
	stickyMaxEntries = 10000
	stickySweepEvery = 64
)

type stickyEntry struct {
	provider, key string
	target        model.Target
	at            time.Time
}

func NewSticky(ttl time.Duration, now func() time.Time) *Sticky {
	if now == nil {
		now = time.Now
	}
	return &Sticky{ttl: ttl, now: now, m: map[string]stickyEntry{}}
}

func (s *Sticky) sweep(now time.Time) {
	s.opsSinceSweep = 0
	if s.ttl <= 0 {

		return
	}
	for k, e := range s.m {
		if now.Sub(e.at) > s.ttl {
			delete(s.m, k)
		}
	}
}

func (s *Sticky) evictLocked() {
	if len(s.m) < stickyMaxEntries {
		return
	}
	var (
		oldestKey string
		oldestAt  time.Time
		found     bool
	)
	for k, e := range s.m {
		if !found || e.at.Before(oldestAt) {
			oldestKey, oldestAt, found = k, e.at, true
		}
	}
	if found {
		delete(s.m, oldestKey)
	}
}

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

func (s *Sticky) Put(sessionKey, provider, key string, target model.Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	s.opsSinceSweep++
	if s.opsSinceSweep >= stickySweepEvery {
		s.sweep(now)
	}
	s.m[sessionKey] = stickyEntry{provider: provider, key: key, target: target, at: now}
	s.evictLocked()
}

func (s *Sticky) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (s *Sticky) Forget(sessionKey string) {
	s.mu.Lock()
	delete(s.m, sessionKey)
	s.mu.Unlock()
}

func (s *Sticky) SetTTL(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = ttl
}

func (s *Sticky) ParentPrefix(sessionKey string, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := ""
	for k, e := range s.m {
		if len(k) >= len(sessionKey) || !strings.HasPrefix(sessionKey, k) {
			continue
		}
		if s.ttl > 0 && now.Sub(e.at) > s.ttl {

			continue
		}
		if len(k) > len(best) {
			best = k
		}
	}
	return best
}

type BudgetStatus struct {
	Provider     string `json:"provider"`
	Key          string `json:"key"`
	RPMRemaining int    `json:"rpm_remaining"`
	Inflight     int    `json:"inflight"`
	MaxInflight  int    `json:"max_inflight"`
	CostCents    int64  `json:"cost_cents"`
	CostLimit    int64  `json:"cost_limit_cents"`
}

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

			CostCents: money.MicroUSDToCents(s.CostMicros),
			CostLimit: money.MicroUSDToCents(s.CostLimitMicros),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Key < out[j].Key
	})
	return out
}

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
