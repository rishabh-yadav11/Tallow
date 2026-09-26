// Package routing implements model-alias resolution and two-layer selection
// (provider, then key), sticky session affinity, and ordered fallback.
package routing

import (
	"errors"
	"fmt"
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

	// M6: candidate lists are a pure function of the registry snapshot, but
	// were rebuilt on every Select (53% of all allocations, 37.6% of Select
	// CPU). They are memoized here and invalidated by registry version, so a
	// config hot-reload is the only thing that rebuilds them.
	cmu     sync.Mutex
	candVer uint64                 // registry version candMap was built from
	candMap map[string][]candidate // alias -> expanded candidates
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
		sticky:  NewSticky(stickyTTL, now),
		now:     now,
		rr:      map[string]int{},
		// Seeded as already-stale so the first Select populates the cache.
		candMap: map[string][]candidate{},
	}
}

// SetBudgets (re)installs budget trackers. Existing trackers are updated with
// the new limits in place (live windows and in-flight/cost state are preserved
// so hot-reload does not reset counters), and trackers for removed entities are
// dropped.
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
	// Drop budgets for keys no longer present.
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
	// Drop provider budgets no longer present.
	for id := range r.pBudget {
		if _, ok := providers[id]; !ok {
			delete(r.pBudget, id)
		}
	}
	// M10: prune round-robin offsets for providers that no longer exist. The rr
	// map is keyed by provider id and was only ever added to, so a gateway that
	// reloaded config repeatedly with changing provider names grew it without
	// bound, retaining an int for every provider id ever seen.
	r.rrmu.Lock()
	for id := range r.rr {
		if _, ok := providers[id]; !ok {
			delete(r.rr, id)
		}
	}
	r.rrmu.Unlock()

	// M6: a budget change can coincide with a registry change; drop the
	// candidate cache so the next Select re-expands against the current
	// snapshot instead of trusting a possibly stale list.
	r.cmu.Lock()
	r.candMap = map[string][]candidate{}
	r.candVer = 0
	r.cmu.Unlock()
}

// BudgetFor reports the key budget installed for a "provider/key" id, and
// whether one exists.
//
// It exists so the reload publish path can be OBSERVED rather than assumed. The
// audit C4 invariant ("every routable key has a budget entry") spans two
// independently-locked objects, and the window in which it could be violated is
// too narrow for a concurrent test to catch reliably. Exposing a read-only
// accessor is what makes the intermediate state assertable, so a regression in
// the publish ORDER fails a test instead of shipping. The handle is returned
// only for inspection; mutating it is the caller's responsibility and the
// router is the only intended user.
func (r *Router) BudgetFor(id string) (*budget.KeyBudget, bool) {
	r.bmu.RLock()
	defer r.bmu.RUnlock()
	kb, ok := r.budgets[id]
	return kb, ok
}

// errStaleSelection marks a selection whose provider/key vanished from the
// registry underneath it. C1: validity used to be *inferred* from an empty
// BaseURL, and the caller then dereferenced sel.kb on the assumption that a
// selection always has a budget. That assumption is false after a hot-reload
// drops the key, and the resulting nil dereference remotely killed the gateway.
// Callers now get an explicit signal instead of guessing.
var errStaleSelection = errors.New("selection target no longer in registry")

// errNoBudget marks a key that is routable but has no budget entry. C4: the
// old code did `if kb != nil { ... }`, which silently treated a missing budget
// as "unlimited" - fail-open. A key with no budget is now rejected, and this is
// a distinct error from staleness so the routing trail says which it was.
var errNoBudget = errors.New("no budget installed for key")

// ErrNoRoute is returned when no candidate could serve the alias, and classifies
// that failure. The class matters because a routing failure is not always an
// upstream failure.
//
// A limit is not an outage. When the only reason there is no route is that a
// budget or RPM window is spent, the upstream is perfectly healthy, so reporting
// it as a gateway/upstream error is a lie that costs the caller twice: a client
// reading 502 backs off as though the provider were broken, which does nothing
// because nothing is broken, and an operator reading upstream_error goes looking
// at provider health instead of at the limit they configured. It is also a
// small amplification risk, since retrying a request that is rate limited is
// exactly the wrong response to a rate limit.
//
// ErrNoRoute is returned when the routing failure was caused solely by exhausted
// limits, so the caller can answer 429 and tell the client to come back later.
var ErrNoRoute = errors.New("no route available")

