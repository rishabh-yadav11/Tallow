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

func TestC4BudgetsPublishedBeforeRegistryOnRemove(t *testing.T) {
	reg, r := newTestRouter()

	provs := providersWithKey(budget.KeyLimits{RPM: 0, MaxConcurrent: 4})
	reg.Swap(provs, aliasesFor("p1"))
	r.SetBudgets(keyLimits(provs), provRPM(provs))

	if _, err := r.Select("a1", "", nil); err != nil {
		t.Fatalf("precondition: p1 should be routable: %v", err)
	}

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
					continue
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
