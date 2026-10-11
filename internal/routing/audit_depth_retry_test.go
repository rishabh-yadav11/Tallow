package routing

// P3 audit tests for the router depth bound and routing-failure classification:
//   - a fallback chain of depth exactly maxFallbackDepth (8) expands to depth 8 but
//     a depth-9 hop is dropped (item 9);
//   - a fully-exhausted provider RPM set carries the EARLIEST reset across multiple
//     exhausted providers, and RetryAfter is zero when the failure is not limited
//     (item 9);
//   - splitID on an id without "/" yields the id with an empty key, and
//     ProviderRPM on a provider without a cap is -1 (item 15).

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

func TestMaxFallbackDepthExpandsExactlyToDepthCap(t *testing.T) {
	// A chain p0 -> p1 -> ... -> p9, each provider falling back to the next.
	// maxFallbackDepth=8: a chain of depth exactly 8 must have expanded (p0..p8
	// are candidates), and the depth-9 hop (p9) must have been dropped.
	provs := []model.Provider{{Name: "p0", Fallback: []string{"p1"}}}
	for i := 1; i < 9; i++ {
		provs = append(provs, model.Provider{Name: fmt.Sprintf("p%d", i), Fallback: []string{fmt.Sprintf("p%d", i+1)}})
	}
	provs = append(provs, model.Provider{Name: "p9"})
	aliases := []model.Alias{{Name: "a", Targets: []model.Target{{Provider: "p0", Model: "m"}}}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, time.Now)

	var got []string
	for _, c := range r.candidates(&aliases[0]) {
		got = append(got, c.provider)
	}
	if len(got) != maxFallbackDepth+1 {
		t.Fatalf("candidates have %d providers, want exactly depth-cap+1 (%d): "+
			"p0..p8 must expand, p9 must not", len(got), maxFallbackDepth+1)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Fatalf("candidate %q appeared twice", p)
		}
		seen[p] = true
	}
	if !seen["p8"] {
		t.Fatalf("a chain of depth exactly %d must expand through p8; got %v",
			maxFallbackDepth, got)
	}
	if seen["p9"] {
		t.Fatalf("a depth-%d hop must be dropped (maxFallbackDepth=%d); got %v",
			maxFallbackDepth+1, maxFallbackDepth, got)
	}
}

func TestSelectRetryAfterIsEarliestResetAcrossExhaustedProviders(t *testing.T) {
	// Two providers, both with provider RPM=1, exhausted at different times. The
	// RouteError must carry the reset of the provider that frees up FIRST, so a
	// client is told when a retry can actually succeed.
	reg := registry.New(
		[]model.Provider{
			{Name: "p1", BaseURL: "https://p1/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
			{Name: "p2", BaseURL: "https://p2/v1", Keys: []model.Key{{ID: "k2", Secret: "s2"}}},
		},
		[]model.Alias{{Name: "flash", Targets: []model.Target{
			{Provider: "p1", Model: "m"},
			{Provider: "p2", Model: "m"},
		}}},
	)
	cur := time.Unix(1000, 0)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return cur })
	r.SetBudgets(map[string]budget.KeyLimits{
		"p1/k1": {RPM: 1000},
		"p2/k2": {RPM: 1000},
	}, map[string]int{"p1": 1, "p2": 1})

	// Exhaust p1's provider RPM at t0.
	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatalf("select p1: %v", err)
	}
	r.Release(sel, 0)

	// Exhaust p2's provider RPM 15s later.
	cur = cur.Add(15 * time.Second)
	sel, err = r.Select("flash", "", nil)
	if err != nil {
		t.Fatalf("select p2: %v", err)
	}
	r.Release(sel, 0)

	// Now both are exhausted; p1 frees up in 45s, p2 in 60s. The reported wait
	// must be the EARLIEST of the two.
	_, err = r.Select("flash", "", nil)
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("Select with both providers exhausted: %v, want ErrNoRoute", err)
	}
	var re *RouteError
	if !errors.As(err, &re) {
		t.Fatalf("Select returned %T, want *RouteError", err)
	}
	if !re.Limited {
		t.Fatalf("both rejections were provider_rpm limits, want Limited=true: %v", re.Reasons)
	}
	if want := 45 * time.Second; re.RetryAfter != want {
		t.Fatalf("RetryAfter = %v, want the earliest reset %v (p1 then p2)", re.RetryAfter, want)
	}
}

func TestRetryAfterIsZeroWhenNotLimited(t *testing.T) {
	// A breaker-open failure is not a limit, so the classification must discard
	// any retry hint: answering "come back in N seconds" to a dead provider would
	// send the client off to wait at a provider that is not rate limited.
	reg := registry.New(
		[]model.Provider{{Name: "p1", BaseURL: "https://p1/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}}},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "m"}}}},
	)
	now := time.Unix(1000, 0)
	h := health.NewRegistry()
	r := NewRouter(reg, h, time.Minute, func() time.Time { return now })
	r.SetBudgets(map[string]budget.KeyLimits{
		"p1/k1": {RPM: 1000},
	}, map[string]int{"p1": 5})
	h.Provider("p1").ForceOpen(now)

	_, err := r.Select("flash", "", nil)
	var re *RouteError
	if !errors.As(err, &re) {
		t.Fatalf("Select returned %T, want *RouteError", err)
	}
	if re.Limited {
		t.Fatalf("breaker_open is not a limit: Limited must be false, got %v", re.Reasons)
	}
	if re.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0: a non-limit failure must not carry a "+
			"retry hint, or a dead provider would tell the client to wait", re.RetryAfter)
	}
}

func TestSplitIDWithoutSlash(t *testing.T) {
	prov, key := splitID("no-slash-here")
	if prov != "no-slash-here" || key != "" {
		t.Fatalf("splitID(%q) = (%q, %q), want (id, \"\")", "no-slash-here", prov, key)
	}
	prov, key = splitID("p/k")
	if prov != "p" || key != "k" {
		t.Fatalf("splitID(%q) = (%q, %q), want (p, k)", "p/k", prov, key)
	}
}

func TestProviderRPMUnknownReturnsMinusOne(t *testing.T) {
	r := NewRouter(registry.New(nil, nil), health.NewRegistry(), time.Minute, time.Now)
	if got := r.ProviderRPM("no-such-provider", time.Time{}); got != -1 {
		t.Fatalf("ProviderRPM for an unknown provider = %d, want -1 (no cap)", got)
	}
}
