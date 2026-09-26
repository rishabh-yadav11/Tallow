package routing

import (
	"errors"
	"strings"
	"testing"
)

// A rate limit is only reportable as a 429 when it is the WHOLE story. If any
// candidate was rejected because something was actually wrong, the caller must
// not be told to wait, because waiting does not help and it hides a real fault
// behind a benign status.
//
// The classification therefore has to be all-or-nothing, and the cases below
// are the ones where getting it wrong would mislead an operator: a limit
// alongside a dead provider, a limit alongside an open breaker, and a provider
// that was removed by a hot reload.

// TestRouteErrorClassifiesLimitOnlyFailures pins the positive case from the
// real reason vocabulary the selection loop and KeyBudget.Acquire emit, not
// from strings invented for the test.
func TestRouteErrorClassifiesLimitOnlyFailures(t *testing.T) {
	limitOnly := [][]string{
		{"provider:p1:provider_rpm"},
		{"provider:p1:provider_rpm_race"},
		{"key:k1:key_rpm"},
		{"key:k1:key_quota"},
		{"key:k1:key_max_concurrent"},
		{"key:k1:key_cost_limit"},
		{"sticky:p1/k1:exhausted"},
		// A mixed trail where every entry is still a limit.
		{"sticky:p1/k1:exhausted", "provider:p1:provider_rpm"},
		{"provider:p1:provider_rpm", "key:k1:key_rpm", "key:k2:key_rpm"},
	}
	for _, reasons := range limitOnly {
		re := newRouteError("flash", reasons)
		if !re.Limited {
			t.Errorf("reasons %v classified as NOT limited: a client would be told "+
				"the provider is broken when a limit was actually reached", reasons)
		}
		if !errors.Is(error(re), ErrNoRoute) {
			t.Errorf("reasons %v: errors.Is(err, ErrNoRoute) = false, so the "+
				"caller cannot classify this failure at all", reasons)
		}
	}
}

// TestRouteErrorRefusesToCallAFaultALimit is the safety half. Each case pairs a
// limit with something genuinely wrong, or is entirely something wrong. None
// may be reported as a limit, or the caller waits for a limit to clear while a
// dead provider or an open breaker is the real problem.
func TestRouteErrorRefusesToCallAFaultALimit(t *testing.T) {
	faulty := [][]string{
		{"provider:p1:breaker_open"},
		{"provider:p1:no_key"},
		{"provider:p1:removed"},
		{"sticky:p1/k1:stale"},
		{"sticky:p1/k1:removed"},
		{"key:k1:no_budget"},
		{"key:k1:breaker_open"},
		{"key:k1:skip"},
		// A limit alongside a fault: the fault wins, because the limit alone
		// would not have produced this outcome.
		{"provider:p1:provider_rpm", "provider:p2:breaker_open"},
		{"sticky:p1/k1:exhausted", "provider:p1:provider_rpm", "provider:p2:no_key"},
		{"key:k1:key_rpm", "key:k2:breaker_open"},
		// An unrecognised reason must not be assumed healthy. Forward
		// compatibility cuts the other way: a new reason means the code adding
		// it knows something this classifier does not.
		{"provider:p1:some_future_reason"},
	}
	for _, reasons := range faulty {
		re := newRouteError("flash", reasons)
		if re.Limited {
			t.Errorf("reasons %v classified as limited: the caller would be told "+
				"to wait, which does not help when a provider is at fault", reasons)
		}
		if !errors.Is(error(re), ErrNoRoute) {
			t.Errorf("reasons %v: errors.Is(err, ErrNoRoute) = false", reasons)
		}
	}
}

// TestRouteErrorWithNoReasonsIsNotLimited is the degenerate case. An empty
// trail means the loop never even produced a candidate, which is not evidence
// that the providers are healthy, so it must not become a 429.
func TestRouteErrorWithNoReasonsIsNotLimited(t *testing.T) {
	re := newRouteError("flash", nil)
	if re.Limited {
		t.Error("an empty reason trail was classified as limited: nothing was " +
			"established about any provider")
	}
	if got, want := re.Error(), `no route for "flash" (reasons: )`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestRouteErrorPreservesTheAliasAndTrail pins that the classification did not
// cost the diagnostic. The reason trail is what an operator reads to work out
// which limit to raise, so it has to survive being classified.
func TestRouteErrorPreservesTheAliasAndTrail(t *testing.T) {
	reasons := []string{"sticky:p1/k1:exhausted", "provider:p1:provider_rpm"}
	re := newRouteError("flash", reasons)
	if re.Alias != "flash" {
		t.Errorf("Alias = %q, want flash", re.Alias)
	}
	if len(re.Reasons) != len(reasons) {
		t.Fatalf("Reasons = %v, want %v", re.Reasons, reasons)
	}
	for i := range reasons {
		if re.Reasons[i] != reasons[i] {
			t.Errorf("Reasons[%d] = %q, want %q", i, re.Reasons[i], reasons[i])
		}
	}
	if got := re.Error(); got == "" || !strings.Contains(got, "provider_rpm") {
		t.Errorf("Error() = %q, want it to name the limit that was reached", got)
	}
}
