package health

import (
	"sync"
	"testing"
	"time"
)

// at is a fixed clock, so cooldown behavior is asserted exactly rather than
// through sleeps. Times are chosen to sit unambiguously on one side of the
// cooldown boundary.
func at(sec int64) time.Time { return time.Unix(sec, 0) }

const (
	cooldown        = 30 * time.Second
	defaultCooldown = 30 * time.Second
)

// TestRecordSuccessInHalfOpenClearsTheProbeFlag is the H1 fix, from the routing
// side.
//
// A single recovery probe closes the breaker. Until that probe returns, the
// breaker is half-open and refuses every other request, which is correct. But
// when the probe succeeds, the breaker must become fully usable again in the
// SAME state transition. Instead, Allow moved Open -> HalfOpen and a success
// set state = Closed, so the HalfOpen value was never left and a probe-admission
// flag was needed to reproduce the "one probe at a time" rule.
//
// That flag is what leaked: requests that had already passed Allow() and were
// upstream in flight at the moment the breaker opened were never accounted for.
// Their later RecordSuccess ran unconditionally, so one late success closed a
// breaker that was open on N >= threshold real failures. Late successes are the
// common case, not an edge case: a request admitted before a burst can land
// after it.
//
// The invariant that makes both cases correct: any account arriving after the
// most recent transition to Open is stale and must not close the breaker.
func TestRecordSuccessInHalfOpenClearsTheProbeFlag(t *testing.T) {
	b := NewBreaker(3, cooldown)
	open := at(1000)

	// Three failures open it.
	for i := 0; i < 3; i++ {
		b.RecordFailure(open)
	}
	if got := b.State(open); got != Open {
		t.Fatalf("after 3 failures state = %v, want Open", got)
	}

	// Cooldown elapses; the next request becomes the probe.
	if !b.Allow(open.Add(cooldown)) {
		t.Fatal("Allow after cooldown = false, want true (probe admitted)")
	}
	if got := b.State(open.Add(cooldown)); got != HalfOpen {
		t.Fatalf("state after probe admitted = %v, want HalfOpen", got)
	}
	// And only that one probe.
	if b.Allow(open.Add(cooldown)) {
		t.Error("second Allow during half-open = true, want false")
	}

	// The probe succeeds: fully usable again, and admitting many.
	b.RecordSuccess(open.Add(cooldown))
	if got := b.State(open.Add(cooldown)); got != Closed {
		t.Errorf("state after successful probe = %v, want Closed", got)
	}
	if !b.Allow(open.Add(cooldown + 1)) {
		t.Error("Allow after a successful probe = false, want true")
	}
}

func TestALateSuccessCannotCloseAnOpenBreaker(t *testing.T) {
	b := NewBreaker(3, cooldown)

	// A request is admitted while the breaker is still closed, and stamps
	// itself then. Routing records account at the moment the request reports
	// back, so this stamp is the admission time standing in for "before the
	// burst". A slow upstream completes the request afterwards, but the
	// account still carries the older stamp.
	admittedAt := at(1000)

	// The burst then opens the breaker.
	for i := 0; i < 3; i++ {
		b.RecordFailure(at(1001))
	}
	if got := b.State(at(1001)); got != Open {
		t.Fatalf("after 3 failures state = %v, want Open", got)
	}

	// The slow request reports success, carrying its admission stamp.
	b.RecordSuccess(admittedAt)

	if got := b.State(at(1001)); got != Open {
		t.Errorf("state = %v, want Open: a late success must not close the breaker", got)
	}
	if b.Allow(at(1001)) {
		t.Error("Allow = true, want false: the breaker must still be open")
	}
}

// TestARealSuccessStillClosesTheBreaker is the counterweight. Epoch 0 is the
// zero time, so a breaker that has never opened has openedAt == 0 and no
// timestamped account can be older than it. Staleness therefore cannot reject a
// genuine success, and the accounting that closes breakers still works.
func TestARealSuccessStillClosesTheBreaker(t *testing.T) {
	b := NewBreaker(3, cooldown)

	// One failure does not open a 3-failure breaker, so this is a success
	// accounted at a timestamp strictly greater than any possible openedAt.
	b.RecordFailure(at(1000))
	b.RecordSuccess(at(1000))

	if got := b.State(at(1000)); got != Closed {
		t.Errorf("state = %v, want Closed: a real success must still close the breaker", got)
	}
	if !b.Allow(at(1000)) {
		t.Error("Allow = false, want true")
	}
}

