// Package budget implements hard, fixed-window rate and quota limits. Fixed
// window was chosen explicitly over sliding window / token bucket for its
// minimal compute cost: a counter plus a reset timestamp per window.
package budget

import (
	"sync"
	"time"
)

// FixedWindow is a counter that resets every Window. Thread-safe.
type FixedWindow struct {
	Limit  int
	Window time.Duration
	mu     sync.Mutex
	count  int
	// startAt is when the current window began. A window that has not rolled
	// yet has a zero startAt, which Refund treats as "no current window", so the
	// first Allow is what establishes it.
	startAt time.Time
	resetAt time.Time
}

// Allow consumes one unit if under the limit and the window is current.
// Zero/negative Limit means unlimited. now must be monotonic-ish (time.Now).
func (w *FixedWindow) Allow(now time.Time) bool {
	if w.Limit <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.After(w.resetAt) {
		w.count = 0
		w.startAt = now
		w.resetAt = now.Add(w.Window)
	}
	if w.count >= w.Limit {
		return false
	}
	w.count++
	return true
}

// Peek reports whether a unit could be admitted without consuming it. It is a
// pure read: it must not mutate state, so that a following Allow stays
// consistent with the peek.
func (w *FixedWindow) Peek(now time.Time) bool {
	if w.Limit <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.After(w.resetAt) {
		return true // window would reset; count is effectively zero.
	}
	return w.count < w.Limit
}

// Refund returns one previously consumed unit to the window. It is the exact
// inverse of Allow for the same `now`, and is used when a request is admitted
// but then abandoned (e.g. the caller discovers the target was gone and routes
// elsewhere), so a failed attempt does not permanently consume capacity.
//
// count is CONSUMED units, so a refund DECREMENTS it; headroom is
// limit - count. Three safety properties matter, because a refund not perfectly
// paired with its Allow would let a provider exceed its configured RPM:
//   - A refund whose timestamp falls outside the window the charge landed in is
//     a no-op. The Allow it would offset happened in a different window, whose
//     count has already been reset, so decrementing would manufacture headroom
//     that was never paid for. Comparing against the window's START, not only
//     its reset, is what makes this hold when a refund timestamp lands inside
//     the current window's span but before the charge it is offsetting did.
//     Comparing only against resetAt was not enough: that check let a stale
//     refund through and let the provider exceed its configured RPM.
//   - The count is clamped at zero so an unmatched refund cannot walk the
//     count negative, which would manufacture headroom on every extra refund.
//     The clamp also covers the case where no window has been established yet,
//     since a window that has never been charged has a count of zero.
func (w *FixedWindow) Refund(now time.Time) {
	if w.Limit <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// The zero-startAt case needs no explicit guard: if no Allow has
	// established a window then count is still 0, and the clamp below already
	// refuses to decrement it. The check is therefore redundant rather than
	// load-bearing, and a mutation test that deletes it confirms the clamp
	// covers the same case.
	if now.Before(w.startAt) {
		return // the charge predates the current window; it is long gone.
	}
	if w.count > 0 {
		w.count--
	}
}

// Remaining reports headroom in the current window; -1 = unlimited.
func (w *FixedWindow) Remaining(now time.Time) int {
	if w.Limit <= 0 {
		return -1
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.After(w.resetAt) {
		return w.Limit
	}
	r := w.Limit - w.count
	if r < 0 {
		r = 0
	}
	return r
}

// ResetsAt reports the timestamp when the current window rolls over.
func (w *FixedWindow) ResetsAt(now time.Time) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.After(w.resetAt) {
		return now.Add(w.Window)
	}
	return w.resetAt
}

// KeyLimits is the configured hard-limit set for a single key.
//
// CostLimitCents is the operator-facing cap in cents; CostLimitMicros is the
// same cap in the internal micro-USD unit and is what the budget compares
// against. Microunits matter because a single LLM call normally costs a
// fraction of a cent, so a cents-unit accumulator truncated real spend to zero
// and cost caps could never fire.
type KeyLimits struct {
	RPM             int
	MaxRequests     int
	Window          time.Duration
	MaxConcurrent   int
	CostLimitCents  int64
	CostLimitMicros int64
}

// KeyBudget tracks a key's RPM, request window, in-flight concurrency, and
// cumulative cost. All zero limits mean unlimited.
type KeyBudget struct {
	mu          sync.Mutex
	rpm         FixedWindow
	req         FixedWindow
	inflight    int
	costMicros  int64
	costLimit   int64 // micro-USD
	maxInflight int
}

// NewKeyBudget builds a KeyBudget with internal fixed windows.
func NewKeyBudget(l KeyLimits) *KeyBudget {
	return &KeyBudget{
		rpm:         FixedWindow{Limit: l.RPM, Window: time.Minute},
		req:         FixedWindow{Limit: l.MaxRequests, Window: l.Window},
		maxInflight: l.MaxConcurrent,
		costLimit:   l.CostLimitMicros,
	}
}

