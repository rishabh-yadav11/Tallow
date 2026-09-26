// Package health implements circuit-breaker style proactive and reactive
// health state for providers and keys. A breaker marks a provider/key
// unhealthy before routing reaches it; probes use a minimal chat completion
// (chosen explicitly), plus reactive failure accounting from real requests.
package health

import (
	"sync"
	"time"
)

// State is a circuit-breaker state.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// Breaker is a single circuit breaker. It opens after consecutive failures and
// allows a probe after a cooldown (half-open), then closes on success.
//
// Open and Closed are the resting states. HalfOpen is not: it is the exact
// instant at which Allow hands out the single cooldown probe. Modelling it as
// a state therefore required a second flag to express "a probe is in flight",
// and that flag went stale, because Allow could not tell a late-arriving
// account from one that belonged to the probe. See RecordSuccess.
type Breaker struct {
	mu               sync.Mutex
	failureThreshold int
	cooldown         time.Duration
	consecutive      int
	state            State
	openedAt         time.Time
	probeInFlight    bool
}

// NewBreaker builds a breaker. threshold <= 0 means no reactive opening (probe
// results still drive state); cooldown separates open -> probe.
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Breaker{failureThreshold: threshold, cooldown: cooldown, state: Closed}
}

// Allow reports whether a request may proceed now. A breaker whose cooldown has
// elapsed admits exactly one probe, and refuses everything else until that
// probe reports back.
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != Open {
		return true
	}
	if now.Sub(b.openedAt) < b.cooldown {
		return false
	}
	if b.probeInFlight {
		return false // the cooldown probe is still outstanding.
	}
	// The cooldown has elapsed. The breaker does not enter a persistent
	// half-open state; it simply spends one admission on the probe.
	b.probeInFlight = true
	return true
}

// RecordFailure accounts a failed request/probe.
func (b *Breaker) RecordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A failure whose stamp predates the current opening describes a request
	// that started before the breaker opened, so it cannot clear the probe
	// flag that a newer request is holding.
	if b.state == Open && now.Before(b.openedAt) {
		return
	}
	b.probeInFlight = false
	b.consecutive++
	if b.consecutive >= b.failureThreshold {
		b.toOpen(now)
	}
}

// RecordSuccess resets the breaker to closed.
//
// A success is only evidence if it arrived after the breaker last opened. Every
// account carries a timestamp, and Allow does not stamp one: a request admitted
// while the breaker was closed can be in flight for minutes and report back long
// after a burst has opened the breaker. Treating that stale success as current
// silently closed a breaker on real, recent failures - one success from an
// in-flight request, a healthy probe to close a provider that was failing.
//
// Staleness is decided by the stamp against openedAt, and only that. It does
// not depend on which request the account came from, because a breaker shared
// by concurrent requests cannot know. A breaker that has never opened has
// openedAt == 0, the zero time, so no account can be older than it and a
// genuine success is never rejected.
func (b *Breaker) RecordSuccess(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && now.Before(b.openedAt) {
		return
	}
	// No probe-flag clear is needed on the success path. A success that clears
	// the staleness check is by definition at or after the current opening, and
	// toOpen already cleared the flag for that opening; a success on a
	// non-open breaker leaves the flag unreachable, since Allow only sets it
	// while open and the transition to closed here is the only way out. Adding
	// the assignment is a no-op that reads as load-bearing.
	b.consecutive = 0
	b.state = Closed
}

// ForceOpen marks a provider/key unhealthy proactively (e.g. an explicit probe
// failure) bypassing the consecutive-failure threshold.
func (b *Breaker) ForceOpen(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toOpen(now)
}

// toOpen moves the breaker to open and (re)starts the cooldown from now.
// Callers must hold the mutex.
func (b *Breaker) toOpen(now time.Time) {
	b.state = Open
	b.openedAt = now
	b.probeInFlight = false
}

// State reports the current state (read-only). An open breaker whose cooldown has
// elapsed reports half-open, the state it would report while its single probe is
// outstanding. It does not admit that probe: only Allow does.
func (b *Breaker) State(now time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && now.Sub(b.openedAt) >= b.cooldown {
		return HalfOpen
	}
	return b.state
}