// TestAHalfOpenFailureReopensImmediately pins the recovery path: a probe that
// fails must not leave the breaker half-open, where a subsequent success could
// close it as though the provider had recovered.
func TestAHalfOpenFailureReopensImmediately(t *testing.T) {
	b := NewBreaker(3, cooldown)
	open := at(1000)

	for i := 0; i < 3; i++ {
		b.RecordFailure(open)
	}
	probe := open.Add(cooldown)
	if !b.Allow(probe) {
		t.Fatal("Allow after cooldown = false, want true")
	}
	b.RecordFailure(probe)

	if got := b.State(probe); got != Open {
		t.Fatalf("state after failed probe = %v, want Open", got)
	}
	// The next cooldown is measured from the failed probe, not from the
	// original opening.
	if b.Allow(probe.Add(cooldown - time.Second)) {
		t.Error("Allow before the new cooldown elapsed = true, want false")
	}
	if !b.Allow(probe.Add(cooldown)) {
		t.Error("Allow after the new cooldown elapsed = false, want true")
	}
}

// TestForceOpenIsNotClosedByALateSuccess is the same staleness rule applied to
// the proactive path. A probe failure marks a provider unhealthy immediately,
// bypassing the threshold. A request that was already in flight when the probe
// result landed must not undo it.
func TestForceOpenIsNotClosedByALateSuccess(t *testing.T) {
	b := NewBreaker(3, cooldown)

	// The in-flight request was admitted before the probe reported.
	b.RecordSuccess(at(1000))
	b.ForceOpen(at(1001))

	if got := b.State(at(1001)); got != Open {
		t.Errorf("state = %v, want Open: a late success must not undo a proactive open", got)
	}
}

// TestStateDoesNotMutate covers a subtlety the API invites: State reports
// half-open for an elapsed breaker but is documented read-only. If a caller
// used it to admit traffic, it would have to mutate, and the audit's refuted
// "half-open probe storm" claim depends on Allow being the only door.
func TestStateDoesNotMutate(t *testing.T) {
	b := NewBreaker(3, cooldown)
	b.ForceOpen(at(1000))

	after := at(1000).Add(cooldown)
	// Two reads, to show repeated inspection does not spend the probe.
	if got := b.State(after); got != HalfOpen {
		t.Fatalf("State = %v, want HalfOpen", got)
	}
	if got := b.State(after); got != HalfOpen {
		t.Fatalf("second State = %v, want HalfOpen", got)
	}
	// Allow still owns admitting the probe, and admits exactly one. State is
	// read-only, so both of these cannot be true unless Allow is unchanged.
	if !b.Allow(after) {
		t.Error("Allow after two State reads = false, want true: State must not admit a probe")
	}
	if b.Allow(after) {
		t.Error("second Allow = true, want false")
	}
}

// TestNewBreakerDefaults pins the secure defaults for a misconfigured or
// absent threshold and cooldown. Both must resolve to a breaker that still
// opens and still cools down, never to one that is disabled.
func TestNewBreakerDefaults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int
		cooldown  time.Duration
	}{
		{"zero", 0, 0},
		{"negative", -1, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBreaker(tc.threshold, tc.cooldown)
			now := at(1000)
			for i := 0; i < 3; i++ {
				b.RecordFailure(now)
			}
			if got := b.State(now); got != Open {
				t.Fatalf("state = %v, want Open: defaults must still open the breaker", got)
			}
			if b.Allow(now) {
				t.Error("Allow = true, want false immediately after opening")
			}
			// One second before the 30s default cooldown, still refusing.
			if b.Allow(now.Add(defaultCooldown - time.Second)) {
				t.Error("Allow one second before the default cooldown = true, want false")
			}
			// And at the default cooldown, the probe is admitted.
			if !b.Allow(now.Add(defaultCooldown)) {
				t.Error("Allow at the default cooldown = false, want true")
			}
		})
	}
}

