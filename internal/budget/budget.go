package budget

import (
	"sync"
	"time"
)

type FixedWindow struct {
	Limit  int
	Window time.Duration
	mu     sync.Mutex
	count  int

	startAt time.Time
	resetAt time.Time
}

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

func (w *FixedWindow) Peek(now time.Time) bool {
	if w.Limit <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.After(w.resetAt) {
		return true
	}
	return w.count < w.Limit
}

func (w *FixedWindow) Refund(now time.Time) {
	if w.Limit <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if now.Before(w.startAt) {
		return
	}
	if w.count > 0 {
		w.count--
	}
}

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

func (w *FixedWindow) ResetsAt(now time.Time) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.After(w.resetAt) {
		return now.Add(w.Window)
	}
	return w.resetAt
}

type KeyLimits struct {
	RPM             int
	MaxRequests     int
	Window          time.Duration
	MaxConcurrent   int
	CostLimitCents  int64
	CostLimitMicros int64
}

type KeyBudget struct {
	mu          sync.Mutex
	rpm         FixedWindow
	req         FixedWindow
	inflight    int
	costMicros  int64
	costLimit   int64
	maxInflight int
}

func NewKeyBudget(l KeyLimits) *KeyBudget {
	return &KeyBudget{
		rpm:         FixedWindow{Limit: l.RPM, Window: time.Minute},
		req:         FixedWindow{Limit: l.MaxRequests, Window: l.Window},
		maxInflight: l.MaxConcurrent,
		costLimit:   l.CostLimitMicros,
	}
}

func (b *KeyBudget) SetLimits(l KeyLimits) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rpm.Limit = l.RPM
	b.req.Limit = l.MaxRequests
	b.req.Window = l.Window
	b.maxInflight = l.MaxConcurrent
	b.costLimit = l.CostLimitMicros
}

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

	b.rpm.Allow(now)
	if b.req.Window > 0 && b.req.Limit > 0 {
		b.req.Allow(now)
	}
	b.inflight++
	return true, ""
}

func (b *KeyBudget) Release(costMicros int64) {
	b.mu.Lock()
	if b.inflight > 0 {
		b.inflight--
	}
	b.costMicros += costMicros
	b.mu.Unlock()
}

func (b *KeyBudget) AccumulateCost(costMicros int64) {
	b.mu.Lock()
	b.costMicros += costMicros
	b.mu.Unlock()
}

type Snapshot struct {
	RPMRemaining   int
	RPMResetsAt    time.Time
	QuotaRemaining int
	QuotaResetsAt  time.Time
	Inflight       int
	MaxInflight    int

	CostMicros      int64
	CostLimitMicros int64
}

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

type ProviderBudget struct {
	rpm FixedWindow
}

func NewProviderBudget(rpm int) *ProviderBudget {
	return &ProviderBudget{rpm: FixedWindow{Limit: rpm, Window: time.Minute}}
}

func (p *ProviderBudget) RPMWindow() time.Duration {
	return p.rpm.Window
}

func (p *ProviderBudget) RPMResetsAt(now time.Time) time.Time {
	return p.rpm.ResetsAt(now)
}

func (p *ProviderBudget) SetRPM(rpm int) {
	p.rpm.mu.Lock()
	p.rpm.Limit = rpm
	p.rpm.mu.Unlock()
}

func (p *ProviderBudget) Allow(now time.Time) bool {
	return p.rpm.Allow(now)
}

func (p *ProviderBudget) Release(now time.Time) {
	p.rpm.Refund(now)
}

func (p *ProviderBudget) Remaining(now time.Time) int {
	return p.rpm.Remaining(now)
}
