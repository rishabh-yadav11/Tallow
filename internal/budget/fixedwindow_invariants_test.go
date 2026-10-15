package budget

import (
	"sync"
	"testing"
	"time"
)

func TestSetLimitsIsRaceFreeUnderConcurrentTraffic(t *testing.T) {
	b := NewKeyBudget(KeyLimits{RPM: 10, MaxRequests: 10, Window: time.Minute})
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.SetLimits(KeyLimits{RPM: 5 + n, MaxRequests: 5 + n, Window: time.Minute})
			}
		}(i)
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if ok, _ := b.Acquire(now); ok {
					b.Release(0)
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestSetLimitsPreservesLiveCounters(t *testing.T) {
	b := NewKeyBudget(KeyLimits{RPM: 10, Window: time.Minute})
	now := time.Now()
	for i := 0; i < 10; i++ {
		if ok, reason := b.Acquire(now); !ok {
			t.Fatalf("acquire %d: %q", i, reason)
		}
		b.Release(0)
	}

	if ok, _ := b.Acquire(now); ok {
		t.Fatal("the 11th request should be refused at RPM 10")
	}

	b.SetLimits(KeyLimits{RPM: 10, Window: time.Minute})
	if ok, _ := b.Acquire(now); ok {
		t.Error("a hot-reload with an unchanged limit reset the live RPM counter, " +
			"letting a full window admit another request")
	}
}

func TestCostLimitBoundary(t *testing.T) {
	b := NewKeyBudget(KeyLimits{CostLimitMicros: 1000})
	now := time.Now()
	if ok, reason := b.Acquire(now); !ok {
		t.Fatalf("first acquire: %q", reason)
	}
	b.Release(1000)
	if ok, reason := b.Acquire(now); ok {
		t.Errorf("acquire succeeded at exactly the cost cap (remaining=%d)",
			b.Snapshot(now).CostMicros)
	} else if reason != "key_cost_limit" {
		t.Errorf("reason = %q, want key_cost_limit", reason)
	}

	b2 := NewKeyBudget(KeyLimits{CostLimitMicros: 1000})
	b2.Release(999)
	if ok, reason := b2.Acquire(now); !ok {
		t.Errorf("acquire refused just under the cost cap: %q", reason)
	}
}

func TestRefundOnNeverUsedWindowIsANoOp(t *testing.T) {
	now := time.Now()

	w := FixedWindow{Limit: 2, Window: time.Minute}
	for i := 0; i < 10; i++ {
		w.Refund(now)
	}

	for i := 0; i < 2; i++ {
		if !w.Allow(now) {
			t.Fatalf("request %d refused: refunds on an unused window "+
				"manufactured headroom", i)
		}
	}
	if w.Allow(now) {
		t.Error("the window admits more than its limit after refunds on an " +
			"unused window")
	}
	if got := w.Remaining(now); got != 0 {
		t.Errorf("Remaining = %d, want 0: the count went negative", got)
	}

	u := FixedWindow{Limit: 0, Window: time.Minute}
	u.Refund(now)
	if !u.Allow(now) {
		t.Error("an unlimited window refused after a refund")
	}
	if got := u.Remaining(now); got != -1 {
		t.Errorf("Remaining = %d on an unlimited window, want -1", got)
	}
}

func TestUnmatchedRefundCannotWalkCountNegative(t *testing.T) {
	now := time.Now()
	w := FixedWindow{Limit: 4, Window: time.Minute}
	if !w.Allow(now) {
		t.Fatal("first request refused")
	}

	for i := 0; i < 100; i++ {
		w.Refund(now)
	}
	if got := w.Remaining(now); got != 4 {
		t.Errorf("Remaining = %d after 100 refunds against 1 charge, want 4: "+
			"the count went negative, so the cap is no longer enforced", got)
	}

	for i := 0; i < 4; i++ {
		if !w.Allow(now) {
			t.Fatalf("request %d refused below the cap", i)
		}
	}
	if w.Allow(now) {
		t.Error("the cap was not enforced after unmatched refunds")
	}
}

func TestRefundNeverExceedsRealChargesInTheWindow(t *testing.T) {
	w := FixedWindow{Limit: 3, Window: time.Minute}
	base := time.Now()

	if !w.Allow(base) {
		t.Fatal("first request refused")
	}

	w2 := FixedWindow{Limit: 3, Window: time.Minute}
	if !w2.Allow(base.Add(2 * time.Minute)) {
		t.Fatal("charge in the new window refused")
	}

	w2.Refund(base.Add(90 * time.Second))
	if got := w2.Remaining(base.Add(2 * time.Minute)); got != 2 {
		t.Errorf("Remaining = %d, want 2: a stale refund offset a charge that "+
			"had already been reset, inflating headroom", got)
	}
}

func TestInflightUnderflowFromUnmatchedReleases(t *testing.T) {
	b := NewKeyBudget(KeyLimits{MaxConcurrent: 2})
	now := time.Now()

	for i := 0; i < 10; i++ {
		b.Release(0)
	}
	for i := 0; i < 2; i++ {
		if ok, reason := b.Acquire(now); !ok {
			t.Fatalf("acquire %d: %q", i, reason)
		}
	}
	if ok, _ := b.Acquire(now); ok {
		t.Error("the concurrency cap was bypassed after unmatched releases")
	}
}

func TestFixedWindowExactResetBoundary(t *testing.T) {
	w := &FixedWindow{Limit: 1, Window: 10 * time.Second}
	base := time.Unix(1000, 0)
	if !w.Allow(base) {
		t.Fatal("first admission should succeed")
	}
	reset := w.ResetsAt(base)
	if reset != base.Add(10*time.Second) {
		t.Fatalf("ResetsAt = %v, want base+window (%v)", reset, base.Add(10*time.Second))
	}
	if w.Allow(reset) {
		t.Fatal("at exactly now==resetAt a full window must still refuse: the reset " +
			"happens strictly after resetAt, so the exact boundary is the old window")
	}
	if w.Peek(reset) {
		t.Fatal("peek at exactly resetAt must still see the old full window")
	}
	if got := w.Remaining(reset); got != 0 {
		t.Fatalf("Remaining at exactly resetAt = %d, want 0 (the old window)", got)
	}
	if !w.Allow(reset.Add(1)) {
		t.Fatal("an instant after resetAt the window must have rolled and admit again")
	}
}
