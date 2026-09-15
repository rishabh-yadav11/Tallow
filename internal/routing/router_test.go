package routing

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

func fixture() (*Router, time.Time) {
	provs := []model.Provider{
		{
			Name:    "p1",
			BaseURL: "https://p1.example/v1",
			Keys: []model.Key{
				{ID: "k1", Secret: "s1", RPM: 100},
				{ID: "k2", Secret: "s2", RPM: 100},
			},
		},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k3", Secret: "s3", RPM: 100}}},
	}
	aliases := []model.Alias{
		{
			Name: "flash",
			Targets: []model.Target{
				{Provider: "p1", Model: "flash-v4", SupportsTools: true, PriceInputPerM: 1.0, PriceOutputPerM: 2.0},
				{Provider: "p2", Model: "flash-v4"},
			},
		},
	}
	reg := registry.New(provs, aliases)
	h := health.NewRegistry()
	r := NewRouter(reg, h, time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{
		"p1/k1": {RPM: 100},
		"p1/k2": {RPM: 100},
		"p2/k3": {RPM: 100},
	}, map[string]int{})
	return r, time.Unix(1000, 0)
}

func TestSelectTwoLayerAndSticky(t *testing.T) {
	r, _ := fixture()
	sel, err := r.Select("flash", "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider != "p1" || sel.Key == "" || sel.Model != "flash-v4" {
		t.Fatalf("unexpected selection: %+v", sel)
	}
	// Sticky: same session resolves to the same provider/key.
	sel2, err := r.Select("flash", "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel2.Provider != sel.Provider || sel2.Key != sel.Key {
		t.Fatalf("sticky broken: got %s/%s want %s/%s", sel2.Provider, sel2.Key, sel.Provider, sel.Key)
	}
	r.Release(sel, 0)
	r.Release(sel2, 0)
}

func TestStickyRespectsHealth(t *testing.T) {
	r, now := fixture()
	sel, _ := r.Select("flash", "sess", nil)
	r.Release(sel, 0)

	// Force the pinned provider/key unhealthy via the breaker.
	r.health.Provider(sel.Provider).ForceOpen(now)
	r.health.Key(sel.Provider, sel.Key).ForceOpen(now)

	// Sticky must not re-pin an unhealthy key; it should fall through.
	sel2, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel2.Provider == sel.Provider && sel2.Key == sel.Key {
		t.Fatal("sticky pinned an unhealthy key")
	}
	r.Release(sel2, 0)
}

func TestFallbackOnNoKey(t *testing.T) {
	r, now := fixture()
	// Exhaust p1's keys by opening both breakers.
	r.health.Key("p1", "k1").ForceOpen(now)
	r.health.Key("p1", "k2").ForceOpen(now)
	sel, err := r.Select("flash", "s2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider != "p2" {
		t.Fatalf("expected fallback to p2, got %s", sel.Provider)
	}
	r.Release(sel, 0)
}

func TestUnknownAlias(t *testing.T) {
	r, _ := fixture()
	if _, err := r.Select("nope", "s", nil); err == nil {
		t.Fatal("expected error for unknown alias")
	}
}

func TestSelectHonorsKeySkip(t *testing.T) {
	r, _ := fixture()
	// First select pins nothing yet; force it to take a key and record it.
	skip := map[string]bool{"p1/k1": true}
	sel, err := r.Select("flash", "s1", skip)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider != "p1" || sel.Key == "k1" {
		t.Fatalf("expected p1 to avoid k1, got %s/%s", sel.Provider, sel.Key)
	}
	// p1 still has k2 available; key k1 stayed untouched by this selection.
	r.Release(sel, 0)
}

func TestStickyPinToRemovedProviderNotReused(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1", RPM: 100}}},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k3", Secret: "s3", RPM: 100}}},
	}
	aliases := []model.Alias{
		{
			Name: "flash",
			Targets: []model.Target{
				{Provider: "p1", Model: "flash-v4"},
				{Provider: "p2", Model: "flash-v4"},
			},
		},
	}
	reg := registry.New(provs, aliases)
	h := health.NewRegistry()
	r := NewRouter(reg, h, time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {RPM: 100}, "p2/k3": {RPM: 100}}, map[string]int{})

	// Pin session to p1/k1.
	sel, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider != "p1" {
		t.Fatalf("expected initial pin on p1, got %s", sel.Provider)
	}
	r.Release(sel, 0)

	// Hot-reload: p1 is removed from the registry.
	r.reg.Swap([]model.Provider{
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k3", Secret: "s3", RPM: 100}}},
	}, aliases)

	sel2, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("stale pin should fall through to p2, got error: %v", err)
	}
	if sel2.Provider == "p1" {
		t.Fatalf("sticky pin to removed provider p1 was reused: %+v", sel2)
	}
	if sel2.Provider != "p2" {
		t.Fatalf("expected fallback to p2, got %s", sel2.Provider)
	}
	if sel2.BaseURL == "" {
		t.Fatalf("selection must never have an empty BaseURL: %+v", sel2)
	}
	r.Release(sel2, 0)
}

func TestConcurrentSelectBudgetsNoRace(t *testing.T) {
	r, _ := fixture()
	// Pin some sessions and then hammer Select/Budgets from many goroutines
	// while Budgets runs concurrently (exercises the bmu read/write paths).
	done := make(chan bool, 32)
	for g := 0; g < 32; g++ {
		go func(gi int) {
			for i := 0; i < 200; i++ {
				sess := "s" + string(rune('a'+gi%26))
				if sel, err := r.Select("flash", sess, nil); err == nil {
					r.Release(sel, 0)
				}
				_ = r.Budgets(time.Unix(1000, 0))
				_ = r.ProviderRPM("p1", time.Unix(1000, 0))
			}
			done <- true
		}(g)
	}
	for g := 0; g < 32; g++ {
		<-done
	}
}
