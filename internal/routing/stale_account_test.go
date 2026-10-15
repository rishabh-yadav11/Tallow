package routing

import (
	"testing"
	"time"
)

func TestALateSuccessDoesNotUndoFailover(t *testing.T) {
	r, _, advance := breakerFixture(t)

	inflight := selectP1(t, r)

	var fail *Selection
	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		fail = selectP1(t, r)
		r.Release(fail, 0)
		r.RecordFailure(fail)
	}

	sel, err := r.Select("flash", "sess-late", nil)
	if err != nil {
		t.Fatalf("select after failures: %v", err)
	}
	if sel.Provider != "p2" {
		t.Fatalf("after 3 failures Select chose %s, want p2 (failover)", sel.Provider)
	}

	advance(time.Second)
	r.Release(inflight, 0)
	r.RecordSuccess(inflight)

	sel, err = r.Select("flash", "sess-after", nil)
	if err != nil {
		t.Fatalf("select after the late success: %v", err)
	}
	if sel.Provider != "p2" {
		t.Errorf("after a late success Select chose %s, want p2: a stale success must not restore a failing provider", sel.Provider)
	}
}

func TestTheCooldownProbeCanStillCloseTheBreaker(t *testing.T) {
	r, _, advance := breakerFixture(t)

	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		fail := selectP1(t, r)
		r.Release(fail, 0)
		r.RecordFailure(fail)
	}

	advance(31 * time.Second)

	probe, err := r.Select("flash", "sess-probe", nil)
	if err != nil {
		t.Fatalf("select after cooldown: %v", err)
	}
	if probe.Provider != "p1" {
		t.Fatalf("after the cooldown Select chose %s, want p1 (the probe)", probe.Provider)
	}

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

func TestAStaleFailureDoesNotReopenARecoveredBreaker(t *testing.T) {
	r, _, advance := breakerFixture(t)

	var inflight []*Selection
	for i := 0; i < 3; i++ {
		inflight = append(inflight, selectP1(t, r))
	}

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

	r.Release(sel, 0)
}

func TestCurrentFailuresStillOpenTheBreaker(t *testing.T) {
	r, _, advance := breakerFixture(t)

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

func TestAStaleSuccessCannotWedgeAProviderOutForever(t *testing.T) {
	r, _, advance := breakerFixture(t)

	for i := 0; i < 3; i++ {
		advance(time.Millisecond)
		f, err := r.Select("flash", "sess", nil)
		if err != nil {
			t.Fatalf("opening select %d: %v", i, err)
		}
		r.Release(f, 0)
		r.RecordFailure(f)
	}

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
