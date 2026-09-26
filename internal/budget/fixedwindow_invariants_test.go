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
func TestZZZSetLimitsRace(t *testing.T) {
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
func TestZZZSetLimitsPreservesCounters(t *testing.T) {
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
func TestZZZCostLimitBoundary(t *testing.T) {
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

// maxInflight is a hard cap. Does the accounting survive a Release that was
// never paired with an Acquire?
func TestZZZInflightUnderflow(t *testing.T) {
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
