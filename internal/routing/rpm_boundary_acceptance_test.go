package routing

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

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

func TestAbandonedCandidatesCannotManufactRPMAcrossAWindowBoundary(t *testing.T) {
	const rpm = 3
	const window = time.Minute

	now := time.Unix(1000, 0)
	r := rpmRouter(t, rpm, &now)

	for i := 0; i < rpm; i++ {
		sel, err := r.Select("flash", "", nil)
		if err != nil {
			t.Fatalf("window 1 request %d: %v", i, err)
		}

		r.Release(sel, 0)
	}
	if got := providerRemaining(t, r, now); got != 0 {
		t.Fatalf("window 1: remaining=%d, want 0 after %d requests", got, rpm)
	}

	if _, err := r.Select("flash", "", nil); err == nil {
		t.Fatalf("window 1: provider RPM %d was exceeded: a %dth request was admitted", rpm, rpm+1)
	}

	now = now.Add(2 * window)
	if got := providerRemaining(t, r, now); got != rpm {
		t.Fatalf("window 2 starts with remaining=%d, want the full %d", got, rpm)
	}

	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatalf("window 2: %v", err)
	}
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Fatalf("window 2: after one admission remaining=%d, want %d", got, rpm-1)
	}

	now = now.Add(2 * window)
	if got := providerRemaining(t, r, now); got != rpm {
		t.Fatalf("window 3 starts with remaining=%d, want the full %d", got, rpm)
	}

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

	r.releaseSelection(sel)

	if got, want := providerRemaining(t, r, now), rpm-realInWindow3; got != want {
		t.Errorf("after a stale abandon, window 3 has remaining=%d, want %d: the refund "+
			"for a charge made in a previous window decremented the current "+
			"window's count, manufacturing RPM headroom that no request earned", got, want)
	}

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

func TestRepeatedStaleAbandonsCannotAccumulateHeadroom(t *testing.T) {
	const rpm = 5
	const window = time.Minute

	now := time.Unix(1000, 0)
	r := rpmRouter(t, rpm, &now)

	for round := 0; round < 8; round++ {
		sel, err := r.Select("flash", "", nil)
		if err != nil {
			break
		}
		before := providerRemaining(t, r, now)

		now = now.Add(2 * window)

		s, err := r.Select("flash", "", nil)
		if err != nil {
			break
		}
		r.Release(s, 0)
		afterReal := providerRemaining(t, r, now)

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

func TestAnAbandonInsideItsOwnWindowStillRefunds(t *testing.T) {
	const rpm = 2
	const window = time.Minute

	now := time.Unix(1000, 0).Add(window - time.Second)
	r := rpmRouter(t, rpm, &now)

	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Fatalf("after one admission remaining=%d, want %d", got, rpm-1)
	}

	now = now.Add(time.Second)
	r.releaseSelection(sel)
	if got := providerRemaining(t, r, now); got != rpm {
		t.Errorf("remaining=%d after an in-window abandon, want the full %d: the "+
			"stale-refund guard also rejected legitimate same-window refunds, "+
			"leaking provider capacity", got, rpm)
	}
}

func TestAnAbandonReturnsOnlyTheSlotItCharged(t *testing.T) {
	const rpm = 3
	const window = time.Minute

	now := time.Unix(1000, 0).Add(window - 2*time.Second)
	r := rpmRouter(t, rpm, &now)

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

	now = now.Add(time.Second)
	r.releaseSelection(abandoned)
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Errorf("remaining=%d after abandoning one of two admissions, want %d: the "+
			"refund did not correspond to the charge it was supposed to offset",
			got, rpm-1)
	}

	now = now.Add(time.Second)
	r.Release(kept, 0)
	if got := providerRemaining(t, r, now); got != rpm-1 {
		t.Errorf("remaining=%d after releasing the completed request, want %d: a "+
			"completed request must keep its slot for the rest of the window",
			got, rpm-1)
	}
}

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

				r.Release(sel, 0)
			}
			if admitted != rpm {
				t.Errorf("with RPM %d, admitted %d requests before the cap bit", rpm, admitted)
			}

			now = now.Add(2 * time.Minute)
			if got := providerRemaining(t, r, now); got != rpm {
				t.Errorf("with RPM %d, a fresh window has remaining=%d, want %d", rpm, got, rpm)
			}
		})
	}
}
