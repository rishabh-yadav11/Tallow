package budget

import (
	"sync"
	"testing"
	"time"
)

// ProviderBudget is the aggregate RPM cap across a provider's keys. Release
// exists because audit finding M5: without it, every request ever routed to a
// provider permanently consumed a slot, so the cap ratcheted downward and
// eventually starved the provider no matter how little traffic it received.
//
// That failure mode is invisible in a single-shot test, so these pin both the
// steady-state behaviour and the exact regression.

// TestProviderBudgetReleaseReturnsTheSlot is the M5 regression. Without
// Release, a provider at RPM 10 accepts 10 requests and then refuses the
// eleventh for the rest of the window, even though the first ten all finished.
func TestProviderBudgetReleaseReturnsTheSlot(t *testing.T) {
	p := NewProviderBudget(10)
	now := time.Now()

	for i := 0; i < 10; i++ {
		if !p.Allow(now) {
			t.Fatalf("request %d refused below the cap", i)
		}
		p.Release(now)
	}
	if got := p.Remaining(now); got != 10 {
		t.Errorf("Remaining = %d after 10 allowed-and-released, want 10: "+
			"the provider cap is still ratcheting down", got)
	}
	// And the cap must still bite when slots are genuinely held.
	for i := 0; i < 10; i++ {
		if !p.Allow(now) {
			t.Fatalf("held request %d refused", i)
		}
	}
	if p.Allow(now) {
		t.Error("the 11th held request was admitted at a cap of 10")
	}
	if got := p.Remaining(now); got != 0 {
		t.Errorf("Remaining = %d with 10 held, want 0", got)
	}
}

// TestProviderBudgetReleaseOnARolledWindowIsANoOp guards the rollover case.
// A slot consumed in the previous window is already gone; refunding it into the
// current window would manufacture headroom the provider never had, and
// repeated refunds would walk the count negative.
func TestProviderBudgetReleaseOnARolledWindowIsANoOp(t *testing.T) {
	p := NewProviderBudget(2)
	first := time.Now()
	if !p.Allow(first) {
		t.Fatal("first request refused")
	}
	// Roll into a new window and consume one slot there. The window that
	// accepted `first` ends at first+1m, so this call both rolls the window
	// forward and establishes the new window's own resetAt.
	later := first.Add(2 * time.Minute)
	if !p.Allow(later) {
		t.Fatal("request in the new window refused")
	}
	if got := p.Remaining(later); got != 1 {
		t.Fatalf("Remaining = %d in the new window, want 1", got)
	}
	// A stale refund from the previous window must not add headroom. By now
	// `later` is also past the new window's resetAt, so the window has rolled
	// twice over.
	stale := first.Add(90 * time.Second)
	p.Release(stale)
	if got := p.Remaining(later); got != 1 {
		t.Errorf("Remaining = %d after a stale refund, want 1: "+
			"a refund from a previous window manufactured headroom", got)
	}
	// The stale refund must not have opened a SECOND slot, so the cap of 2 is
	// still exactly one request away from being full. This is the assertion
	// that fails when the stale refund is wrongly applied.
	p.Allow(later)
	if got := p.Remaining(later); got != 0 {
		t.Errorf("Remaining = %d after filling the last real slot, want 0: "+
			"the stale refund left extra headroom", got)
	}
	if p.Allow(later) {
		t.Error("a stale refund let an extra request through")
	}
	// After a real rollover the cap is free again, proving the window itself
	// still works and the assertion above was about the refund alone.
	if !p.Allow(later.Add(2 * time.Minute)) {
		t.Error("the cap did not free up after the window rolled")
	}
}

// TestProviderBudgetUnlimitedWhenRPMIsZero pins the zero-means-unlimited
// reading, which is how an operator disables the cap.
func TestProviderBudgetUnlimitedWhenRPMIsZero(t *testing.T) {
	p := NewProviderBudget(0)
	now := time.Now()
	for i := 0; i < 1000; i++ {
		if !p.Allow(now) {
			t.Fatalf("request %d refused with an unlimited cap", i)
		}
	}
	if got := p.Remaining(now); got != -1 {
		t.Errorf("Remaining = %d, want -1 for unlimited", got)
	}
	// Refunding an unlimited window must not underflow anything.
	p.Release(now)
	if got := p.Remaining(now); got != -1 {
		t.Errorf("Remaining = %d after a refund on an unlimited cap, want -1", got)
	}
}

// TestProviderBudgetSetRPMKeepsTheLiveWindow checks that a hot-reload tightens
// the cap without resetting the window. If it reset, an operator could raise
// traffic until the cap blocked, reload, and immediately be unblocked again.
func TestProviderBudgetSetRPMKeepsTheLiveWindow(t *testing.T) {
	p := NewProviderBudget(10)
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !p.Allow(now) {
			t.Fatalf("request %d refused", i)
		}
	}
	if p.Allow(now) {
		t.Fatal("the 11th request should be refused at a cap of 10")
	}
	p.SetRPM(10)
	if p.Allow(now) {
		t.Error("SetRPM reset the live window, so a full cap admitted another request")
	}
	// Tightening must take effect immediately.
	p.SetRPM(2)
	if got := p.Remaining(now); got != 0 {
		t.Errorf("Remaining = %d after tightening to 2, want 0", got)
	}
	// Loosening must not manufacture headroom beyond the new limit either.
	p.SetRPM(100)
	if got := p.Remaining(now); got != 90 {
		t.Errorf("Remaining = %d after loosening to 100, want 90", got)
	}
}

// TestProviderBudgetIsSafeUnderConcurrency exercises the cap from many
// goroutines. The cap is a hard limit, so the number admitted must never
// exceed it.
func TestProviderBudgetIsSafeUnderConcurrency(t *testing.T) {
	const limit = 50
	p := NewProviderBudget(limit)
	now := time.Now()

	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Allow(now) {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted > limit {
		t.Errorf("admitted %d requests against a cap of %d", admitted, limit)
	}
	if admitted != limit {
		t.Errorf("admitted %d, want exactly %d", admitted, limit)
	}
}

// TestAccumulateCostAddsWithoutTouchingInflight covers the separate accounting
// path. It must move cost but never the in-flight count, since it is used where
// no slot was reserved.
func TestAccumulateCostAddsWithoutTouchingInflight(t *testing.T) {
	b := NewKeyBudget(KeyLimits{MaxConcurrent: 1, CostLimitMicros: 10000})
	now := time.Now()
	if ok, reason := b.Acquire(now); !ok {
		t.Fatalf("acquire: %q", reason)
	}
	// Inflight is 1. AccumulateCost must not change it.
	b.AccumulateCost(2500)
	snap := b.Snapshot(now)
	if snap.Inflight != 1 {
		t.Errorf("Inflight = %d after AccumulateCost, want 1", snap.Inflight)
	}
	if snap.CostMicros != 2500 {
		t.Errorf("CostMicros = %d, want 2500", snap.CostMicros)
	}
	// The concurrency slot is still held, so a second acquire must be refused.
	if ok, _ := b.Acquire(now); ok {
		t.Error("AccumulateCost released the in-flight slot")
	}
	// And the accumulated cost counts toward the cap.
	b.Release(7500)
	if ok, reason := b.Acquire(now); ok {
		t.Errorf("acquire succeeded at exactly the cap: %q", reason)
	} else if reason != "key_cost_limit" {
		t.Errorf("reason = %q, want key_cost_limit", reason)
	}
}