// RouteError wraps ErrNoRoute with the alias and the per-candidate reason trail.
// It is a distinct type precisely so the caller can classify the failure
// without matching on the message text, which the reason trail makes unstable.
type RouteError struct {
	Alias   string
	Reasons []string
	// Limited is true when EVERY rejection was a budget or rate limit, i.e.
	// nothing was actually wrong with any provider. Any breaker-open, no-key or
	// removed candidate makes this false, because then at least one provider was
	// genuinely unusable and an upstream error is the honest answer.
	Limited bool
}

func (e *RouteError) Error() string {
	return fmt.Sprintf("no route for %q (reasons: %s)", e.Alias, strings.Join(e.Reasons, "; "))
}

func (e *RouteError) Unwrap() error { return ErrNoRoute }

// Selection is a routed, budget-reserved destination.
type Selection struct {
	Alias      string
	SessionKey string
	Provider   string
	Key        string
	Model      string // upstream model string.
	Target     model.Target
	Reason     string // routing path + why (for observability).

	// admittedAt is when Select handed this request to the provider, not when
	// it reported back. It is what makes late accounting safe: a request
	// admitted before a burst of failures completes successfully afterwards,
	// and its success is evidence about a request that started before the
	// breaker opened. Accounting at completion time instead would stamp that
	// success as the newest evidence available and let it close a breaker that
	// is open on current failures, undoing failover.
	admittedAt time.Time

	BaseURL string
	Secret  string
	Timeout time.Duration

	kb *budget.KeyBudget
	// pb records a provider-level RPM charge this selection consumed but that
	// Select then ABANDONED, so it must be handed back.
	//
	// This is deliberately not how a completed request is accounted. Provider
	// rpm is a request RATE limit, not a concurrency limit: a request that ran
	// to completion has still consumed one unit of the provider's per-minute
	// quota, and refunding it on release would make the cap unenforceable.
	// TestE2EStickyProviderRPMEnforced exists to pin exactly that. The only
	// charge refunded is one taken for a candidate Select rejected before the
	// request was ever sent there, so audit M5's "consumed but never released"
	// is real but scoped to the abandon path.
	pb    *budget.ProviderBudget
	pbSet bool
}

// claimProviderRPM records an abandoned provider-level RPM charge for refund,
// and reports whether this call was the one that claimed it.
//
// Several candidate-loop exits can each decide to abandon a selection, so
// ownership must be settled in exactly one place. Otherwise a refund would be
// applied for a charge never taken, manufacturing headroom past the RPM cap.
func (sel *Selection) claimProviderRPM(pb *budget.ProviderBudget) bool {
	if sel == nil || sel.pbSet {
		return false
	}
	sel.pb, sel.pbSet = pb, true
	return true
}

// Candidates are the ordered provider/model pairs for an alias, including
// provider-level fallback chains expanded inline.
type candidate struct {
	provider string
	model    string
	target   model.Target
}

// maxFallbackDepth bounds transitive fallback expansion (M4). Cycles are
// handled by `seen`; this is a second belt-and-braces stop so a pathological
// graph cannot produce an unbounded candidate list even if `seen` is bypassed.
const maxFallbackDepth = 8

// candidates expands an alias's targets plus their transitive provider fallback
// chains, in preference order.
//
// The result is a pure function of the registry snapshot, yet it accounted for
// 53% of all allocations and 37.6% of Select CPU (M6) because it was rebuilt
// on every request. Callers reach it through r.cachedCandidates, which memoizes
// per registry version.
func (r *Router) candidates(alias *model.Alias) []candidate {
	var out []candidate
	seen := map[string]bool{}
	// queue holds providers whose fallback chain still needs expanding. M4: the
	// original walked exactly one level, so a->b->c never reached c. A worklist
	// walks the graph to a fixed point, bounded by maxFallbackDepth.
	type pending struct {
		provider string
		model    string
		target   model.Target
		depth    int
	}
	var queue []pending
	for _, t := range alias.Targets {
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
		// A fallback provider inherits the *originating* upstream model and
		// target, so the request stays semantically the same request.
		for _, f := range p.Fallback {
			if !seen[f] {
				out = append(out, candidate{provider: f, model: cur.model, target: cur.target})
				seen[f] = true
				queue = append(queue, pending{provider: f, model: cur.model, target: cur.target, depth: cur.depth + 1})
			}
		}
	}
	return out
}

