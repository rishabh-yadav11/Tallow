package app

import (
	"sync"
	"testing"
)

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

func TestH5AuthSnapshotIsNeverTorn(t *testing.T) {
	a := &App{}
	a.setAuth(nil, true)

	const writers = 4
	const iterations = 2000

	var wg sync.WaitGroup
	stop := make(chan struct{})

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

				a.setAuth([]string{"k1", "k2"}, false)

				a.setAuth(nil, true)
			}
		}()
	}

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
				ok := (!closed && len(keys) == 0) ||
					(closed && sameSet(keys, "k1", "k2"))
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

func TestH5ClosingAnOpenGatewayAcceptsNoToken(t *testing.T) {
	a := &App{}

	a.setAuth(nil, true)
	if !a.checkAuth("") {
		t.Fatal("precondition: an open gateway should accept a tokenless request")
	}

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

	closed, keys := authSnapshot(a)
	if !closed || !sameSet(keys, "k1", "k2") {
		t.Fatalf("closed gateway exposed %s; the open flag and the allow-list disagree", describeAuthSnapshot(closed, keys))
	}
}

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
