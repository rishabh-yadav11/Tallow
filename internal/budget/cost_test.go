package budget

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/money"
)

// TestCostLimitFiresOnSubCentSpend is the C3 regression test at the budget
// level. Cost was accumulated in integer cents, so a request costing a
// fraction of a cent contributed 0 and `CostLimitCents` could never be
// reached. The budget now compares in micro-USD.
func TestCostLimitFiresOnSubCentSpend(t *testing.T) {
	// A 1-cent cap. Each request costs $0.002 (2000 micro-USD).
	const capCents = 1
	limits := KeyLimits{
		CostLimitCents:  capCents,
		CostLimitMicros: money.CentsToMicroUSD(capCents), // 10,000 micro-USD
	}
	b := NewKeyBudget(limits)
	now := time.Unix(1000, 0)

	// 5 requests * 2000 micro-USD = 10,000 micro-USD = exactly 1 cent.
	for i := 0; i < 5; i++ {
		ok, reason := b.Acquire(now)
		if !ok {
			t.Fatalf("request %d rejected early with reason %q; want admission until the cap", i, reason)
		}
		b.Release(2000) // $0.002 per request
	}

	// The cap is now exhausted. The next request must be rejected.
	ok, reason := b.Acquire(now)
	if ok {
		t.Fatalf("Acquire succeeded after the 1-cent cap was reached; cost limits are unenforced")
	}
	if reason != "key_cost_limit" {
		t.Errorf("reason = %q, want %q", reason, "key_cost_limit")
	}
}

// TestZeroCostLimitMeansUnlimited preserves the documented "0 = unlimited"
// contract while the unit changed.
func TestZeroCostLimitMeansUnlimited(t *testing.T) {
	b := NewKeyBudget(KeyLimits{}) // no cost limit
	now := time.Unix(1000, 0)
	for i := 0; i < 100; i++ {
		if ok, reason := b.Acquire(now); !ok {
			t.Fatalf("Acquire %d rejected with %q; an unset cost limit is unlimited", i, reason)
		}
		b.Release(1_000_000) // $1.00 each
	}
}

// TestCostAccumulatesInMicroUSD verifies the snapshot reports micro-USD and
// that SetLimits preserves the accumulated total across a hot-reload.
func TestCostAccumulatesInMicroUSD(t *testing.T) {
	b := NewKeyBudget(KeyLimits{CostLimitMicros: money.CentsToMicroUSD(100)})
	now := time.Unix(1000, 0)

	b.Acquire(now)
	b.Release(2_500_000) // $2.50

	s := b.Snapshot(now)
	if s.CostMicros != 2_500_000 {
		t.Errorf("CostMicros = %d, want 2500000", s.CostMicros)
	}
	if s.CostLimitMicros != money.CentsToMicroUSD(100) {
		t.Errorf("CostLimitMicros = %d, want %d", s.CostLimitMicros, money.CentsToMicroUSD(100))
	}

	// Hot-reload must not reset the accumulated cost.
	b.SetLimits(KeyLimits{CostLimitMicros: money.CentsToMicroUSD(200)})
	s = b.Snapshot(now)
	if s.CostMicros != 2_500_000 {
		t.Errorf("CostMicros after SetLimits = %d, want 2500000 (reload must not reset cost)", s.CostMicros)
	}
	if s.CostLimitMicros != money.CentsToMicroUSD(200) {
		t.Errorf("CostLimitMicros after SetLimits = %d, want %d", s.CostLimitMicros, money.CentsToMicroUSD(200))
	}
}