// cachedCandidates returns the memoized candidate list for an alias, rebuilding
// it only when the registry snapshot has changed (M6).
//
// The returned slice is shared and must be treated as read-only by callers; it
// is never mutated after being cached.
func (r *Router) cachedCandidates(aliasName string, a *model.Alias) []candidate {
	ver := r.reg.Version()

	r.cmu.Lock()
	defer r.cmu.Unlock()
	if r.candVer == ver {
		if c, ok := r.candMap[aliasName]; ok {
			return c
		}
	}
	// Either the registry moved on or this alias is not cached yet.
	built := r.candidates(a)
	if r.candVer != ver {
		// A new snapshot invalidates every alias, not just this one.
		r.candMap = make(map[string][]candidate, len(r.candMap)+1)
		r.candVer = ver
	}
	r.candMap[aliasName] = built
	return built
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
					// The pin is stale: the provider/key (or its budget) was
					// removed on hot-reload. Forget it and route normally.
					// r.releaseSelection is nil-safe, so a budget-less
					// selection cannot panic here (C1).
					r.releaseSelection(sel)
					r.sticky.Forget(sessionKey)
					reasons = append(reasons, "sticky:"+p+"/"+k+":removed")
				default:
					// Budget or provider RPM exhausted on the pinned target:
					// fall through to normal routing rather than stranding the
					// session, but forget the pin so the next attempt re-picks.
					r.releaseSelection(sel)
					r.sticky.Forget(sessionKey)
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
			// C1: the candidate's provider disappeared between building the
			// candidate list and acquiring. Release via the nil-safe helper.
			r.releaseSelection(sel)
			if errors.Is(err, errStaleSelection) {
				reasons = append(reasons, "provider:"+c.provider+":removed")
			} else {
				reasons = append(reasons, provID+":no_key")
			}
			continue
		}
		// C1: same nil-deref hazard on the removed-provider path.
		if sel.BaseURL == "" {
			// Provider was removed from the registry on hot-reload; skip this
			// candidate rather than forwarding to an empty upstream URL.
			r.releaseSelection(sel)
			reasons = append(reasons, "provider:"+c.provider+":removed")
			continue
		}
		if pb != nil && !pb.Allow(now) {
			// Allow returned false, so the provider window was NOT charged and
			// there is nothing to refund. Releasing the key slot lets the loop
			// try the next candidate. Note this is the second RPM check for
			// this candidate: the first was the cheap Remaining() peek above,
			// and this one closes the race between the peek and here.
			r.releaseSelection(sel)
			reasons = append(reasons, provID+":provider_rpm_race")
			continue
		}
		// M5: this Allow is the ONLY provider-RPM charge on the non-sticky
		// path (acquireKey does not charge it), so the selection owns exactly
		// one slot. Claiming it means an abandon in this iteration refunds it
		// rather than leaking it; a request that runs keeps it, because
		// provider rpm is a rate limit, not a concurrency limit.
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

	return nil, newRouteError(alias, reasons)
}

// limitReasons is the closed set of routing reasons that mean "a limit was
// reached", as opposed to "something is wrong". It is derived from the reason
// strings the selection loop and KeyBudget.Acquire actually emit, which is why
// it is written out rather than guessed: a suffix invented from intuition will
// not match, and a mismatch silently classifies a rate limit as an upstream
// fault, which is the exact bug this type exists to prevent.
//
// The two levels use different vocabularies for the same condition. The provider
// loop writes "provider:p1:provider_rpm" while the key loop writes
// "key:k1:key_rpm", because one is an aggregate and the other a per-credential
// window. Both are limits, so both are listed.
var limitReasons = map[string]bool{
	// Provider level.
	"provider_rpm":      true,
	"provider_rpm_race": true,
	// Key level, from KeyBudget.Acquire.
	"key_rpm":            true,
	"key_quota":          true,
	"key_max_concurrent": true,
	"key_cost_limit":     true,
	// Sticky: the pinned provider/key is out of budget, which is a limit, not a
	// fault. The sticky entry is then forgotten and the normal candidate scan
	// runs, so this can appear alone in the trail.
	"exhausted": true,
}

