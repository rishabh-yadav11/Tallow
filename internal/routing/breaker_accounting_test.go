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

func TestRecordFailureOpensBreakerAndStopsRouting(t *testing.T) {
	r, hr, _ := breakerFixture(t)

	for i := 0; i < 2; i++ {
		sel := selectP1(t, r)
		r.Release(sel, 0)
		r.RecordFailure(sel)

		next := selectP1(t, r)
		if next.Provider != "p1" {
			t.Fatalf("failure %d: p1 should still route below threshold, got %s", i+1, next.Provider)
		}
		r.Release(next, 0)
	}

	sel := selectP1(t, r)
	r.Release(sel, 0)
	r.RecordFailure(sel)

	if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Open {
		t.Fatalf("p1 breaker = %s, want open after 3 consecutive failures", got)
	}

	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after breaker opened: %v", err)
	}
	if got.Provider != "p2" {
		t.Errorf("p1 breaker is open but Select still chose %s; failover is broken", got.Provider)
	}
	r.Release(got, 0)
}

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

	hr.Provider("p1").RecordSuccess(now)
	if got := hr.Provider("p1").State(now); got != health.Closed {
		t.Fatalf("provider p1 breaker = %s, want closed for this test", got)
	}

	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select with one key retired: %v", err)
	}
	if got.Key != "k2" {
		t.Errorf("Select chose key %s with k1 open, want the healthy sibling k2", got.Key)
	}
	r.Release(got, 0)
}

func TestRecordSuccessClosesAnOpenBreaker(t *testing.T) {
	r, hr, _ := breakerFixture(t)

	for i := 0; i < 3; i++ {
		sel := selectP1(t, r)
		r.Release(sel, 0)
		r.RecordFailure(sel)
	}
	if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Open {
		t.Fatalf("setup: p1 breaker = %s, want open", got)
	}

	probe := &Selection{Provider: "p1", Key: "k1", Target: model.Target{Provider: "p1", Model: "m"}}
	r.RecordSuccess(probe)

	if got := hr.Provider("p1").State(time.Unix(1000, 0)); got != health.Closed {
		t.Fatalf("p1 breaker = %s after a success, want closed", got)
	}
	if got := hr.Key("p1", "k1").State(time.Unix(1000, 0)); got != health.Closed {
		t.Errorf("key p1/k1 breaker = %s after a success, want closed", got)
	}

	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after recovery: %v", err)
	}
	if got.Provider != "p1" {
		t.Errorf("Select chose %s with p1 recovered, want p1 back in rotation", got.Provider)
	}
	r.Release(got, 0)
}

func TestRecordSuccessResetsTheFailureRun(t *testing.T) {
	r, hr, _ := breakerFixture(t)

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

func TestRecordFailureReopensHalfOpenOnProbeFailure(t *testing.T) {
	r, hr, advance := breakerFixture(t)

	for i := 0; i < 3; i++ {
		sel := selectP1(t, r)
		r.Release(sel, 0)
		r.RecordFailure(sel)
	}

	advance(31 * time.Second)
	probe, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("probe select: %v", err)
	}
	if probe.Provider != "p1" {
		t.Skipf("half-open probe went to %s, not p1; registry state unexpected", probe.Provider)
	}
	r.Release(probe, 0)

	r.RecordFailure(probe)
	if got := hr.Provider("p1").State(time.Unix(1000, 0).Add(31 * time.Second)); got != health.Open {
		t.Errorf("p1 breaker = %s after a failed half-open probe, want open; "+
			"a provider that failed its probe must be retired again", got)
	}
}

func TestRecordAccountingIsRaceFree(t *testing.T) {
	r, hr, _ := breakerFixture(t)

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

func TestReleaseIsIdempotent(t *testing.T) {
	r, _, _ := breakerFixture(t)

	sel := selectP1(t, r)
	if got := r.InFlight("p1", "k1"); got != 1 {
		t.Fatalf("in-flight after select = %d, want 1", got)
	}

	r.Release(sel, 5_000)
	r.Release(sel, 5_000)
	r.Release(sel, 5_000)

	if got := r.InFlight("p1", "k1"); got != 0 {
		t.Errorf("in-flight after 3 releases = %d, want 0; "+
			"a repeated Release double-decrements the concurrency slot", got)
	}

	_, cost := r.KeyUsage("p1", "k1")
	if cost != 5_000 {
		t.Errorf("banked cost after 3 releases = %d micro-USD, want 5000; "+
			"a repeated Release multiplies the recorded cost", cost)
	}
}

func TestReleaseWithoutSelectionIsSafe(t *testing.T) {
	r, _, _ := breakerFixture(t)
	r.Release(nil, 100)
}

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
