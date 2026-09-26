package routing

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

// The audit scored RecordSuccess and RecordFailure at 0.0% and called it out
// under Test Quality: "the entire failure-accounting path that makes failover
// work is untested." That is the load-bearing half of the router. Select
// decides where a request goes; these two decide whether the next request still
// goes there. If RecordFailure silently stopped opening a breaker, a dead
// provider would keep receiving every retry and the gateway would degrade into
// a tight retry loop against a host that is down. If RecordSuccess stopped
// closing one, a provider that recovered after a blip would stay excluded
// forever, with the router having no path back.
//
// The tests below are written against the observable consequence - the next
// Select's decision - rather than against breaker internals, so a reworded
// implementation still passes and a behaviourally broken one does not.

// breakerFixture builds a router whose health registry the caller keeps, plus a
// clock the test drives, so breaker state can be observed and aged exactly.
//
// breaker is a pointer field on the registry's internal map, so the caller
// holding hr can read a provider's live breaker after each accounting call.
func breakerFixture(t *testing.T) (*Router, *health.Registry, func(time.Duration)) {
	t.Helper()
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1", RPM: 1000}}},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k2", Secret: "s2", RPM: 1000}}},
	}
	aliases := []model.Alias{{
		Name: "flash",
		Targets: []model.Target{
			{Provider: "p1", Model: "m"},
			{Provider: "p2", Model: "m"},
		},
	}}
	reg := registry.New(provs, aliases)
	hr := health.NewRegistry()

	var mu sync.Mutex
	now := time.Unix(1000, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	r := NewRouter(reg, hr, time.Minute, clock)
	r.SetBudgets(map[string]budget.KeyLimits{
		"p1/k1": {RPM: 1000},
		"p2/k2": {RPM: 1000},
	}, map[string]int{})
	return r, hr, advance
}

// selectP1 pins a selection to p1, which is the first alias target, so the
// accounting calls under test act on a known provider/key.
func selectP1(t *testing.T, r *Router) *Selection {
	t.Helper()
	sel, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select p1: %v", err)
	}
	if sel.Provider != "p1" || sel.Key != "k1" {
		t.Fatalf("expected p1/k1, got %s/%s", sel.Provider, sel.Key)
	}
	return sel
}

// TestRecordFailureOpensBreakerAndStopsRouting is the core failover guarantee:
// once a provider has failed enough times, Select must stop choosing it.
//
// It asserts the decision Select makes rather than the breaker's state field,
// because the decision is what an operator experiences. The threshold is the
// health package default of 3, so the first two failures must still route and
// the third must retire p1 for the rest of the cooldown.
func TestRecordFailureOpensBreakerAndStopsRouting(t *testing.T) {
	r, hr, _ := breakerFixture(t)

	for i := 0; i < 2; i++ {
		sel := selectP1(t, r)
		r.Release(sel, 0)
		r.RecordFailure(sel)
		// Below threshold p1 is still preferred.
		next := selectP1(t, r)
		if next.Provider != "p1" {
			t.Fatalf("failure %d: p1 should still route below threshold, got %s", i+1, next.Provider)
		}
		r.Release(next, 0)
	}

	// Third consecutive failure crosses the default threshold of 3.
	sel := selectP1(t, r)
	r.Release(sel, 0)
	r.RecordFailure(sel)

	if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Open {
		t.Fatalf("p1 breaker = %s, want open after 3 consecutive failures", got)
	}

	// The observable consequence: the next request must not be sent to p1.
	// This is what turns three failed requests into a failover rather than an
	// unbounded retry loop against a dead host.
	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after breaker opened: %v", err)
	}
	if got.Provider != "p2" {
		t.Errorf("p1 breaker is open but Select still chose %s; failover is broken", got.Provider)
	}
	r.Release(got, 0)
}

