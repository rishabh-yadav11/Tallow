package budget

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/money"
)

func TestCostLimitFiresOnSubCentSpend(t *testing.T) {

	const capCents = 1
	limits := KeyLimits{
		CostLimitCents:  capCents,
		CostLimitMicros: money.CentsToMicroUSD(capCents),
	}
	b := NewKeyBudget(limits)
	now := time.Unix(1000, 0)

	for i := 0; i < 5; i++ {
		ok, reason := b.Acquire(now)
		if !ok {
			t.Fatalf("request %d rejected early with reason %q; want admission until the cap", i, reason)
		}
		b.Release(2000)
	}

	ok, reason := b.Acquire(now)
	if ok {
		t.Fatalf("Acquire succeeded after the 1-cent cap was reached; cost limits are unenforced")
	}
	if reason != "key_cost_limit" {
		t.Errorf("reason = %q, want %q", reason, "key_cost_limit")
	}
}

func TestZeroCostLimitMeansUnlimited(t *testing.T) {
	b := NewKeyBudget(KeyLimits{})
	now := time.Unix(1000, 0)
	for i := 0; i < 100; i++ {
		if ok, reason := b.Acquire(now); !ok {
			t.Fatalf("Acquire %d rejected with %q; an unset cost limit is unlimited", i, reason)
		}
		b.Release(1_000_000)
	}
}

func TestCostAccumulatesInMicroUSD(t *testing.T) {
	b := NewKeyBudget(KeyLimits{CostLimitMicros: money.CentsToMicroUSD(100)})
	now := time.Unix(1000, 0)

	b.Acquire(now)
	b.Release(2_500_000)

	s := b.Snapshot(now)
	if s.CostMicros != 2_500_000 {
		t.Errorf("CostMicros = %d, want 2500000", s.CostMicros)
	}
	if s.CostLimitMicros != money.CentsToMicroUSD(100) {
		t.Errorf("CostLimitMicros = %d, want %d", s.CostLimitMicros, money.CentsToMicroUSD(100))
	}

	b.SetLimits(KeyLimits{CostLimitMicros: money.CentsToMicroUSD(200)})
	s = b.Snapshot(now)
	if s.CostMicros != 2_500_000 {
		t.Errorf("CostMicros after SetLimits = %d, want 2500000 (reload must not reset cost)", s.CostMicros)
	}
	if s.CostLimitMicros != money.CentsToMicroUSD(200) {
		t.Errorf("CostLimitMicros after SetLimits = %d, want %d", s.CostLimitMicros, money.CentsToMicroUSD(200))
	}
}
