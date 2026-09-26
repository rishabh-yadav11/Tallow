package app

import (
	"sync"
	"testing"
)

// H5: auth was installed in two critical sections - setAuth(keys) followed by
// setAuthOpen(open). Both fields live under the same authMu, so the race
// detector sees nothing wrong; the defect is logical. checkAuth short-circuits
// on authOpen, so closing an open gateway (open=true -> open=false plus a key
// list) in one reload left a window where the new allow-list was already live
// while authOpen was still true, and a request carrying NO token was accepted.
//
// The invariant the fix restores is: the (allow-list, open) pair visible to any
// checkAuth call is always a pair some setAuth call actually installed. Under
// the two-section version an observer can see a pair from the cross product of
// two different calls, which no call ever installed.

// authSnapshot reads both auth fields under one read lock, the way any
// observer would.
func authSnapshot(a *App) (closed bool, keys []string) {
	a.authMu.RLock()
	defer a.authMu.RUnlock()
	keys = make([]string, 0, len(a.auth))
	for k := range a.auth {
		keys = append(keys, k)
	}
	return !a.authOpen, keys
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	m := make(map[string]bool, len(want))
	for _, k := range want {
		m[k] = true
	}
	for _, k := range got {
		if !m[k] {
			return false
		}
	}
	return true
}

// TestH5AuthSnapshotIsNeverTorn is the H5 regression.
//
// Writers alternate between two configurations whose cross product contains
// combinations no single call ever installs:
//
//	open with NO keys        - accepts everything
//	closed with keys k1,k2  - accepts exactly k1 and k2
//
// The dangerous cross pair is "closed + no keys observed while a writer is
// mid-close", and more generally any observed (closed, keys) combination that
// is not one of the two installed configurations. Observing a torn snapshot
// means auth was installed in two steps.
func TestH5AuthSnapshotIsNeverTorn(t *testing.T) {
	a := &App{}
	a.setAuth(nil, true) // installed config A: open, no keys

	const writers = 4
	const iterations = 2000

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers ping-pong between the two installed configurations.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// config B: closed, with a key list
				a.setAuth([]string{"k1", "k2"}, false)
				// config A: open, no keys
				a.setAuth(nil, true)
			}
		}()
	}

	// Readers take a self-consistent snapshot and require it to be one of the
	// two installed configurations.
	var (
		mu       sync.Mutex
		violates int
		sample   string
	)
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations*4; i++ {
				select {
				case <-stop:
					return
				default:
				}
				closed, keys := authSnapshot(a)
				ok := (!closed && len(keys) == 0) || // config A
					(closed && sameSet(keys, "k1", "k2")) // config B
				if !ok {
					mu.Lock()
					violates++
					if sample == "" {
						sample = describeAuthSnapshot(closed, keys)
					}
					mu.Unlock()
					return
				}
			}
		}()
	}

	// Drain the readers, then close the gateway and stop the writers.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	a.setAuth([]string{"k1", "k2"}, false)
	close(stop)
	<-done

	if violates > 0 {
		t.Fatalf("H5 NOT FIXED: observed %d torn auth snapshot(s); first was %s. "+
			"Auth must be installed in a single critical section.", violates, sample)
	}
}

func describeAuthSnapshot(closed bool, keys []string) string {
	state := "open"
	if closed {
		state = "closed"
	}
	if len(keys) == 0 {
		return state + " with NO keys"
	}
	return state + " with keys " + joinKeys(keys)
}

func joinKeys(keys []string) string {
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		out += k
	}
	return out
}

// TestH5ClosingAnOpenGatewayAcceptsNoToken is the behavioural consequence of the
// same invariant, checked without concurrency.
//
// The exact H5 window was: allow-list installed, authOpen still true, tokenless
// request accepted. Closing the gateway in one setAuth call must leave no such
// state, and the closed gateway must reject a tokenless request outright.
func TestH5ClosingAnOpenGatewayAcceptsNoToken(t *testing.T) {
	a := &App{}

	a.setAuth(nil, true)
	if !a.checkAuth("") {
		t.Fatal("precondition: an open gateway should accept a tokenless request")
	}

	// One atomic close, exactly as a reload that turns open=false performs it.
	a.setAuth([]string{"k1", "k2"}, false)

	if a.checkAuth("") {
		t.Fatal("H5 NOT FIXED: a tokenless request was accepted after the gateway was closed")
	}
	for _, bad := range []string{"k3", "K1", "k1 ", " k1", "unknown"} {
		if a.checkAuth(bad) {
			t.Fatalf("H5 NOT FIXED: token %q was accepted by a closed gateway", bad)
		}
	}
	for _, good := range []string{"k1", "k2"} {
		if !a.checkAuth(good) {
			t.Fatalf("allow-listed token %q was rejected by a closed gateway", good)
		}
	}

	// And the snapshot is self-consistent: closed implies the key list is live.
	closed, keys := authSnapshot(a)
	if !closed || !sameSet(keys, "k1", "k2") {
		t.Fatalf("closed gateway exposed %s; the open flag and the allow-list disagree", describeAuthSnapshot(closed, keys))
	}
}

// TestH5SetAuthInstallsBothFieldsTogether pins the per-call contract directly,
// for every shape of configuration an operator can write.
func TestH5SetAuthInstallsBothFieldsTogether(t *testing.T) {
	cases := []struct {
		name  string
		keys  []string
		open  bool
		allow []string
		deny  []string
	}{
		{name: "open accepts everything", open: true, allow: []string{"", "k1", "anything"}},
		{
			name:  "closed accepts only the list",
			keys:  []string{"k1", "k2"},
			allow: []string{"k1", "k2"},
			deny:  []string{"", "k3", "k1 ", "K1"},
		},
		{name: "closed with no keys accepts nothing", keys: nil, deny: []string{"", "k1"}},
		{name: "empty keys are dropped", keys: []string{"", "k1", ""}, allow: []string{"k1"}, deny: []string{""}},
		{name: "duplicate keys collapse", keys: []string{"k1", "k1"}, allow: []string{"k1"}, deny: []string{"", "k2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{}
			a.setAuth(tc.keys, tc.open)
			for _, tok := range tc.allow {
				if !a.checkAuth(tok) {
					t.Errorf("token %q was rejected but should be allowed", tok)
				}
			}
			for _, tok := range tc.deny {
				if a.checkAuth(tok) {
					t.Errorf("token %q was accepted but should be denied", tok)
				}
			}
			closed, keys := authSnapshot(a)
			if closed == tc.open {
				t.Fatalf("snapshot open=%v but setAuth was called with open=%v", !closed, tc.open)
			}
			if !sameSet(keys, nonEmpty(tc.keys)...) {
				t.Fatalf("snapshot keys %s do not match the installed list", describeAuthSnapshot(closed, keys))
			}
		})
	}
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, k := range in {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