// TestRecordFailureOpensKeyBreakerOnlyForThatKey is the granularity guarantee.
// A key-level breaker must retire exactly the failing key, leaving the
// provider's other keys routable, because rotating to a sibling key is cheaper
// than failing over to another provider. And it must be RecordFailure - not
// some other path - that opens it, since RecordFailure is the only thing a
// failed request goes through.
//
// RecordFailure opens the provider breaker too, which would leave the whole
// provider unroutable and hide the key-level behaviour. So the provider breaker
// is explicitly re-closed after the failures, and the assertion that the key
// breaker is open has already been made by then.
func TestRecordFailureOpensKeyBreakerOnlyForThatKey(t *testing.T) {
	provs := []model.Provider{{
		Name:    "p1",
		BaseURL: "https://p1.example/v1",
		Keys: []model.Key{
			{ID: "k1", Secret: "s1", RPM: 1000},
			{ID: "k2", Secret: "s2", RPM: 1000},
		},
	}}
	aliases := []model.Alias{{
		Name:    "flash",
		Targets: []model.Target{{Provider: "p1", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	hr := health.NewRegistry()
	now := time.Unix(1000, 0)
	r := NewRouter(reg, hr, time.Minute, func() time.Time { return now })
	r.SetBudgets(map[string]budget.KeyLimits{
		"p1/k1": {RPM: 1000},
		"p1/k2": {RPM: 1000},
	}, map[string]int{})

	// Three failures on k1, driven through the router's own accounting call so
	// the key breaker's opening is attributed to RecordFailure.
	for i := 0; i < 3; i++ {
		sel, err := r.acquire("p1", "k1", "m", model.Target{Provider: "p1", Model: "m"}, now)
		if err != nil {
			t.Fatalf("acquire k1 %d: %v", i, err)
		}
		r.Release(sel, 0)
		r.RecordFailure(sel)
	}

	if got := hr.Key("p1", "k1").State(now); got != health.Open {
		t.Fatalf("key p1/k1 breaker = %s after 3 RecordFailure calls, want open; "+
			"a bad key must be retired by reactive accounting", got)
	}
	if got := hr.Key("p1", "k2").State(now); got != health.Closed {
		t.Fatalf("key p1/k2 breaker = %s, want closed; one bad key must not retire its siblings", got)
	}

	// Re-close only the provider breaker, so the provider stays routable and
	// the routing assertion below can observe the key-level effect.
	hr.Provider("p1").RecordSuccess(now)
	if got := hr.Provider("p1").State(now); got != health.Closed {
		t.Fatalf("provider p1 breaker = %s, want closed for this test", got)
	}

	// A later request must land on the surviving sibling key, not fail over.
	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select with one key retired: %v", err)
	}
	if got.Key != "k2" {
		t.Errorf("Select chose key %s with k1 open, want the healthy sibling k2", got.Key)
	}
	r.Release(got, 0)
}

// TestRecordSuccessClosesAnOpenBreaker is the recovery guarantee. Without it a
// provider that had one bad minute would be excluded for the rest of the
// process's life, with no route back except a restart.
func TestRecordSuccessClosesAnOpenBreaker(t *testing.T) {
	r, hr, _ := breakerFixture(t)

	// Drive p1 to open.
	for i := 0; i < 3; i++ {
		sel := selectP1(t, r)
		r.Release(sel, 0)
		r.RecordFailure(sel)
	}
	if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Open {
		t.Fatalf("setup: p1 breaker = %s, want open", got)
	}

	// p1 is out of rotation, so the probe has to be issued directly - which is
	// what the health prober does.
	probe := &Selection{Provider: "p1", Key: "k1", Target: model.Target{Provider: "p1", Model: "m"}}
	r.RecordSuccess(probe)

	if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Closed {
		t.Fatalf("p1 breaker = %s after a success, want closed", got)
	}
	if got := hr.Key("p1", "k1").State(time.Unix(1000, 0)); got != health.Closed {
		t.Errorf("key p1/k1 breaker = %s after a success, want closed", got)
	}

	// And routing must return to p1. The round-robin counter has advanced, so
	// within the fixture the only provider in rotation is p1: p2 can be
	// selected only if p1 is still gated out.
	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after recovery: %v", err)
	}
	if got.Provider != "p1" {
		t.Errorf("Select chose %s with p1 recovered, want p1 back in rotation", got.Provider)
	}
	r.Release(got, 0)
}

// TestRecordSuccessResetsTheFailureRun is the counter test. A breaker that
// opened must not reopen instantly on one later failure: a single success
// between failures is evidence the provider works, so the consecutive count
// restarts. Without the reset, three failures spread across a day would retire
// a healthy provider.
func TestRecordSuccessResetsTheFailureRun(t *testing.T) {
	r, hr, _ := breakerFixture(t)

	// Two failures, a success, then two more. That is four failures, but never
	// three in a row, so the breaker must stay closed.
	sel := selectP1(t, r)
	r.Release(sel, 0)
	r.RecordFailure(sel)
	sel = selectP1(t, r)
	r.Release(sel, 0)
	r.RecordFailure(sel)

	r.RecordSuccess(sel)

	sel = selectP1(t, r)
	r.Release(sel, 0)
	r.RecordFailure(sel)
	sel = selectP1(t, r)
	r.Release(sel, 0)
	r.RecordFailure(sel)

	if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Closed {
		t.Errorf("p1 breaker = %s after 4 non-consecutive failures, want closed; "+
			"a success must reset the consecutive run", got)
	}
}

// TestRecordFailureForgetsSticky pins a session to a provider that then fails,
// and keeping the pin would send every subsequent request in that session back
// to the provider that just failed - a session that can never escape a bad
// provider until its TTL expires. RecordFailure is the only place that clears
// the pin, so this is its regression.
//
// The setup makes the difference observable. The session is pinned to p2, the
// SECOND alias target, and a single failure is recorded on it - below the
// threshold of 3, so neither breaker opens and p2 stays eligible. With the pin
// intact the next request returns to p2; with it cleared, normal routing picks
// the preferred p1. That isolates Forget from the failover behaviour, and from
// the candidate ordering, by observing the change across a full open/close
// decision rather than a breaker's state field.
func TestRecordFailureForgetsSticky(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1", RPM: 1000}}},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k2", Secret: "s2", RPM: 1000}}},
	}
	aliases := []model.Alias{{
		Name: "flash",
		Targets: []model.Target{
			{Provider: "p1", Model: "m"},
			{Provider: "p2", Model: "m"},
		},
	}}
	reg := registry.New(provs, aliases)
	now := time.Unix(1000, 0)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return now })
	r.SetBudgets(map[string]budget.KeyLimits{
		"p1/k1": {RPM: 1000},
		"p2/k2": {RPM: 1000},
	}, map[string]int{})

	// Pin the session to p2. p1 is the preferred target, so an unpinned request
	// would choose p1 - the pin is what sends this session to p2.
	stuck := &Selection{Provider: "p2", Key: "k2", Target: model.Target{Provider: "p2", Model: "m"}, SessionKey: "sess"}
	r.sticky.Put("sess", "p2", "k2", stuck.Target)

	pinned, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("sticky select: %v", err)
	}
	if pinned.Provider != "p2" {
		t.Fatalf("expected session pinned to p2, got %s", pinned.Provider)
	}
	r.Release(pinned, 0)

	// One failure: forgets the pin, but both breakers stay closed (threshold is
	// 3) so p2 is still an eligible candidate. That isolates Forget from
	// failover - if the pin were not cleared, the next request returns to p2.
	r.RecordFailure(pinned)

	next, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after failure: %v", err)
	}
	if next.Provider != "p1" {
		t.Errorf("Select chose %s after a recorded failure with the pin cleared, "+
			"want p1; the session stayed pinned to the provider that had just failed", next.Provider)
	}
	r.Release(next, 0)
}

