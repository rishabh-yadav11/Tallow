package budget

import (
	"testing"
	"time"
)

func TestFixedWindowRPM(t *testing.T) {
	w := FixedWindow{Limit: 3, Window: time.Minute}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !w.Allow(now) {
			t.Fatalf("expected allow %d", i)
		}
	}
	if w.Allow(now) {
		t.Fatal("expected deny after limit")
	}
	// Rollover resets the counter.
	if !w.Allow(now.Add(2 * time.Minute)) {
		t.Fatal("expected allow after window rollover")
	}
}

func TestPeekIsPure(t *testing.T) {
	w := FixedWindow{Limit: 1, Window: time.Minute}
	now := time.Now()
	// Fill the window.
	if !w.Allow(now) {
		t.Fatal("expected first allow")
	}
	// Peek must not mutate: still denied while full.
	if w.Peek(now) {
		t.Fatal("peek should deny a full window")
	}
	if w.Allow(now) {
		t.Fatal("allow should still deny a full window after peek")
	}
}

// TestQuotaFailureDoesNotBurnRPM guards a regression where a request blocked by
// the quota limit still consumed an RPM unit, making the RPM counter lie.
func TestQuotaFailureDoesNotBurnRPM(t *testing.T) {
	b := NewKeyBudget(KeyLimits{RPM: 100, MaxRequests: 2, Window: time.Hour})
	now := time.Now()
	if ok, _ := b.Acquire(now); !ok {
		t.Fatal("acquire 1 failed")
	}
	if ok, _ := b.Acquire(now); !ok {
		t.Fatal("acquire 2 failed")
	}
	ok, reason := b.Acquire(now)
	if ok {
		t.Fatal("acquire 3 should fail on quota")
	}
	if reason != "key_quota" {
		t.Fatalf("reason = %q, want key_quota", reason)
	}
	s := b.Snapshot(now)
	if s.RPMRemaining != 98 {
		t.Fatalf("RPM remaining = %d, want 98 (quota failures must not burn RPM)", s.RPMRemaining)
	}
}

func TestMaxConcurrentEnforced(t *testing.T) {
	b := NewKeyBudget(KeyLimits{MaxConcurrent: 1, RPM: 0})
	now := time.Now()
	if ok, reason := b.Acquire(now); !ok {
		t.Fatalf("first acquire failed: %s", reason)
	}
	if ok, _ := b.Acquire(now); ok {
		t.Fatal("second acquire should fail on max_concurrent")
	}
	b.Release(0)
	if ok, _ := b.Acquire(now); !ok {
		t.Fatal("acquire after release should succeed")
	}
}
