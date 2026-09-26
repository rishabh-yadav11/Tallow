package budget

import (
	"sync"
	"testing"
	"time"
)

// SetLimits is documented as "applies updated limits without resetting live
// counters". But it writes b.rpm.Limit and b.req.Limit WITHOUT taking the
// FixedWindow's own mutex - it only holds b.mu. FixedWindow.Allow/Peek/Remaining
// take w.mu. Is b.mu sufficient to exclude them?
func TestSetLimitsIsRaceFreeUnderConcurrentTraffic(t *testing.T) {
	b := NewKeyBudget(KeyLimits{RPM: 10, MaxRequests: 10, Window: time.Minute})
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent hot-reloads, as an operator's config watcher would do.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.SetLimits(KeyLimits{RPM: 5 + n, MaxRequests: 5 + n, Window: time.Minute})
			}
		}(i)
	}
	// Concurrent traffic.
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

// The documented guarantee: SetLimits must not reset live counters. If an
// operator tightens a limit, requests already admitted in this window must
// still count against it, or the limit can be bypassed by reloading.
func TestSetLimitsPreservesLiveCounters(t *testing.T) {
	b := NewKeyBudget(KeyLimits{RPM: 10, Window: time.Minute})
	now := time.Now()
	for i := 0; i < 10; i++ {
		if ok, reason := b.Acquire(now); !ok {
			t.Fatalf("acquire %d: %q", i, reason)
		}
		b.Release(0)
	}
	// Window is full at RPM 10.
	if ok, _ := b.Acquire(now); ok {
		t.Fatal("the 11th request should be refused at RPM 10")
	}
	// Reload with the SAME limit: the window must still be full.
	b.SetLimits(KeyLimits{RPM: 10, Window: time.Minute})
	if ok, _ := b.Acquire(now); ok {
		t.Error("a hot-reload with an unchanged limit reset the live RPM counter, " +
			"letting a full window admit another request")
	}
}

// Release with cost below the cap: the cost limit is a running total, and once
// reached it must stay reached. Check the boundary is not off by one.
func TestCostLimitBoundary(t *testing.T) {
	b := NewKeyBudget(KeyLimits{CostLimitMicros: 1000})
	now := time.Now()
	if ok, reason := b.Acquire(now); !ok {
		t.Fatalf("first acquire: %q", reason)
	}
	b.Release(1000) // exactly at the cap
	if ok, reason := b.Acquire(now); ok {
		t.Errorf("acquire succeeded at exactly the cost cap (remaining=%d)",
			b.Snapshot(now).CostMicros)
	} else if reason != "key_cost_limit" {
		t.Errorf("reason = %q, want key_cost_limit", reason)
	}

	// One micro under the cap must still admit.
	b2 := NewKeyBudget(KeyLimits{CostLimitMicros: 1000})
	b2.Release(999)
	if ok, reason := b2.Acquire(now); !ok {
		t.Errorf("acquire refused just under the cost cap: %q", reason)
	}
}

// A FixedWindow built as a struct literal (as several tests and the production
// constructors do) has a zero startAt until its first Allow. A refund against
// such a window must be a no-op: nothing was ever charged, so decrementing
// would manufacture headroom. Refund has no explicit check for this, because
// the count is still 0 and the clamp refuses to decrement it; this test pins
// that property so the clamp cannot be removed on the assumption that a
// separate guard covers it.
func TestRefundOnNeverUsedWindowIsANoOp(t *testing.T) {
	now := time.Now()
	// Struct literal, exactly as the production constructors build one.
	w := FixedWindow{Limit: 2, Window: time.Minute}
	for i := 0; i < 10; i++ {
		w.Refund(now)
	}
	// The window must still be completely free: no refund invented a charge
	// that was then refunded, which would have pushed count below zero and
	// left the window permanently over-admitting.
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

	// The same holds for a window whose limit is unlimited: refunds must not
	// make an unlimited window start refusing.
	u := FixedWindow{Limit: 0, Window: time.Minute}
	u.Refund(now)
	if !u.Allow(now) {
		t.Error("an unlimited window refused after a refund")
	}
	if got := u.Remaining(now); got != -1 {
		t.Errorf("Remaining = %d on an unlimited window, want -1", got)
	}
}

// An unmatched refund on a window that IS in use must not walk the count
// negative either. The clamp is the second line of defence, and the symptom of
// losing it is severe: count goes below zero, so Remaining exceeds the limit and
// the window silently stops enforcing its cap for the rest of the window.
func TestUnmatchedRefundCannotWalkCountNegative(t *testing.T) {
	now := time.Now()
	w := FixedWindow{Limit: 4, Window: time.Minute}
	if !w.Allow(now) {
		t.Fatal("first request refused")
	}
	// 100 refunds against a single charge.
	for i := 0; i < 100; i++ {
		w.Refund(now)
	}
	if got := w.Remaining(now); got != 4 {
		t.Errorf("Remaining = %d after 100 refunds against 1 charge, want 4: "+
			"the count went negative, so the cap is no longer enforced", got)
	}
	// And the cap must actually bite at the limit.
	for i := 0; i < 4; i++ {
		if !w.Allow(now) {
			t.Fatalf("request %d refused below the cap", i)
		}
	}
	if w.Allow(now) {
		t.Error("the cap was not enforced after unmatched refunds")
	}
}

// The stale-refund regression at the FixedWindow level, stating the invariant
// directly: a refund must never increase headroom beyond what a real charge
// left available.
func TestRefundNeverExceedsRealChargesInTheWindow(t *testing.T) {
	w := FixedWindow{Limit: 3, Window: time.Minute}
	base := time.Now()

	// Establish a window at base, then charge it once.
	if !w.Allow(base) {
		t.Fatal("first request refused")
	}
	// A refund timestamped before that window began, but after the window's
	// span was entered by a LATER charge. Simulates a request admitted at the
	// tail of the previous window and aborted just after the boundary.
	w2 := FixedWindow{Limit: 3, Window: time.Minute}
	if !w2.Allow(base.Add(2 * time.Minute)) { // rolls the window to base+2m
		t.Fatal("charge in the new window refused")
	}
	// Refund the old-window charge, timestamped inside the new window's span.
	w2.Refund(base.Add(90 * time.Second))
	if got := w2.Remaining(base.Add(2 * time.Minute)); got != 2 {
		t.Errorf("Remaining = %d, want 2: a stale refund offset a charge that "+
			"had already been reset, inflating headroom", got)
	}
}

// maxInflight is a hard cap. Does the accounting survive a Release that was
// never paired with an Acquire?
func TestInflightUnderflowFromUnmatchedReleases(t *testing.T) {
	b := NewKeyBudget(KeyLimits{MaxConcurrent: 2})
	now := time.Now()
	// Unmatched releases must not manufacture concurrency headroom.
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
