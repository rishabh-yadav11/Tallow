package routing

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

// This file drives the RPM cap through the real routing entry point, Select,
// and reads the result back through the router's own accounting accessors.
//
// Refunds are only reachable in production through two paths: Release, for a
// request that ran, and releaseSelection, the ABANDON path, for a candidate
// Select charged and then gave up on. A unit test on budget.FixedWindow proves
// the window guard rejects a refund stamped before the current window, but it
// cannot prove which timestamp the router actually hands it. This does.
//
// The bypass these tests pin was in that timestamp. Select charges a provider's
// RPM at admittedAt, but releaseSelection stamped the refund with r.now() at
// RELEASE time. When the minute window rolled over in between, the
// release-time stamp looked current, so the refund decremented a count that
// this selection's charge had never contributed to. Every abandoned request
// that outlived its window bought one extra request of headroom, and the
// operator's configured provider RPM stopped meaning anything. FixedWindow's
// window guard could not catch it: given a current timestamp, refusing it would
// have wrongly punished a genuine in-window abandon. The pairing has to be
// exact, so the refund now carries the admission time that was charged.
//
// The clock is injected (NewRouter's now func), so window boundaries are
// crossed exactly, with no sleeping and no flakiness.

// rpmRouter builds a single-provider router whose only cap is a provider RPM of
// rpm, with key RPM left effectively unlimited. The provider rpm is the limit
// under test, so nothing else may constrain it.
func rpmRouter(t *testing.T, rpm int, now *time.Time) *Router {
	t.Helper()
	provs := []model.Provider{{
		Name:    "p1",
		BaseURL: "https://p1.example/v1",
		Keys:    []model.Key{{ID: "k1", Secret: "s1", RPM: 1000}},
	}}
	aliases := []model.Alias{{
		Name: "flash",
		Targets: []model.Target{
			{Provider: "p1", Model: "flash-v4", PriceInputPerM: 1.0, PriceOutputPerM: 2.0},
		},
	}}
	reg := registry.New(provs, aliases)
	h := health.NewRegistry()
	r := NewRouter(reg, h, time.Minute, func() time.Time { return *now })
	r.SetBudgets(
		map[string]budget.KeyLimits{"p1/k1": {RPM: 1000}},
		map[string]int{"p1": rpm},
	)
	return r
}

// providerRemaining reports the provider's live RPM headroom, which is the
// operator-configured number an RPM bypass would inflate.
func providerRemaining(t *testing.T, r *Router, now time.Time) int {
	t.Helper()
	r.bmu.RLock()
	pb := r.pBudget["p1"]
	r.bmu.RUnlock()
	if pb == nil {
		t.Fatal("no provider budget is registered for p1")
	}
	return pb.Remaining(now)
}