// newRouteError classifies a routing failure from the reason trail.
//
// The classification is deliberately conservative. Every reason must be a
// limit for the failure to count as limited, and anything unrecognised counts
// against it: an unknown reason is not evidence that the providers are healthy,
// and answering 429 when the real cause is a dead provider would send the caller
// off to wait when the thing it needed was to fail over.
func newRouteError(alias string, reasons []string) *RouteError {
	limited := len(reasons) > 0
	for _, r := range reasons {
		// Reasons are "<scope>:<id>:<cause>"; the cause is the last segment.
		cause := r
		if i := strings.LastIndex(r, ":"); i >= 0 {
			cause = r[i+1:]
		}
		if !limitReasons[cause] {
			limited = false
		}
	}
	return &RouteError{Alias: alias, Reasons: reasons, Limited: limited}
}

// finalReason renders the routing trail: skipped candidates, then the choice.
func finalReason(trail []string, final string) string {
	if len(trail) == 0 {
		return final
	}
	return strings.Join(trail, "; ") + " -> " + final
}

// releaseSelection returns a reservation that Select will not use, without
// assuming the selection has a budget. C1: the old code called sel.kb.Release
// directly, so a budget-less selection (legal after a hot-reload drops the
// key's budget) dereferenced nil and took the process down.
//
// This is the ABANDON path: Select gives the slot back because it will not use
// this candidate, so any provider RPM it charged is refunded here (M5). A
// selection that is actually used goes through Release instead.
func (r *Router) releaseSelection(sel *Selection) {
	if sel == nil {
		return
	}
	if sel.kb != nil {
		sel.kb.Release(0)
	}
	if sel.pb != nil && sel.pbSet {
		// M5: the refund is stamped with the ADMISSION time, not the release
		// time, and that is load-bearing. Select charged this provider's RPM at
		// admittedAt, so the refund has to belong to the same window as the
		// charge it offsets. Stamping it with r.now() is wrong whenever the
		// window rolls over between the two: FixedWindow.Refund then compares a
		// release-time timestamp against the CURRENT window's start, decides the
		// refund is in-window, and decrements a count that this selection's
		// charge never contributed to. The provider ends up with one request per
		// abandon more headroom than the operator configured, so a request that
		// outlives its window silently bypasses the provider RPM cap. The
		// admittedAt stamp makes the pair exact: the same timestamp that was
		// charged is the one refunded.
		sel.pb.Release(sel.accountedAt(r.now()))
	}
	sel.pb, sel.pbSet, sel.kb = nil, false, nil
}

// acquire reserves a specific provider/key (used by sticky path). It enforces
// both the key budget and the provider-level RPM cap, mirroring the non-sticky
// acquireKey path so a pinned session cannot exceed the aggregate provider cap.
func (r *Router) acquire(provider, key, model string, target model.Target, now time.Time) (*Selection, error) {
	id := provider + "/" + key
	r.bmu.RLock()
	kb := r.budgets[id]
	pb := r.pBudget[provider]
	r.bmu.RUnlock()

	// C4: fail CLOSED. A key with no budget entry is NOT unlimited; treating a
	// missing budget as "no limit" is fail-open, so a hot-reload window that
	// dropped a budget silently removed the key's spend cap. Reject instead.
	if kb == nil {
		return nil, fmt.Errorf("%w: %s", errNoBudget, id)
	}
	if ok, _ := kb.Acquire(now); !ok {
		return nil, fmt.Errorf("budget exhausted")
	}
	if pb != nil && !pb.Allow(now) {
		// Provider RPM exhausted; do not burn the key's reserved slot.
		kb.Release(0)
		return nil, fmt.Errorf("provider rpm exhausted")
	}
	sel := &Selection{Provider: provider, Key: key, Model: model, Target: target, kb: kb, admittedAt: now}
	// The provider RPM charge taken above belongs to this selection. Claiming
	// it here means a later abandon (releaseSelection) hands the slot back,
	// while a request that actually runs keeps the charge for the rest of the
	// window - provider rpm is a rate limit, not a concurrency limit.
	sel.claimProviderRPM(pb)
	fillDest(r.reg, sel)
	if sel.BaseURL == "" {
		// Provider vanished from the registry between the budget read and now.
		// C1: return the reserved slot through the nil-safe path, which also
		// refunds the provider RPM just charged - the request never went here.
		r.releaseSelection(sel)
		return sel, errStaleSelection
	}
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
		// C4: fail closed. Every routable key must have a budget; a key without
		// one is rejected instead of being served with no limits at all.
		if kb == nil {
			*reasons = append(*reasons, "key:"+k.ID+":no_budget")
			continue
		}
		if ok, reason := kb.Acquire(now); !ok {
			*reasons = append(*reasons, "key:"+k.ID+":"+reason)
			continue
		}
		sel := &Selection{Provider: provider, Key: k.ID, kb: kb, admittedAt: now}
		fillDest(r.reg, sel)
		if sel.BaseURL == "" {
			// Provider removed from the registry between the key scan and now.
			// C1: return the reserved slot through the nil-safe path.
			kb.Release(0)
			sel.kb = nil
			return sel, errStaleSelection
		}
		return sel, nil
	}
	return nil, fmt.Errorf("no usable key")
}

