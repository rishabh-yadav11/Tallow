package routing

import (
	"errors"
	"strings"
	"testing"
)

func TestRouteErrorClassifiesLimitOnlyFailures(t *testing.T) {
	limitOnly := [][]string{
		{"provider:p1:provider_rpm"},
		{"provider:p1:provider_rpm_race"},
		{"key:k1:key_rpm"},
		{"key:k1:key_quota"},
		{"key:k1:key_max_concurrent"},
		{"key:k1:key_cost_limit"},
		{"sticky:p1/k1:exhausted"},

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

		{"provider:p1:provider_rpm", "provider:p2:breaker_open"},
		{"sticky:p1/k1:exhausted", "provider:p1:provider_rpm", "provider:p2:no_key"},
		{"key:k1:key_rpm", "key:k2:breaker_open"},

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
