package app

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// providersWithKey builds a single-provider snapshot whose single key carries
// the given limits, so a test can observe exactly one budget id: "p1/k1".
//
// BaseURL is set because the router treats an empty one as "the provider
// vanished", which is the very signal the stale-selection fix (C1) relies on.
func providersWithKey(limits budget.KeyLimits) []model.Provider {
	return []model.Provider{{
		Name:    "p1",
		BaseURL: "https://p1.example",
		Keys:    []model.Key{{ID: "k1", RPM: limits.RPM, MaxConcurrent: limits.MaxConcurrent}},
	}}
}

func aliasesFor(p string) []model.Alias {
	return []model.Alias{{
		Name:    "a1",
		Targets: []model.Target{{Provider: p, Model: "m1"}},
	}}
}

func newTestRouter() (*registry.Registry, *routing.Router) {
	reg := registry.New(nil, nil)
	return reg, routing.NewRouter(reg, health.NewRegistry(), 0, nil)
}

// TestC4PublishOrderClosesTornWindow is the DETERMINISTIC C4 regression test.
//
// It drives a real App.Reload and uses the publishHook seam to inspect the
// exact intermediate state, between the two independently-locked mutations.
// That is the only way to test this reliably: the window is nanoseconds wide,
// so a concurrent observer cannot catch a regression, which is precisely why
// the audit's original code shipped broken with a green suite.
//
// The invariant under test is one-directional and is what the publish order
// buys: at no point may a key be ROUTABLE and lacking a budget. The reverse -
// a budget installed for a key not yet in the registry - is harmless, because
// an unroutable key cannot be reached.
func TestC4PublishOrderClosesTornWindow(t *testing.T) {
	dir := t.TempDir()
	const masterKey = "c4-master-key"
	box, err := secret.NewBox([]byte(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("p1:k1", "sk-fake"); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.toml")
	cfg := `version = 1

[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "` + filepath.Join(dir, "admin.sock") + `"
max_concurrent = 8

[secret]
keystore = "` + filepath.Join(dir, "keys.json") + `"

[store]
path = "` + filepath.Join(dir, "tallow.db") + `"

[observability]
enabled = true

[[provider]]
name = "p1"
base_url = "https://p1.example"

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  rpm = 1
  max_concurrent = 1

[[alias]]
name = "a1"
  [[alias.target]]
  provider = "p1"
  model = "m1"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, masterKey)

	a, err := New(cfgPath, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Add a SECOND provider on reload. p2 is the interesting one: before this
	// reload only p1 existed, so p2 is "newly added" and is exactly the key the
	// audit's torn window left unrestrained.
	cfg2 := cfg + `
[[provider]]
name = "p2"
base_url = "https://p2.example"

  [[provider.key]]
  id = "k9"
  ref = "p1:k1"
  rpm = 1
  max_concurrent = 1
`
	if err := os.WriteFile(cfgPath, []byte(cfg2), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		mu              sync.Mutex
		observed        []string
		intermediateErr error
	)

	publishHook = func() {
		// This runs between SetBudgets and Swap under the audit's OLD order it
		// would run between Swap and SetBudgets, so what we see here is exactly
		// the state a request could have landed on.
		mu.Lock()
		defer mu.Unlock()
		_, p2Routable := a.reg.Provider("p2")
		_, kb := a.router.BudgetFor("p2/k9")
		switch {
		case p2Routable && !kb:
			observed = append(observed, "TORN: p2 is routable with no budget (C4)")
			intermediateErr = errors.New("torn window: routable key without a budget")
		case p2Routable && kb:
			observed = append(observed, "safe: p2 routable and budgeted")
		case !p2Routable && kb:
			observed = append(observed, "safe: p2 budgeted but not yet routable (inert)")
		default:
			observed = append(observed, "unknown: p2 neither routable nor budgeted")
		}
	}
	t.Cleanup(func() { publishHook = nil })

	if err := a.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("intermediate states observed: %v", observed)
	if intermediateErr != nil {
		t.Fatalf("C4 regression: %v", intermediateErr)
	}
	if len(observed) == 0 {
		t.Fatal("publishHook never fired; the seam is not wired into Reload")
	}
}

// TestC4BudgetsPublishedBeforeRegistryOnRemove covers the removal half, which
// the audit noted had no test at all and which is the path that panicked in C1.
// Removing a provider must leave it unroutable and un-budgeted, and a Select
// must fail cleanly rather than dereference a nil budget.
func TestC4BudgetsPublishedBeforeRegistryOnRemove(t *testing.T) {
	reg, r := newTestRouter()

	provs := providersWithKey(budget.KeyLimits{RPM: 0, MaxConcurrent: 4})
	reg.Swap(provs, aliasesFor("p1"))
	r.SetBudgets(keyLimits(provs), provRPM(provs))

	if _, err := r.Select("a1", "", nil); err != nil {
		t.Fatalf("precondition: p1 should be routable: %v", err)
	}

	// Reload that REMOVES p1: budgets first, then the registry.
	empty := []model.Provider{}
	r.SetBudgets(keyLimits(empty), provRPM(empty))
	reg.Swap(empty, aliasesFor("p1"))

	if _, err := r.Select("a1", "", nil); err == nil {
		t.Fatal("a removed provider must not be routable")
	}
	if _, ok := reg.Provider("p1"); ok {
		t.Fatal("p1 should be absent from the registry after removal")
	}
	if _, ok := r.BudgetFor("p1/k1"); ok {
		t.Fatal("p1/k1's budget should be absent after removal")
	}
}

// TestC4KeyAddedMidPublishIsNeverUnbudgeted reproduces the audit's measurement
// directly: 50 requests against a key configured RPM=1 / MaxConcurrent=1. If
// the key were unrestrained, all 50 would succeed.
func TestC4KeyAddedMidPublishIsNeverUnbudgeted(t *testing.T) {
	reg, r := newTestRouter()

	provs := providersWithKey(budget.KeyLimits{RPM: 1, MaxConcurrent: 1})
	r.SetBudgets(keyLimits(provs), provRPM(provs))
	reg.Swap(provs, aliasesFor("p1"))

	var succeeded int
	for i := 0; i < 50; i++ {
		if sel, err := r.Select("a1", "", nil); err == nil && sel != nil {
			succeeded++
			r.Release(sel, 0)
		}
	}
	if succeeded > 5 {
		t.Fatalf("key appears unbudgeted: %d/50 requests succeeded with RPM=1/MaxConcurrent=1", succeeded)
	}
}

// TestC4NoTornWindowUnderConcurrentReload hammers Select against a reload
// publishing in a loop, and asserts the race-free property under -race. It is
// not a deterministic regression test (see the hook test above for that); it
// exists to catch data races and to ensure no request panics or receives a
// nil-budget selection.
func TestC4NoTornWindowUnderConcurrentReload(t *testing.T) {
	reg, r := newTestRouter()

	provs := providersWithKey(budget.KeyLimits{RPM: 0, MaxConcurrent: 8})
	reg.Swap(provs, aliasesFor("p1"))
	r.SetBudgets(keyLimits(provs), provRPM(provs))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.SetBudgets(keyLimits(provs), provRPM(provs))
			reg.Swap(provs, aliasesFor("p1"))
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sel, err := r.Select("a1", "", nil)
				if err != nil {
					continue // clean failure is acceptable
				}
				if sel == nil {
					t.Error("select returned (nil, nil)")
					return
				}
				r.Release(sel, 0)
			}
		}()
	}

	for i := 0; i < 20000; i++ {
		sel, err := r.Select("a1", "", nil)
		if err == nil && sel != nil {
			r.Release(sel, 0)
		}
	}
	close(stop)
	wg.Wait()
}
