package money

import "testing"

// TestCostMicrosNotTruncated reproduces the C3 defect at the unit level. The
// audit showed that at $0.50/1M in and $1.50/1M out, 7 of 8 realistic
// request shapes had a real cost but recorded exactly 0 cents, because cost was
// accumulated in integer cents and truncated toward zero. Every row below must
// now produce a non-zero micro-USD amount.
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
		// One micro-USD is the unit of account, so a real cost must survive at
		// roughly 1e-6 USD precision. Anything below half a micro-USD is
		// genuinely unrepresentable and may legitimately round to zero.
		if diff := gotUSD - tc.wantUSD; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("CostMicros(%d,%d) = %d micro-USD = %g USD, want %g USD",
				tc.prompt, tc.completion, got, gotUSD, tc.wantUSD)
		}
	}
}

// TestCostMicrosAggregate pins the audit's headline case: 100,000 requests at
// p=200 c=100 and $0.15/$0.60 per 1M tokens must total $9.00, where the old
// cents path recorded 0.
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

// TestCostMicrosZeroPricing covers the documented "0 = unknown pricing" rule.
func TestCostMicrosZeroPricing(t *testing.T) {
	if got := CostMicros(0, 0, 1000, 1000); got != 0 {
		t.Errorf("CostMicros with no pricing = %d, want 0", got)
	}
	// One axis priced is enough to produce a cost.
	if got := CostMicros(1.0, 0, 1_000_000, 0); got != 1_000_000 {
		t.Errorf("CostMicros(1.0, 0, 1e6, 0) = %d, want 1000000 micro-USD ($1.00)", got)
	}
	// No tokens, no cost.
	if got := CostMicros(1.0, 1.0, 0, 0); got != 0 {
		t.Errorf("CostMicros with zero tokens = %d, want 0", got)
	}
}

// TestCentsRoundTrip covers the config/display boundary. `cost_limit_cents`
// stays in cents for operators while the budget compares in micro-USD.
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

// TestMicroUSDToCentsRoundsAwayFromZero ensures a sub-cent spend never displays
// as exactly $0.00, which is how the truncation hid real cost from operators.
func TestMicroUSDToCentsRoundsAwayFromZero(t *testing.T) {
	// An exact multiple of a cent is unchanged.
	if got := MicroUSDToCents(20_000); got != 2 {
		t.Errorf("MicroUSDToCents(20000) = %d, want 2", got)
	}
	// Any positive remainder rounds up to the next whole cent, never down to 0,
	// so real spend never displays as exactly $0.00.
	cases := []struct{ micros, want int64 }{
		{1, 1},      // $0.000001 -> 1 cent
		{5_000, 1},  // $0.005, half a cent -> 1
		{9_999, 1},  // just under a cent
		{10_001, 2}, // just over a cent
		{12_345, 2},
		{19_999, 2},      // just under two cents
		{20_000, 2},      // exactly two cents
		{2_000_000, 200}, // $2.00
	}
	for _, tc := range cases {
		if got := MicroUSDToCents(tc.micros); got != tc.want {
			t.Errorf("MicroUSDToCents(%d) = %d, want %d (round up so spend stays visible)",
				tc.micros, got, tc.want)
		}
	}
	// Negative values round away from zero symmetrically.
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