// TestDefaultCooldownIsThirtySeconds pins the cooldown fallback as an exact
// number, from both sides of the boundary.
//
// Defaulting to 5s instead would still cool down, so an assertion that only
// checks "a probe is eventually admitted" passes either way. The number is the
// behavior: 30s bounds how long a failing provider is excluded, and an operator
// debugging a failover has to be able to state it. A default that silently
// changed would change how quickly traffic returns to a recovered provider
// without appearing anywhere in configuration.
func TestDefaultCooldownIsThirtySeconds(t *testing.T) {
	for _, in := range []time.Duration{0, -time.Second} {
		b := NewBreaker(3, in)
		b.ForceOpen(at(1000))

		if b.Allow(at(1000).Add(29 * time.Second)) {
			t.Errorf("cooldown input %v: Allow after 29s = true, want false", in)
		}
		if !b.Allow(at(1000).Add(30 * time.Second)) {
			t.Errorf("cooldown input %v: Allow after 30s = false, want true", in)
		}
		if b.Allow(at(1000).Add(31 * time.Second)) {
			t.Errorf("cooldown input %v: Allow after 31s = true, want false (the probe is already out)", in)
		}
	}
}

// TestDefaultThresholdIsThree pins the default threshold from both sides, not
// just that the breaker eventually opens.
//
// The fallback exists for a threshold of zero or less. Resolving that to 1
// instead of 3 would still open the breaker, so an assertion only on "does it
// open" passes either way - and the consequence is severe and quiet: every
// single transient upstream blip trips the breaker, routing fails over, and
// failover itself becomes the outage. The default has to be asserted as an
// exact number, from both sides of the boundary.
func TestDefaultThresholdIsThree(t *testing.T) {
	for _, threshold := range []int{0, -1, -100} {
		b := NewBreaker(threshold, cooldown)
		now := at(1000)

		b.RecordFailure(now)
		if got := b.State(now); got != Closed {
			t.Errorf("threshold %d: after 1 failure state = %v, want Closed", threshold, got)
		}
		b.RecordFailure(now)
		if got := b.State(now); got != Closed {
			t.Errorf("threshold %d: after 2 failures state = %v, want Closed", threshold, got)
		}
		b.RecordFailure(now)
		if got := b.State(now); got != Open {
			t.Errorf("threshold %d: after 3 failures state = %v, want Open", threshold, got)
		}
	}
}

// TestAnExplicitThresholdIsHonored is the counterweight: the default must not
// override a configured value.
func TestAnExplicitThresholdIsHonored(t *testing.T) {
	now := at(1000)

	b := NewBreaker(1, cooldown)
	b.RecordFailure(now)
	if got := b.State(now); got != Open {
		t.Errorf("threshold 1: after 1 failure state = %v, want Open", got)
	}

	b2 := NewBreaker(5, cooldown)
	for i := 0; i < 4; i++ {
		b2.RecordFailure(now)
	}
	if got := b2.State(now); got != Closed {
		t.Errorf("threshold 5: after 4 failures state = %v, want Closed", got)
	}
	b2.RecordFailure(now)
	if got := b2.State(now); got != Open {
		t.Errorf("threshold 5: after 5 failures state = %v, want Open", got)
	}
}

// TestRecordSuccessAfterAClosedBreakerIsFresh is the property that makes the
// staleness rule safe, stated as a test rather than only in a comment.
//
// Every state a success can legitimately close is enumerated, because a
// staleness check that misfires in any of them would strand traffic on a dead
// provider. The rule is "a success is stale only if it predates the current
// opening", so a breaker that has never opened can never reject one.
func TestRecordSuccessAfterAClosedBreakerIsFresh(t *testing.T) {
	now := at(1000)

	// Never opened.
	b := NewBreaker(3, cooldown)
	b.RecordSuccess(now)
	if !b.Allow(now) {
		t.Error("a success on a never-opened breaker left it refusing traffic")
	}

	// Opened, then a probe closed it; a later success must still be accepted.
	b2 := NewBreaker(3, cooldown)
	for i := 0; i < 3; i++ {
		b2.RecordFailure(now)
	}
	b2.RecordSuccess(now.Add(cooldown))
	if !b2.Allow(now.Add(cooldown)) {
		t.Error("a success after recovery left the breaker refusing traffic")
	}
	// And it must be able to open again afterwards, not be wedged closed.
	for i := 0; i < 3; i++ {
		b2.RecordFailure(now.Add(cooldown))
	}
	if got := b2.State(now.Add(cooldown)); got != Open {
		t.Errorf("state after re-failing = %v, want Open", got)
	}

	// Equal stamps are not stale: the opening and the account are the same
	// instant, which is what happens when one goroutine opens the breaker and
	// another immediately reports.
	b3 := NewBreaker(3, cooldown)
	for i := 0; i < 3; i++ {
		b3.RecordFailure(now)
	}
	b3.RecordSuccess(now)
	if got := b3.State(now); got != Closed {
		t.Errorf("state after a same-instant success = %v, want Closed", got)
	}
}

