package health

import (
	"sync"
	"time"
)

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

type Breaker struct {
	mu               sync.Mutex
	failureThreshold int
	cooldown         time.Duration
	consecutive      int
	state            State
	openedAt         time.Time
	probeInFlight    bool
}

func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Breaker{failureThreshold: threshold, cooldown: cooldown, state: Closed}
}

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
		return false
	}

	b.probeInFlight = true
	return true
}

func (b *Breaker) RecordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == Open && now.Before(b.openedAt) {
		return
	}
	b.probeInFlight = false
	b.consecutive++
	if b.consecutive >= b.failureThreshold {
		b.toOpen(now)
	}
}

func (b *Breaker) RecordSuccess(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && now.Before(b.openedAt) {
		return
	}

	b.consecutive = 0
	b.state = Closed
}

func (b *Breaker) ForceOpen(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toOpen(now)
}

func (b *Breaker) toOpen(now time.Time) {
	b.state = Open
	b.openedAt = now
	b.probeInFlight = false
}

func (b *Breaker) State(now time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && now.Sub(b.openedAt) >= b.cooldown {
		return HalfOpen
	}
	return b.state
}
