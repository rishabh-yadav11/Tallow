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
	Limit   int
	Window  time.Duration
	mu      sync.Mutex
	count   int
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
type KeyLimits struct {
	RPM            int
	MaxRequests    int
	Window         time.Duration
	MaxConcurrent  int
	CostLimitCents int64
}

// KeyBudget tracks a key's RPM, request window, in-flight concurrency, and
// cumulative cost. All zero limits mean unlimited.
type KeyBudget struct {
	mu          sync.Mutex
	rpm         FixedWindow
	req         FixedWindow
	inflight    int
	costCents   int64
	costLimit   int64
	maxInflight int
}

// NewKeyBudget builds a KeyBudget with internal fixed windows.
func NewKeyBudget(l KeyLimits) *KeyBudget {
	return &KeyBudget{
		rpm:         FixedWindow{Limit: l.RPM, Window: time.Minute},
		req:         FixedWindow{Limit: l.MaxRequests, Window: l.Window},
		maxInflight: l.MaxConcurrent,
		costLimit:   l.CostLimitCents,
	}
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
	if b.costLimit > 0 && b.costCents >= b.costLimit {
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

// Release returns one in-flight slot and accumulates cost.
func (b *KeyBudget) Release(costCents int64) {
	b.mu.Lock()
	if b.inflight > 0 {
		b.inflight--
	}
	b.costCents += costCents
	b.mu.Unlock()
}

// AccumulateCost adds cost without touching in-flight (for already-released or
// non-slot accounting paths). Kept separate from Release for clarity.
func (b *KeyBudget) AccumulateCost(costCents int64) {
	b.mu.Lock()
	b.costCents += costCents
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
	CostCents      int64
	CostLimitCents int64
}

// Snapshot returns a consistent live status.
func (b *KeyBudget) Snapshot(now time.Time) Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{
		RPMRemaining:   b.rpm.Remaining(now),
		RPMResetsAt:    b.rpm.ResetsAt(now),
		QuotaRemaining: b.req.Remaining(now),
		QuotaResetsAt:  b.req.ResetsAt(now),
		Inflight:       b.inflight,
		MaxInflight:    b.maxInflight,
		CostCents:      b.costCents,
		CostLimitCents: b.costLimit,
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

// Allow consumes a provider-level RPM slot.
func (p *ProviderBudget) Allow(now time.Time) bool {
	return p.rpm.Allow(now)
}

// Remaining reports provider-level RPM headroom; -1 = unlimited.
func (p *ProviderBudget) Remaining(now time.Time) int {
	return p.rpm.Remaining(now)
}
