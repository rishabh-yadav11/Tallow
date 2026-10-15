package money

import (
	"math"
	"testing"
)

func TestCostMicrosNotTruncated(t *testing.T) {
	const priceIn, priceOut = 0.50, 1.50
	cases := []struct {
		prompt, completion int
		wantUSD            float64
	}{
		{1000, 1000, 0.002},
		{100, 50, 0.000125},
		{50, 20, 0.000055},
		{10, 5, 0.0000125},
		{200, 100, 0.00025},
		{2000, 1000, 0.0025},
	}
	for _, tc := range cases {
		got := CostMicros(priceIn, priceOut, tc.prompt, tc.completion)
		if got == 0 {
			t.Errorf("CostMicros(%d,%d) = 0; a request with real cost %g USD must not truncate to zero",
				tc.prompt, tc.completion, tc.wantUSD)
		}
		gotUSD := MicroUSDtoDollars(got)

		if diff := gotUSD - tc.wantUSD; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("CostMicros(%d,%d) = %d micro-USD = %g USD, want %g USD",
				tc.prompt, tc.completion, got, gotUSD, tc.wantUSD)
		}
	}
}

func TestCostMicrosAggregate(t *testing.T) {
	const (
		requests   = 100_000
		prompt     = 200
		completion = 100
		priceIn    = 0.15
		priceOut   = 0.60
	)
	per := CostMicros(priceIn, priceOut, prompt, completion)
	total := per * requests
	wantUSD := 9.0
	gotUSD := MicroUSDtoDollars(total)
	if diff := gotUSD - wantUSD; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("aggregate = %d micro-USD = %g USD, want %g USD", total, gotUSD, wantUSD)
	}
	if total == 0 {
		t.Fatalf("aggregate cost is zero; the cents truncation defect is present")
	}
}

func TestCostMicrosZeroPricing(t *testing.T) {
	if got := CostMicros(0, 0, 1000, 1000); got != 0 {
		t.Errorf("CostMicros with no pricing = %d, want 0", got)
	}

	if got := CostMicros(1.0, 0, 1_000_000, 0); got != 1_000_000 {
		t.Errorf("CostMicros(1.0, 0, 1e6, 0) = %d, want 1000000 micro-USD ($1.00)", got)
	}

	if got := CostMicros(1.0, 1.0, 0, 0); got != 0 {
		t.Errorf("CostMicros with zero tokens = %d, want 0", got)
	}
}

func TestCentsRoundTrip(t *testing.T) {
	cases := []int64{0, 1, 5, 500, 5000, 123456}
	for _, cents := range cases {
		micros := CentsToMicroUSD(cents)
		if micros != cents*MicroUSDPerCent {
			t.Errorf("CentsToMicroUSD(%d) = %d, want %d", cents, micros, cents*MicroUSDPerCent)
		}
		if got := MicroUSDToCents(micros); got != cents {
			t.Errorf("round trip of %d cents = %d", cents, got)
		}
	}
}

func TestMicroUSDToCentsRoundsAwayFromZero(t *testing.T) {

	if got := MicroUSDToCents(20_000); got != 2 {
		t.Errorf("MicroUSDToCents(20000) = %d, want 2", got)
	}

	cases := []struct{ micros, want int64 }{
		{1, 1},
		{5_000, 1},
		{9_999, 1},
		{10_001, 2},
		{12_345, 2},
		{19_999, 2},
		{20_000, 2},
		{2_000_000, 200},
	}
	for _, tc := range cases {
		if got := MicroUSDToCents(tc.micros); got != tc.want {
			t.Errorf("MicroUSDToCents(%d) = %d, want %d (round up so spend stays visible)",
				tc.micros, got, tc.want)
		}
	}

	if got := MicroUSDToCents(-1); got != -1 {
		t.Errorf("MicroUSDToCents(-1) = %d, want -1", got)
	}
	if got := MicroUSDToCents(-20_000); got != -2 {
		t.Errorf("MicroUSDToCents(-20000) = %d, want -2", got)
	}
	if got := MicroUSDToCents(0); got != 0 {
		t.Errorf("MicroUSDToCents(0) = %d, want 0", got)
	}
}

func TestCentsToMicroUSDOverflowBounds(t *testing.T) {
	t.Run("max representable rounds trip", func(t *testing.T) {
		cents := math.MaxInt64 / MicroUSDPerCent
		if got := CentsToMicroUSD(cents); got != cents*MicroUSDPerCent {
			t.Errorf("CentsToMicroUSD(%d) = %d, want %d", cents, got, cents*MicroUSDPerCent)
		}
		if got := MicroUSDToCents(CentsToMicroUSD(cents)); got != cents {
			t.Errorf("round trip of max cents = %d, want %d", got, cents)
		}
	})
	t.Run("just above overflow clamps to max", func(t *testing.T) {
		cents := math.MaxInt64/MicroUSDPerCent + 1
		if got := CentsToMicroUSD(cents); got != math.MaxInt64 {
			t.Errorf("CentsToMicroUSD(%d) = %d, want math.MaxInt64 (clamped, not wrapped)", cents, got)
		}
		if got := CentsToMicroUSD(math.MaxInt64); got != math.MaxInt64 {
			t.Errorf("CentsToMicroUSD(math.MaxInt64) = %d, want math.MaxInt64", got)
		}
	})
	t.Run("negative clamps to min", func(t *testing.T) {
		if got := CentsToMicroUSD(math.MinInt64); got != math.MinInt64 {
			t.Errorf("CentsToMicroUSD(math.MinInt64) = %d, want math.MinInt64", got)
		}
	})
	t.Run("negative ordinary", func(t *testing.T) {
		if got := CentsToMicroUSD(-100); got != -1_000_000 {
			t.Errorf("CentsToMicroUSD(-100) = %d, want -1000000", got)
		}
	})
	t.Run("zero-centered round", func(t *testing.T) {
		if got := MicroUSDToCents(-5000); got != -1 {
			t.Errorf("MicroUSDToCents(-5000) = %d, want -1 (round away from zero)", got)
		}
		if got := MicroUSDToCents(5000); got != 1 {
			t.Errorf("MicroUSDToCents(5000) = %d, want 1", got)
		}
	})
}
