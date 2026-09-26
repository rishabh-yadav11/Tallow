package routing

import (
	"testing"
	"time"
)

// The breaker is shared by every request in flight against a provider, and
// RecordSuccess / RecordFailure are called after the upstream call returns. That
// gap is where a stale account comes from: a request admitted while the
// breaker was closed reports back after a burst has opened it.
//
// These tests assert the decision Select makes, not the breaker's state field,
// because the routing decision is what an operator experiences. They are the
// reachable proof that the staleness rule in internal/health changes behavior
// here and not only in isolation.

// TestALateSuccessDoesNotUndoFailover is the user-visible consequence of the
// staleness rule.
//
// A provider fails three times, so Select starts failing over to p2. Then a
// request that was admitted before those failures completes successfully - a
// slow upstream, not a recovery - and reports success. The provider is still
// failing. Routing must stay off it.
//
// Before the fix this was not the case: the success closed the breaker and
// Select went straight back to the provider that had just failed three times,
// with no cooldown and no evidence. A provider under sustained failure could be
// returned to by any one in-flight request, which is the shape of a flap.
func TestALateSuccessDoesNotUndoFailover(t *testing.T) {
	r, _, advance := breakerFixture(t)

	// A long request is admitted now and is still upstream the whole time.
	// It is the one whose late success must not count.
	inflight := selectP1(t, r)

	// Three SEPARATE requests are admitted and fail, one after another. Each
	// carries its own admission stamp, so the third - the one that opens the
	// breaker - is stamped later than inflight.
	var fail *Selection
	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		fail = selectP1(t, r)
		r.Release(fail, 0)
		r.RecordFailure(fail)
	}

	// Routing now fails over.
	sel, err := r.Select("flash", "sess-late", nil)
	if err != nil {
		t.Fatalf("select after failures: %v", err)
	}
	if sel.Provider != "p2" {
		t.Fatalf("after 3 failures Select chose %s, want p2 (failover)", sel.Provider)
	}

	// The long request finally completes successfully, long after the burst.
	// Its account is stamped with when it was admitted, which predates the
	// opening.
	advance(time.Second)
	r.Release(inflight, 0)
	r.RecordSuccess(inflight)

	// The provider is still failing; routing must not go back.
	sel, err = r.Select("flash", "sess-after", nil)
	if err != nil {
		t.Fatalf("select after the late success: %v", err)
	}
	if sel.Provider != "p2" {
		t.Errorf("after a late success Select chose %s, want p2: a stale success must not restore a failing provider", sel.Provider)
	}
}

// TestTheCooldownProbeCanStillCloseTheBreaker is the counterweight, and the one
// that keeps the fix from stranding traffic on a dead provider.
//
// Once the cooldown elapses, a request is admitted against p1. If that request
// succeeds, the provider has genuinely recovered and must be routed to again.
// The staleness rule must not reject this, because the account arrives after the
// opening rather than before it.
func TestTheCooldownProbeCanStillCloseTheBreaker(t *testing.T) {
	r, _, advance := breakerFixture(t)

	// Three separate failing requests open the breaker.
	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		fail := selectP1(t, r)
		r.Release(fail, 0)
		r.RecordFailure(fail)
	}

	// The cooldown elapses and p1 is probed. Advance well past the health
	// package's 30s default.
	advance(31 * time.Second)

	probe, err := r.Select("flash", "sess-probe", nil)
	if err != nil {
		t.Fatalf("select after cooldown: %v", err)
	}
	if probe.Provider != "p1" {
		t.Fatalf("after the cooldown Select chose %s, want p1 (the probe)", probe.Provider)
	}

	// The probe succeeds: p1 has recovered.
	r.Release(probe, 0)
	r.RecordSuccess(probe)

	sel, err := r.Select("flash", "sess-recovered", nil)
	if err != nil {
		t.Fatalf("select after recovery: %v", err)
	}
	if sel.Provider != "p1" {
		t.Errorf("after a successful probe Select chose %s, want p1: a real recovery must be honored", sel.Provider)
	}
}

