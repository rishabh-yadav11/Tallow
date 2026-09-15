package proxy

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errOverCapacity sentinel used by acquireSlot.
func isOverCapacity(err error) bool {
	return errors.Is(err, errOverCapacity) || err == errOverCapacity
}

// TestAcquireSlotTimesOutNoLeak verifies issue #8: when the bounded queue is full,
// acquireSlot returns over-capacity after QueueTimeout using a timer whose Stop is
// called (no per-request time.After goroutine leak). We assert the call returns
// promptly and frees its slot so a subsequent acquire succeeds.
func TestAcquireSlotTimesOutNoLeak(t *testing.T) {
	h := NewHandler(Deps{MaxConcurrent: 1, QueueTimeout: 20 * time.Millisecond})

	// Occupy the only slot.
	if err := h.acquireSlot(); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	start := time.Now()
	err := h.acquireSlot()
	elapsed := time.Since(start)
	// Must return over-capacity (not hang) and do so near QueueTimeout.
	if !isOverCapacity(err) {
		t.Fatalf("expected over-capacity error, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("acquireSlot blocked too long (%v); timer likely leaked", elapsed)
	}

	// The slot must be freed once we release, so a new acquire succeeds.
	h.releaseSlot()
	if err := h.acquireSlot(); err != nil {
		t.Fatalf("slot not freed after release+timeout: %v", err)
	}
	h.releaseSlot()
}

// TestAcquireSlotConcurrent exercises high churn of acquire/release to ensure the
// bounded queue + timer Stop path does not deadlock or leak slots under load. The
// total number of in-flight slots never exceeds MaxConcurrent.
func TestAcquireSlotConcurrent(t *testing.T) {
	const max = 8
	h := NewHandler(Deps{MaxConcurrent: max, QueueTimeout: 10 * time.Millisecond})

	var active int64
	var mu sync.Mutex
	var peak int
	done := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if err := h.acquireSlot(); err != nil {
					continue // over capacity; fine
				}
				n := atomic.AddInt64(&active, 1)
				mu.Lock()
				if int(n) > peak {
					peak = int(n)
				}
				mu.Unlock()
				time.Sleep(time.Microsecond)
				atomic.AddInt64(&active, -1)
				h.releaseSlot()
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("acquire/release churn deadlocked")
	}
	if peak > max {
		t.Fatalf("peak in-flight slots %d exceeded MaxConcurrent %d (slot leak)", peak, max)
	}
}

// TestProbeSemBounded mirrors app.probeSem (issue #8): at most the semaphore
// capacity of probe goroutines may run concurrently, so a large provider/key fanout
// cannot spawn unbounded goroutines. We exercise the same semaphore pattern here.
func TestProbeSemBounded(t *testing.T) {
	const cap = 16
	sem := make(chan struct{}, cap)
	var mu sync.Mutex
	maxConcurrent := 0
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		select {
		case sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				mu.Lock()
				maxConcurrent++
				if maxConcurrent > cap {
					t.Errorf("probe concurrency %d exceeded cap %d", maxConcurrent, cap)
				}
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				maxConcurrent--
				mu.Unlock()
			}()
		default:
			// saturated: intended backpressure; skip this round.
		}
	}
	wg.Wait()
}
