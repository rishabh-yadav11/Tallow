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
type Breaker struct {
	mu               sync.Mutex
	failureThreshold int
	cooldown         time.Duration
	consecutive      int
	state            State
	openedAt         time.Time
}

// NewBreaker builds a breaker. threshold <= 0 means no reactive opening (probe
// results still drive state); cooldown separates open -> half-open.
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Breaker{failureThreshold: threshold, cooldown: cooldown, state: Closed}
}

// Allow reports whether a request may proceed now. An open breaker transitions
// to half-open after the cooldown, admitting a single probe.
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if now.Sub(b.openedAt) >= b.cooldown {
			b.state = HalfOpen
			return true // admit one probe.
		}
		return false
	case HalfOpen:
		return false // a probe is already in flight.
	default:
		return true
	}
}

// RecordFailure accounts a failed request/probe.
func (b *Breaker) RecordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	if b.state == HalfOpen || b.consecutive >= b.failureThreshold {
		b.state = Open
		b.openedAt = now
	}
}

// RecordSuccess resets the breaker to closed.
func (b *Breaker) RecordSuccess(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = Closed
	b.consecutive = 0
}

// ForceOpen marks a provider/key unhealthy proactively (e.g. an explicit probe
// failure) bypassing the consecutive-failure threshold.
func (b *Breaker) ForceOpen(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = Open
	b.openedAt = now
	b.consecutive = 0
}

// State reports the current state (read-only).
func (b *Breaker) State(now time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && now.Sub(b.openedAt) >= b.cooldown {
		return HalfOpen
	}
	return b.state
}