// TestAbandonedCandidatesCannotManufactRPMAcrossAWindowBoundary is the
// acceptance check for the fix. It runs real Selects against a real provider
// cap, abandons a selection across a window rollover, and asserts the cap is
// never exceeded in the window the abandon lands in.
func TestAbandonedCandidatesCannotManufactRPMAcrossAWindowBoundary(t *testing.T) {
	const rpm = 3
	const window = time.Minute

	now := time.Unix(1000, 0)
	r := rpmRouter(t, rpm, &now)

	// --- Window 1: exhaust the cap honestly, then abandon selections.
	for i := 0; i < rpm; i++ {
		sel, err := r.Select("flash", "", nil)
		if err != nil {
			t.Fatalf("window 1 request %d: %v", i, err)
		}
		// The request runs, so it keeps its RPM slot for the window: Release
		// deliberately does not refund provider RPM.
		r.Release(sel, 0)
	}
	if got := providerRemaining(t, r, now); got != 0 {
		t.Fatalf("window 1: remaining=%d, want 0 after %d requests", got, rpm)
	}
	// The cap must now reject.
	if _, err := r.Select("flash", "", nil); err == nil {
		t.Fatalf("window 1: provider RPM %d was exceeded: a %dth request was admitted", rpm, rpm+1)
	}

	// --- Roll into window 2. The fresh window starts with full headroom.
	now = now.Add(2 * window)
	if got := providerRemaining(t, r, now); got != rpm {
		t.Fatalf("window 2 starts with remaining=%d, want the full %d", got, rpm)
	}

	// --- The attack, in the order it actually happens. A selection is charged
	// in window 2. The window then rolls over and REAL requests arrive in
	// window 3, which re-establishes window 3 and gives it a fresh count. Only
	// then does the abandoned window-2 selection get released.
	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatalf("window 2: %v", err)
	}
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Fatalf("window 2: after one admission remaining=%d, want %d", got, rpm-1)
	}

	// The upstream call is in flight while the window rolls over.
	now = now.Add(2 * window)
	if got := providerRemaining(t, r, now); got != rpm {
		t.Fatalf("window 3 starts with remaining=%d, want the full %d", got, rpm)
	}

	// Window 3 takes real traffic first. This is the step that matters: it
	// resets the window's count to 0 and then charges it, so window 3 now has
	// a live count that a stale refund must not touch.
	const realInWindow3 = 2
	for i := 0; i < realInWindow3; i++ {
		s, err := r.Select("flash", "", nil)
		if err != nil {
			t.Fatalf("window 3 request %d: %v", i, err)
		}
		r.Release(s, 0)
	}
	if got, want := providerRemaining(t, r, now), rpm-realInWindow3; got != want {
		t.Fatalf("window 3: after %d real requests remaining=%d, want %d",
			realInWindow3, got, want)
	}

	// Now the abandon finally lands, carrying a timestamp from window 2.
	r.releaseSelection(sel)

	// THE ASSERTION. Window 3 holds only its own real traffic. An extra slot
	// here means the abandon decremented window 3's count, manufacturing RPM
	// headroom that no request earned.
	if got, want := providerRemaining(t, r, now), rpm-realInWindow3; got != want {
		t.Errorf("after a stale abandon, window 3 has remaining=%d, want %d: the refund "+
			"for a charge made in a previous window decremented the current "+
			"window's count, manufacturing RPM headroom that no request earned", got, want)
	}

	// The cap must still be enforced at its real value in window 3.
	admitted := 0
	for i := 0; i < rpm+2; i++ {
		s, err := r.Select("flash", "", nil)
		if err != nil {
			break
		}
		admitted++
		r.Release(s, 0)
	}
	if want := rpm - realInWindow3; admitted != want {
		t.Errorf("window 3 admitted %d more requests, want exactly %d", admitted, want)
	}
}

// TestRepeatedStaleAbandonsCannotAccumulateHeadroom drives the bypass harder.
// A single stale refund costs one slot; if a caller can produce many, the
// headroom compounds and the limit stops binding entirely. This is the shape a
// retry or failover loop would produce.
func TestRepeatedStaleAbandonsCannotAccumulateHeadroom(t *testing.T) {
	const rpm = 5
	const window = time.Minute

	now := time.Unix(1000, 0)
	r := rpmRouter(t, rpm, &now)

	// Each round: charge a selection, roll the window, let ONE real request
	// re-establish the new window, and only then abandon the stale selection.
	// The real request is what makes the stale release dangerous, because it is
	// what gives the new window a live count to corrupt.
	for round := 0; round < 8; round++ {
		sel, err := r.Select("flash", "", nil)
		if err != nil {
			break
		}
		before := providerRemaining(t, r, now)

		now = now.Add(2 * window)

		// One real request in the fresh window, re-establishing its count.
		s, err := r.Select("flash", "", nil)
		if err != nil {
			break
		}
		r.Release(s, 0)
		afterReal := providerRemaining(t, r, now)

		// The abandon, after the window rolled over.
		r.releaseSelection(sel)

		if got := providerRemaining(t, r, now); got != afterReal {
			t.Errorf("round %d: an abandon across a window boundary changed headroom "+
				"from %d to %d: the refund decremented a window the charge never "+
				"contributed to", round, afterReal, got)
		}
		if before > rpm {
			t.Errorf("round %d: headroom %d exceeded the configured RPM of %d", round, before, rpm)
		}
	}

	// The cap must actually bind in the final window.
	admitted := 0
	for i := 0; i < rpm+3; i++ {
		s, err := r.Select("flash", "", nil)
		if err != nil {
			break
		}
		admitted++
		r.Release(s, 0)
	}
	if admitted > rpm {
		t.Errorf("admitted %d requests against a configured RPM of %d", admitted, rpm)
	}
	if admitted < 1 {
		t.Errorf("admitted %d requests in a fresh window, want at least 1: the "+
			"window is being starved", admitted)
	}
}