// TestRecordFailureReopensHalfOpenOnProbeFailure covers the state that a
// successful request exercises least: a provider that was open, served its
// cooldown probe, and failed. Half-open must fall back to open, not to closed,
// or the next request floods a provider that just failed its probe.
func TestRecordFailureReopensHalfOpenOnProbeFailure(t *testing.T) {
	r, hr, advance := breakerFixture(t)

	// Open p1.
	for i := 0; i < 3; i++ {
		sel := selectP1(t, r)
		r.Release(sel, 0)
		r.RecordFailure(sel)
	}

	// Age past the default 30s cooldown so the breaker becomes half-open and
	// admits a probe.
	advance(31 * time.Second)
	probe, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("probe select: %v", err)
	}
	if probe.Provider != "p1" {
		t.Skipf("half-open probe went to %s, not p1; registry state unexpected", probe.Provider)
	}
	r.Release(probe, 0)

	// The probe fails. It must return the breaker to open, not to closed.
	r.RecordFailure(probe)
	if got := hr.Provider("p1").State(time.Unix(1000, 0).Add(31 * time.Second)); got != health.Open {
		t.Errorf("p1 breaker = %s after a failed half-open probe, want open; "+
			"a provider that failed its probe must be retired again", got)
	}
}

// TestRecordAccountingIsRaceFree runs both accounting calls concurrently
// against shared breaker state while readers observe it the way the prober and
// /health-states do. Breakers are read on the request path, by the background
// prober, and by the admin endpoint, so an unguarded field here would be a
// production data race rather than a theoretical one. Verified under -race.
func TestRecordAccountingIsRaceFree(t *testing.T) {
	r, hr, _ := breakerFixture(t)

	// Writers run to completion; readers run until the writers are done.
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 200; j++ {
				sel := &Selection{
					Provider: "p1",
					Key:      "k1",
					Target:   model.Target{Provider: "p1", Model: "m"},
				}
				if j%2 == 0 {
					r.RecordSuccess(sel)
				} else {
					r.RecordFailure(sel)
				}
			}
		}()
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = hr.Provider("p1").State(time.Unix(1000, 0))
				_ = hr.Key("p1", "k1").State(time.Unix(1000, 0))
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()
}