// Release returns the reserved in-flight slot and accumulates cost.
//
// It deliberately does NOT refund provider-level RPM. Provider rpm is a rate
// limit over requests, so a completed request has consumed its slot for the
// rest of the window; refunding here would let a caller loop through a
// provider's cap indefinitely. Only a candidate that Select abandoned is
// refunded, in releaseSelection.
func (r *Router) Release(sel *Selection, costMicros int64) {
	if sel == nil {
		return
	}
	if sel.kb != nil {
		sel.kb.Release(costMicros)
		// Clear the handle so a repeated Release cannot double-decrement the
		// in-flight counter or double-count cost.
		sel.kb = nil
	}
	sel.pb, sel.pbSet = nil, false
}

// KeyUsage reports the named key's live concurrency slots and banked cost.
//
// Every slot taken by Select must come back through exactly one Release or
// releaseSelection. A slot that never returns is invisible in the request logs
// but caps the key permanently. A cost that is banked twice reports spend that
// never happened, which is the failure C3 was about. Both are the kind of bug
// that is invisible in production and trivial to assert here, so this accessor
// exists to make them assertable rather than merely suspected.
//
// The provider/key id is the same "provider/key" form used for budget limits.
// The cost is in micro-USD, matching the unit costs are accumulated in.
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

// InFlight reports how many concurrency slots the named key currently holds.
//
// It is the slot half of KeyUsage, kept for callers that only need the slot.
func (r *Router) InFlight(provider, key string) int {
	inflight, _ := r.KeyUsage(provider, key)
	return inflight
}

// RecordSuccess closes the relevant breakers and re-affirms sticky.
//
// The account is stamped with when Select admitted the request, not with the
// current time. A request admitted before a burst of failures and completed
// after it is reporting on a request that started while the breaker was still
// closed; stamping it now would present stale evidence as the newest evidence
// available and close a breaker that is open on current failures.
func (r *Router) RecordSuccess(sel *Selection) {
	r.health.Provider(sel.Provider).RecordSuccess(sel.accountedAt(r.now()))
	r.health.Key(sel.Provider, sel.Key).RecordSuccess(sel.accountedAt(r.now()))
}

// RecordFailure accounts a failure toward breaker opening, stamped with the
// admission time for the same reason as RecordSuccess.
func (r *Router) RecordFailure(sel *Selection) {
	at := sel.accountedAt(r.now())
	r.health.Provider(sel.Provider).RecordFailure(at)
	r.health.Key(sel.Provider, sel.Key).RecordFailure(at)
	if sel.SessionKey != "" {
		r.sticky.Forget(sel.SessionKey)
	}
}

// accountedAt returns the timestamp this selection's breaker accounts carry.
//
// It is the admission stamp Select recorded. A Selection built directly rather
// than by Select carries no stamp, and falls back to the current time, so a
// hand-built selection behaves as it did before and is never treated as stale.
func (s *Selection) accountedAt(now time.Time) time.Time {
	if s.admittedAt.IsZero() {
		return now
	}
	return s.admittedAt
}

// ---------- Sticky ----------

// Sticky maps session keys to a pinned provider/key/target with idle expiry.
type Sticky struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]stickyEntry
	// opsSinceSweep counts Put calls since the last idle sweep. A count is
	// used rather than a timestamp so the sweep cadence does not depend on
	// wall-clock jumps (M12).
	opsSinceSweep int
}

