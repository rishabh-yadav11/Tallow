package budget

import (
	"sync"
	"testing"
	"time"
)

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

func TestProviderBudgetReleaseOnARolledWindowIsANoOp(t *testing.T) {
	p := NewProviderBudget(2)
	first := time.Now()
	if !p.Allow(first) {
		t.Fatal("first request refused")
	}

	later := first.Add(2 * time.Minute)
	if !p.Allow(later) {
		t.Fatal("request in the new window refused")
	}
	if got := p.Remaining(later); got != 1 {
		t.Fatalf("Remaining = %d in the new window, want 1", got)
	}

	stale := first.Add(90 * time.Second)
	p.Release(stale)
	if got := p.Remaining(later); got != 1 {
		t.Errorf("Remaining = %d after a stale refund, want 1: "+
			"a refund from a previous window manufactured headroom", got)
	}

	p.Allow(later)
	if got := p.Remaining(later); got != 0 {
		t.Errorf("Remaining = %d after filling the last real slot, want 0: "+
			"the stale refund left extra headroom", got)
	}
	if p.Allow(later) {
		t.Error("a stale refund let an extra request through")
	}

	if !p.Allow(later.Add(2 * time.Minute)) {
		t.Error("the cap did not free up after the window rolled")
	}
}

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

	p.Release(now)
	if got := p.Remaining(now); got != -1 {
		t.Errorf("Remaining = %d after a refund on an unlimited cap, want -1", got)
	}
}

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

	p.SetRPM(2)
	if got := p.Remaining(now); got != 0 {
		t.Errorf("Remaining = %d after tightening to 2, want 0", got)
	}

	p.SetRPM(100)
	if got := p.Remaining(now); got != 90 {
		t.Errorf("Remaining = %d after loosening to 100, want 90", got)
	}
}

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

func TestAccumulateCostAddsWithoutTouchingInflight(t *testing.T) {
	b := NewKeyBudget(KeyLimits{MaxConcurrent: 1, CostLimitMicros: 10000})
	now := time.Now()
	if ok, reason := b.Acquire(now); !ok {
		t.Fatalf("acquire: %q", reason)
	}

	b.AccumulateCost(2500)
	snap := b.Snapshot(now)
	if snap.Inflight != 1 {
		t.Errorf("Inflight = %d after AccumulateCost, want 1", snap.Inflight)
	}
	if snap.CostMicros != 2500 {
		t.Errorf("CostMicros = %d, want 2500", snap.CostMicros)
	}

	if ok, _ := b.Acquire(now); ok {
		t.Error("AccumulateCost released the in-flight slot")
	}

	b.Release(7500)
	if ok, reason := b.Acquire(now); ok {
		t.Errorf("acquire succeeded at exactly the cap: %q", reason)
	} else if reason != "key_cost_limit" {
		t.Errorf("reason = %q, want key_cost_limit", reason)
	}
}