// TestAStaleFailureDoesNotReopenARecoveredBreaker covers the failure side at
// the routing layer.
//
// p1 fails three times, the cooldown elapses, and a probe succeeds - p1 is
// healthy again. A request that was admitted before the failures then reports
// its own failure. That failure predates the recovery and must not count.
//
// The test deliberately drives enough stale failures to cross the threshold on
// their own. A single stale failure is indistinguishable from a fresh one,
// because one failure never opens a 3-failure breaker either way; only a count
// that would exceed the threshold on stale evidence alone can show that the
// staleness rule is doing anything.
func TestAStaleFailureDoesNotReopenARecoveredBreaker(t *testing.T) {
	r, _, advance := breakerFixture(t)

	// Three requests admitted before the failures, all still upstream.
	var inflight []*Selection
	for i := 0; i < 3; i++ {
		inflight = append(inflight, selectP1(t, r))
	}

	// Three separate failing requests open the breaker.
	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		fail := selectP1(t, r)
		r.Release(fail, 0)
		r.RecordFailure(fail)
	}
	advance(31 * time.Second)

	probe, err := r.Select("flash", "sess-probe2", nil)
	if err != nil {
		t.Fatalf("select after cooldown: %v", err)
	}
	if probe.Provider != "p1" {
		t.Fatalf("probe went to %s, want p1", probe.Provider)
	}
	r.Release(probe, 0)
	r.RecordSuccess(probe)

	// p1 recovered. The three old in-flight requests now report failures.
	// Each predates the recovery, so together they must not push p1 back out
	// of rotation: on stale evidence alone they exceed the threshold.
	for _, sel := range inflight {
		r.Release(sel, 0)
		r.RecordFailure(sel)
	}

	sel, err := r.Select("flash", "sess-still-ok", nil)
	if err != nil {
		t.Fatalf("select after the stale failures: %v", err)
	}
	if sel.Provider != "p1" {
		t.Errorf("after 3 stale failures Select chose %s, want p1: stale failures must not evict a recovered provider", sel.Provider)
	}
	// Release it, or its in-flight slot and RPM stay consumed and the next
	// Select cannot use p1.
	r.Release(sel, 0)
}

// TestCurrentFailuresStillOpenTheBreaker is the counterweight to the staleness
// rule, in the direction that matters operationally: rejecting stale evidence
// must not also reject current evidence, or a genuinely broken provider would
// never be taken out of rotation and failover would silently stop working.
//
// No stale account appears anywhere in this test. Every failure is accounted by
// a request admitted at the time it failed, so the breaker must open exactly as
// it did before the staleness rule existed.
func TestCurrentFailuresStillOpenTheBreaker(t *testing.T) {
	r, _, advance := breakerFixture(t)

	// Three current failures, each from its own freshly admitted request.
	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		fresh, err := r.Select("flash", "sess", nil)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if fresh.Provider != "p1" {
			t.Fatalf("request %d went to %s, want p1", i, fresh.Provider)
		}
		r.Release(fresh, 0)
		r.RecordFailure(fresh)
	}

	sel, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after failures: %v", err)
	}
	if sel.Provider != "p2" {
		t.Errorf("after 3 current failures Select chose %s, want p2: the breaker must open on current evidence", sel.Provider)
	}
}

// TestAStaleSuccessCannotWedgeAProviderOutForever checks the other direction of
// over-rejection. A stale failure is discarded, and a provider that is
// genuinely broken keeps failing with current evidence, so it must be able to
// open again and stay open. Staleness must not accumulate a partial count that
// never reaches the threshold.
func TestAStaleSuccessCannotWedgeAProviderOutForever(t *testing.T) {
	r, _, advance := breakerFixture(t)

	// Open the breaker with current failures.
	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		f, err := r.Select("flash", "sess", nil)
		if err != nil {
			t.Fatalf("opening select %d: %v", i, err)
		}
		r.Release(f, 0)
		r.RecordFailure(f)
	}

	// Let the cooldown elapse and probe. The probe succeeds, so p1 is closed.
	advance(31 * time.Second)
	probe, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("probe select: %v", err)
	}
	if probe.Provider != "p1" {
		t.Fatalf("probe went to %s, want p1", probe.Provider)
	}
	r.Release(probe, 0)
	r.RecordSuccess(probe)

	// Now three more current failures, with no stale account in the picture.
	// They must open the breaker on their own.
	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		f, err := r.Select("flash", "sess", nil)
		if err != nil {
			t.Fatalf("second-round select %d: %v", i, err)
		}
		if f.Provider != "p1" {
			t.Fatalf("second-round request %d went to %s, want p1", i, f.Provider)
		}
		r.Release(f, 0)
		r.RecordFailure(f)
	}

	sel, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after second round: %v", err)
	}
	if sel.Provider != "p2" {
		t.Errorf("Select chose %s, want p2: a recovered-then-refailing provider must open again", sel.Provider)
	}
}