// TestAnAbandonInsideItsOwnWindowStillRefunds is the positive control. The fix
// must not break legitimate same-window refunds. An abandon inside the window of
// its own charge means the request was never sent upstream, so holding the slot
// would waste provider capacity and could starve the provider for the rest of
// the window. If this regressed, the guard would be too strict and the abandon
// path would silently leak capacity.
func TestAnAbandonInsideItsOwnWindowStillRefunds(t *testing.T) {
	const rpm = 2
	const window = time.Minute

	// The clock starts just before the end of its own window, and the abandon
	// happens a second later, INSIDE that window. This detail is load-bearing.
	//
	// The first version of this test used time.Unix(1000, 0) with a router whose
	// window is one minute, so both the Allow and the refund happened at t=1000
	// while the window ran to t=1060. FixedWindow.Remaining short-circuits to
	// "full headroom" whenever now is past resetAt, without consulting the count
	// at all, so the assertion read Limit and passed whether or not the refund
	// had actually been issued. Deleting the refund call from releaseSelection
	// entirely left this test green. The window is now positioned so the read
	// has to go through the count.
	now := time.Unix(1000, 0).Add(window - time.Second)
	r := rpmRouter(t, rpm, &now)

	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Fatalf("after one admission remaining=%d, want %d", got, rpm-1)
	}

	// Abandon one second later, still inside the same window: the slot must come
	// back.
	now = now.Add(time.Second)
	r.releaseSelection(sel)
	if got := providerRemaining(t, r, now); got != rpm {
		t.Errorf("remaining=%d after an in-window abandon, want the full %d: the "+
			"stale-refund guard also rejected legitimate same-window refunds, "+
			"leaking provider capacity", got, rpm)
	}
}

// TestAnAbandonReturnsOnlyTheSlotItCharged checks the refund is paired with its
// own Allow rather than with the window.
//
// The in-window test above proves a refund happens. It cannot prove the refund
// is the RIGHT one, because a refund that ignores its pairing would still restore
// a single slot. Here two requests are charged and only one is abandoned, so a
// refund that is not scoped to the selection it belongs to shows up as a count
// that is too high, which is the direction that breaks the cap rather than the
// one that merely wastes capacity.
func TestAnAbandonReturnsOnlyTheSlotItCharged(t *testing.T) {
	const rpm = 3
	const window = time.Minute

	now := time.Unix(1000, 0).Add(window - 2*time.Second)
	r := rpmRouter(t, rpm, &now)

	// Two admissions, both real and both kept.
	kept, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := providerRemaining(t, r, now); got != rpm-2 {
		t.Fatalf("after two admissions remaining=%d, want %d", got, rpm-2)
	}

	// Abandon the second one, still inside the window. Exactly one slot returns:
	// the one this selection charged.
	now = now.Add(time.Second)
	r.releaseSelection(abandoned)
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Errorf("remaining=%d after abandoning one of two admissions, want %d: the "+
			"refund did not correspond to the charge it was supposed to offset",
			got, rpm-1)
	}

	// And the kept request's slot is still spent, which is the assertion that
	// separates a paired refund from a blanket window reset.
	now = now.Add(time.Second)
	r.Release(kept, 0)
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Errorf("remaining=%d after releasing the completed request, want %d: a "+
			"completed request must keep its slot for the rest of the window",
			got, rpm-1)
	}
}

// TestHonestTrafficExactlyFillsTheCap checks the cap is neither too strict nor
// too loose in ordinary operation. Without this, a guard that blocked all
// refunds would pass the stale-refund tests while letting a provider take far
// more than rpm requests per window.
func TestHonestTrafficExactlyFillsTheCap(t *testing.T) {
	for _, rpm := range []int{1, 2, 7} {
		t.Run("", func(t *testing.T) {
			now := time.Unix(1000, 0)
			r := rpmRouter(t, rpm, &now)

			admitted := 0
			for i := 0; i < rpm+5; i++ {
				sel, err := r.Select("flash", "", nil)
				if err != nil {
					break
				}
				admitted++
				// A completed request keeps its provider slot for the window.
				r.Release(sel, 0)
			}
			if admitted != rpm {
				t.Errorf("with RPM %d, admitted %d requests before the cap bit", rpm, admitted)
			}
			// A new window restores exactly the configured cap.
			now = now.Add(2 * time.Minute)
			if got := providerRemaining(t, r, now); got != rpm {
				t.Errorf("with RPM %d, a fresh window has remaining=%d, want %d", rpm, got, rpm)
			}
		})
	}
}