func TestStateString(t *testing.T) {
	for _, tc := range []struct {
		s    State
		want string
	}{
		{Closed, "closed"},
		{Open, "open"},
		{HalfOpen, "half-open"},
		{State(99), "closed"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// TestALateFailureCannotClearAnOutstandingProbe is the failure-side half of the
// staleness rule, and the one that protects the single-probe guarantee.
//
// A breaker opens at T0. Its cooldown elapses, and request P is admitted as the
// probe. While P is upstream, a slow request admitted before the opening reports
// its failure. That failure is real, but it predates the opening: it says
// nothing about P, so it must not count toward the threshold or, above all,
// clear the probe flag and let a second request in while P is still running.
func TestALateFailureCannotClearAnOutstandingProbe(t *testing.T) {
	b := NewBreaker(3, cooldown)

	// A request admitted before the opening completes with a failure, stamped
	// at its admission time.
	stale := at(1000)
	// Three current failures open the breaker.
	for i := 0; i < 3; i++ {
		b.RecordFailure(at(1001))
	}

	// Cooldown elapses; the probe is admitted.
	probe := at(1001).Add(cooldown)
	if !b.Allow(probe) {
		t.Fatal("Allow after cooldown = false, want true")
	}

	// The stale failure lands while the probe is still upstream.
	b.RecordFailure(stale)

	// The probe must still be the only one admitted.
	if b.Allow(probe) {
		t.Error("second Allow while the probe is in flight = true, want false")
	}
	// State projects half-open once the cooldown has elapsed, whether or not
	// the probe has reported; the stored state is what must remain Open, and
	// that is what this asserts via a non-elapsed read at the opening instant.
	if got := b.State(at(1001)); got != Open {
		t.Errorf("state = %v, want Open: the stale failure must not have changed the breaker", got)
	}
}

// TestOneProbeAtATimeUnderConcurrency re-derives the audit's refuted
// "half-open probe storm" claim, this time against the fixed breaker. The audit
// verified it against the old implementation; the fix must not have
// reintroduced the storm.
func TestOneProbeAtATimeUnderConcurrency(t *testing.T) {
	b := NewBreaker(3, cooldown)
	open := at(1000)
	for i := 0; i < 3; i++ {
		b.RecordFailure(open)
	}

	const n = 200
	admitted := make(chan bool, n)
	var wg sync.WaitGroup
	after := open.Add(cooldown)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			admitted <- b.Allow(after)
		}()
	}
	wg.Wait()
	close(admitted)

	got := 0
	for ok := range admitted {
		if ok {
			got++
		}
	}
	if got != 1 {
		t.Errorf("%d concurrent Allow calls admitted, want exactly 1", got)
	}
}

// TestConcurrentAccountingIsRaceFree exercises the breaker the way the gateway
// does: many goroutines accounting against one breaker at once. Run under
// -race this is the real check that the transition bookkeeping is guarded.
func TestConcurrentAccountingIsRaceFree(t *testing.T) {
	b := NewBreaker(3, cooldown)
	now := at(1000)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				switch (i + j) % 4 {
				case 0:
					b.RecordFailure(now)
				case 1:
					b.RecordSuccess(now)
				default:
					b.Allow(now)
					b.State(now)
				}
			}
		}(i)
	}
	wg.Wait()
}
