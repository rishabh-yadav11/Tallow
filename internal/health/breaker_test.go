package health

import (
	"sync"
	"testing"
	"time"
)

func at(sec int64) time.Time { return time.Unix(sec, 0) }

const (
	cooldown        = 30 * time.Second
	defaultCooldown = 30 * time.Second
)

func TestRecordSuccessInHalfOpenClearsTheProbeFlag(t *testing.T) {
	b := NewBreaker(3, cooldown)
	open := at(1000)

	for i := 0; i < 3; i++ {
		b.RecordFailure(open)
	}
	if got := b.State(open); got != Open {
		t.Fatalf("after 3 failures state = %v, want Open", got)
	}

	if !b.Allow(open.Add(cooldown)) {
		t.Fatal("Allow after cooldown = false, want true (probe admitted)")
	}
	if got := b.State(open.Add(cooldown)); got != HalfOpen {
		t.Fatalf("state after probe admitted = %v, want HalfOpen", got)
	}

	if b.Allow(open.Add(cooldown)) {
		t.Error("second Allow during half-open = true, want false")
	}

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

	admittedAt := at(1000)

	for i := 0; i < 3; i++ {
		b.RecordFailure(at(1001))
	}
	if got := b.State(at(1001)); got != Open {
		t.Fatalf("after 3 failures state = %v, want Open", got)
	}

	b.RecordSuccess(admittedAt)

	if got := b.State(at(1001)); got != Open {
		t.Errorf("state = %v, want Open: a late success must not close the breaker", got)
	}
	if b.Allow(at(1001)) {
		t.Error("Allow = true, want false: the breaker must still be open")
	}
}

func TestARealSuccessStillClosesTheBreaker(t *testing.T) {
	b := NewBreaker(3, cooldown)

	b.RecordFailure(at(1000))
	b.RecordSuccess(at(1000))

	if got := b.State(at(1000)); got != Closed {
		t.Errorf("state = %v, want Closed: a real success must still close the breaker", got)
	}
	if !b.Allow(at(1000)) {
		t.Error("Allow = false, want true")
	}
}

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

	if b.Allow(probe.Add(cooldown - time.Second)) {
		t.Error("Allow before the new cooldown elapsed = true, want false")
	}
	if !b.Allow(probe.Add(cooldown)) {
		t.Error("Allow after the new cooldown elapsed = false, want true")
	}
}

func TestForceOpenIsNotClosedByALateSuccess(t *testing.T) {
	b := NewBreaker(3, cooldown)

	b.RecordSuccess(at(1000))
	b.ForceOpen(at(1001))

	if got := b.State(at(1001)); got != Open {
		t.Errorf("state = %v, want Open: a late success must not undo a proactive open", got)
	}
}

func TestStateDoesNotMutate(t *testing.T) {
	b := NewBreaker(3, cooldown)
	b.ForceOpen(at(1000))

	after := at(1000).Add(cooldown)

	if got := b.State(after); got != HalfOpen {
		t.Fatalf("State = %v, want HalfOpen", got)
	}
	if got := b.State(after); got != HalfOpen {
		t.Fatalf("second State = %v, want HalfOpen", got)
	}

	if !b.Allow(after) {
		t.Error("Allow after two State reads = false, want true: State must not admit a probe")
	}
	if b.Allow(after) {
		t.Error("second Allow = true, want false")
	}
}

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

			if b.Allow(now.Add(defaultCooldown - time.Second)) {
				t.Error("Allow one second before the default cooldown = true, want false")
			}

			if !b.Allow(now.Add(defaultCooldown)) {
				t.Error("Allow at the default cooldown = false, want true")
			}
		})
	}
}

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

func TestRecordSuccessAfterAClosedBreakerIsFresh(t *testing.T) {
	now := at(1000)

	b := NewBreaker(3, cooldown)
	b.RecordSuccess(now)
	if !b.Allow(now) {
		t.Error("a success on a never-opened breaker left it refusing traffic")
	}

	b2 := NewBreaker(3, cooldown)
	for i := 0; i < 3; i++ {
		b2.RecordFailure(now)
	}
	b2.RecordSuccess(now.Add(cooldown))
	if !b2.Allow(now.Add(cooldown)) {
		t.Error("a success after recovery left the breaker refusing traffic")
	}

	for i := 0; i < 3; i++ {
		b2.RecordFailure(now.Add(cooldown))
	}
	if got := b2.State(now.Add(cooldown)); got != Open {
		t.Errorf("state after re-failing = %v, want Open", got)
	}

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

func TestALateFailureCannotClearAnOutstandingProbe(t *testing.T) {
	b := NewBreaker(3, cooldown)

	stale := at(1000)

	for i := 0; i < 3; i++ {
		b.RecordFailure(at(1001))
	}

	probe := at(1001).Add(cooldown)
	if !b.Allow(probe) {
		t.Fatal("Allow after cooldown = false, want true")
	}

	b.RecordFailure(stale)

	if b.Allow(probe) {
		t.Error("second Allow while the probe is in flight = true, want false")
	}

	if got := b.State(at(1001)); got != Open {
		t.Errorf("state = %v, want Open: the stale failure must not have changed the breaker", got)
	}
}

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