// Sticky memory bounds. Session keys arrive from the client via X-Session-Id,
// so they are attacker-controlled: 100k distinct ids cost ~43 MB and nothing
// ever removed them, because ttl == 0 means "never expire" and that was the
// value a config omission produced (audit H7). The TTL is therefore only ONE of
// two bounds - maxEntries is the other, and it applies even when the TTL is
// disabled. stickySweepEvery amortizes expiry checks so idle entries are
// reclaimed promptly without scanning the map on every request.
const (
	stickyMaxEntries = 10000
	stickySweepEvery = 64
)

type stickyEntry struct {
	provider, key string
	target        model.Target
	at            time.Time
}

// NewSticky builds a sticky store. ttl <= 0 disables idle expiry (always
// sticky until evicted by the size cap or an explicit Forget).
//
// now is injected rather than read from time.Now so Put and Get agree on a
// single clock source (M12). Previously Put stamped time.Now() while Get
// compared against the router's injected clock, so an injected clock in tests
// (and any NTP step in production) could pin an entry that was already
// expired, or refuse to expire one that was not.
func NewSticky(ttl time.Duration, now func() time.Time) *Sticky {
	if now == nil {
		now = time.Now
	}
	return &Sticky{ttl: ttl, now: now, m: map[string]stickyEntry{}}
}

// sweep drops idle-expired entries. It must be called with s.mu held.
// Entries are removed by the same rule as Get, so a sweep can never remove an
// entry that Get would consider live.
func (s *Sticky) sweep(now time.Time) {
	s.opsSinceSweep = 0
	if s.ttl <= 0 {
		// Idle expiry is disabled, so a sweep has nothing to reclaim; the size
		// cap is what bounds memory in this configuration.
		return
	}
	for k, e := range s.m {
		if now.Sub(e.at) > s.ttl {
			delete(s.m, k)
		}
	}
}

// evictLocked enforces the size cap by dropping the entry with the oldest
// timestamp. It must be called with s.mu held.
//
// Go randomizes map iteration order, so finding the true oldest entry requires
// a full scan. That is affordable precisely because this only runs on the
// insert that reaches the cap, i.e. once per stickyMaxEntries pins.
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
	// Refresh the idle timer. NOTE (H7): this makes the TTL a *sliding* idle
	// window, so a session polled more often than the TTL never expires. That
	// is intentional for cache reuse, but it is why the TTL cannot be the only
	// memory bound - the size cap above is what stops unbounded growth.
	e.at = now
	s.m[sessionKey] = e
	return e.provider, e.key, e.target, true
}

// Put records a pin.
func (s *Sticky) Put(sessionKey, provider, key string, target model.Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// Amortized idle sweep, then the size-cap eviction for the incoming entry.
	s.opsSinceSweep++
	if s.opsSinceSweep >= stickySweepEvery {
		s.sweep(now)
	}
	s.m[sessionKey] = stickyEntry{provider: provider, key: key, target: target, at: now}
	s.evictLocked()
}

// Len reports the number of live pins; used by tests and the admin view.
func (s *Sticky) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Forget drops a pin.
func (s *Sticky) Forget(sessionKey string) {
	s.mu.Lock()
	delete(s.m, sessionKey)
	s.mu.Unlock()
}

// BudgetStatus is a live per-key budget view for the admin API/TUI. Costs are
// reported to operators in cents, converted from the internal micro-USD unit
// that the budget actually accumulates.
type BudgetStatus struct {
	Provider     string `json:"provider"`
	Key          string `json:"key"`
	RPMRemaining int    `json:"rpm_remaining"`
	Inflight     int    `json:"inflight"`
	MaxInflight  int    `json:"max_inflight"`
	CostCents    int64  `json:"cost_cents"`
	CostLimit    int64  `json:"cost_limit_cents"`
}

// Budgets reports a snapshot of every configured key's budget state.
//
// The rows are sorted by provider then key. Iteration order over r.budgets is
// Go's map order, which is randomised per process and deliberately varies
// between runs, so an unsorted result would hand every caller a different
// ordering of the same data. That matters beyond tidiness: the admin API
// serialises this straight to JSON, a dashboard renders it as a table, and
// anything that compares two snapshots positionally would see a spurious
// difference on every poll. An operator watching a limit move would see the
// rows swap places and could conclude the wrong key was throttled.
//
// Sorting costs a comparison per row against a list that is bounded by the
// number of configured keys, which is small and fixed for the life of the
// process.
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
			// Rounding away from zero keeps sub-cent spend visible instead of
			// displaying as $0.00, which was how the cents truncation hid all cost.
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