// SetLimits applies updated limits without resetting live counters (used by
// config hot-reload so existing windows and in-flight/cost state survive).
func (b *KeyBudget) SetLimits(l KeyLimits) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rpm.Limit = l.RPM
	b.req.Limit = l.MaxRequests
	b.req.Window = l.Window
	b.maxInflight = l.MaxConcurrent
	b.costLimit = l.CostLimitMicros
}

// Acquire reserves a request slot. Returns failure with a reason when any hard
// limit is exhausted. All limits are peeked (non-consumingly) before any is
// consumed, so a request blocked by one limit never burns a slot on another.
func (b *KeyBudget) Acquire(now time.Time) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxInflight > 0 && b.inflight >= b.maxInflight {
		return false, "key_max_concurrent"
	}
	if b.costLimit > 0 && b.costMicros >= b.costLimit {
		return false, "key_cost_limit"
	}
	if !b.rpm.Peek(now) {
		return false, "key_rpm"
	}
	if b.req.Window > 0 && b.req.Limit > 0 && !b.req.Peek(now) {
		return false, "key_quota"
	}
	// All limits clear: consume.
	b.rpm.Allow(now)
	if b.req.Window > 0 && b.req.Limit > 0 {
		b.req.Allow(now)
	}
	b.inflight++
	return true, ""
}

// Release returns one in-flight slot and accumulates cost (in micro-USD).
func (b *KeyBudget) Release(costMicros int64) {
	b.mu.Lock()
	if b.inflight > 0 {
		b.inflight--
	}
	b.costMicros += costMicros
	b.mu.Unlock()
}

// AccumulateCost adds cost without touching in-flight (for already-released or
// non-slot accounting paths). Kept separate from Release for clarity.
func (b *KeyBudget) AccumulateCost(costMicros int64) {
	b.mu.Lock()
	b.costMicros += costMicros
	b.mu.Unlock()
}

// Snapshot is a live view for observability.
type Snapshot struct {
	RPMRemaining   int
	RPMResetsAt    time.Time
	QuotaRemaining int
	QuotaResetsAt  time.Time
	Inflight       int
	MaxInflight    int
	// CostMicros and CostLimitMicros are both in micro-USD.
	CostMicros      int64
	CostLimitMicros int64
}

// Snapshot returns a consistent live status.
func (b *KeyBudget) Snapshot(now time.Time) Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{
		RPMRemaining:    b.rpm.Remaining(now),
		RPMResetsAt:     b.rpm.ResetsAt(now),
		QuotaRemaining:  b.req.Remaining(now),
		QuotaResetsAt:   b.req.ResetsAt(now),
		Inflight:        b.inflight,
		MaxInflight:     b.maxInflight,
		CostMicros:      b.costMicros,
		CostLimitMicros: b.costLimit,
	}
}

// ProviderBudget is the provider-level fixed-window RPM cap (aggregate across
// its keys).
type ProviderBudget struct {
	rpm FixedWindow
}

// NewProviderBudget builds a provider-level RPM limiter.
func NewProviderBudget(rpm int) *ProviderBudget {
	return &ProviderBudget{rpm: FixedWindow{Limit: rpm, Window: time.Minute}}
}

// RPMWindow is the width of the provider RPM window. The proxy needs it to turn
// the router's reset time into the seconds a Retry-After header carries, and
// hardcoding a minute at the call site would silently go wrong if the window
// were ever configured to a different width.
func (p *ProviderBudget) RPMWindow() time.Duration {
	return p.rpm.Window
}

// RPMResetsAt reports when the provider's current RPM window rolls over, so a
// caller can tell the client when to come back rather than leaving it to guess.
func (p *ProviderBudget) RPMResetsAt(now time.Time) time.Time {
	return p.rpm.ResetsAt(now)
}

// SetRPM updates the provider-level RPM cap without resetting the live window.
func (p *ProviderBudget) SetRPM(rpm int) {
	p.rpm.mu.Lock()
	p.rpm.Limit = rpm
	p.rpm.mu.Unlock()
}

// Allow consumes a provider-level RPM slot.
func (p *ProviderBudget) Allow(now time.Time) bool {
	return p.rpm.Allow(now)
}

// Release returns a provider-level RPM slot consumed by Allow. Without this,
// a provider's aggregate cap was consumed permanently by every request ever
// routed to it, so the cap ratcheted downward and eventually starved the
// provider regardless of actual traffic (audit M5).
func (p *ProviderBudget) Release(now time.Time) {
	p.rpm.Refund(now)
}

// Remaining reports provider-level RPM headroom; -1 = unlimited.
func (p *ProviderBudget) Remaining(now time.Time) int {
	return p.rpm.Remaining(now)
}