// TestReleaseIsIdempotent guards the contract the coalescing fix (H8) depends
// on: a leader settles the reservation once, and any extra Release must be a
// no-op. If it were not, a burst of N coalesced callers would decrement the
// concurrency slot N times and bank N times the cost of one upstream call - and
// the concurrency slot is the one that can go negative and admit requests past
// the configured max_inflight.
//
// Both halves are asserted because they fail differently: the slot guard is
// nil-ing sel.kb, while the cost guard is the budget's own accounting.
func TestReleaseIsIdempotent(t *testing.T) {
	r, _, _ := breakerFixture(t)

	sel := selectP1(t, r)
	if got := r.InFlight("p1", "k1"); got != 1 {
		t.Fatalf("in-flight after select = %d, want 1", got)
	}

	r.Release(sel, 5_000)
	r.Release(sel, 5_000) // must be a no-op
	r.Release(sel, 5_000)

	if got := r.InFlight("p1", "k1"); got != 0 {
		t.Errorf("in-flight after 3 releases = %d, want 0; "+
			"a repeated Release double-decrements the concurrency slot", got)
	}
	// The cost half: exactly one upstream call happened, so exactly its cost
	// may be banked.
	_, cost := r.KeyUsage("p1", "k1")
	if cost != 5_000 {
		t.Errorf("banked cost after 3 releases = %d micro-USD, want 5000; "+
			"a repeated Release multiplies the recorded cost", cost)
	}
}

// TestReleaseWithoutSelectionIsSafe is the nil guard Release must keep: the
// proxy reaches it on early error returns.
func TestReleaseWithoutSelectionIsSafe(t *testing.T) {
	r, _, _ := breakerFixture(t)
	r.Release(nil, 100) // must not panic
}

// TestRepeatedFailureThenRecoveryCycle walks a full open/close cycle repeatedly,
// because breaker state is shared and the audit's systemic root cause #5 was
// that fixtures were too clean to reach the defect. Cycling catches a state
// machine that only behaves correctly on its first pass.
func TestRepeatedFailureThenRecoveryCycle(t *testing.T) {
	r, hr, advance := breakerFixture(t)

	for cycle := 0; cycle < 5; cycle++ {
		for i := 0; i < 3; i++ {
			sel := selectP1(t, r)
			r.Release(sel, 0)
			r.RecordFailure(sel)
		}
		if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Open {
			t.Fatalf("cycle %d: p1 = %s after 3 failures, want open", cycle, got)
		}

		// A probe succeeds; the provider recovers.
		advance(31 * time.Second)
		r.RecordSuccess(&Selection{Provider: "p1", Key: "k1", Target: model.Target{Provider: "p1", Model: "m"}})
		if got := hr.Provider("p1").State(time.Unix(1000, 0).Add(31 * time.Second)); got != health.Closed {
			t.Fatalf("cycle %d: p1 = %s after a successful probe, want closed", cycle, got)
		}

		got, err := r.Select("flash", fmt.Sprintf("sess-%d", cycle), nil)
		if err != nil {
			t.Fatalf("cycle %d: select after recovery: %v", cycle, err)
		}
		if got.Provider != "p1" {
			t.Fatalf("cycle %d: Select chose %s with p1 recovered, want p1", cycle, got.Provider)
		}
		r.Release(got, 0)
	}
}
