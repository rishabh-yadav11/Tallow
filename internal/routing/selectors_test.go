package routing

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

func keyLimit(rpm int) budget.KeyLimits { return budget.KeyLimits{RPM: rpm} }

func newSelectorsRouter(t *testing.T, strategy Strategy, provs []model.Provider, aliases []model.Alias, limits map[string]budget.KeyLimits) (*Router, time.Time) {
	t.Helper()
	reg := registry.New(provs, aliases)
	h := health.NewRegistry()
	now := time.Unix(1000, 0)
	r := NewRouterWithOpts(reg, h, time.Minute, func() time.Time { return now }, strategy)
	r.SetBudgets(limits, map[string]int{})
	return r, now
}

func TestWeightedSelectionReachesAllKeys(t *testing.T) {
	prov := model.Provider{
		Name:    "p1",
		BaseURL: "https://p1.example/v1",
		Keys: []model.Key{
			{ID: "heavy", Secret: "sh", Weight: 4, RPM: 1_000_000},
			{ID: "light", Secret: "sl", Weight: 2, RPM: 1_000_000},
			{ID: "zero", Secret: "sz", Weight: 0, RPM: 1_000_000},
		},
	}
	alias := model.Alias{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}
	r, _ := newSelectorsRouter(t, StrategyWeightedRoundRobin, []model.Provider{prov}, []model.Alias{alias}, map[string]budget.KeyLimits{
		"p1/heavy": keyLimit(1_000_000),
		"p1/light": keyLimit(1_000_000),
		"p1/zero":  keyLimit(1_000_000),
	})

	const draws = 2400
	counts := map[string]int{}
	for range draws {
		sel, err := r.Select("flash", "", nil)
		if err != nil {
			t.Fatalf("select %d: %v", len(counts), err)
		}
		counts[sel.Key]++
		r.Release(sel, 0)
	}
	if len(counts) != 3 {
		t.Fatalf("expected all three weighted keys to be reached, got %v", counts)
	}
	if counts["heavy"] <= counts["light"] {
		t.Fatalf("heavy key not proportionally preferred: %v", counts)
	}
}

func TestFillFirstPacksHighPriorityThenFallsThrough(t *testing.T) {
	prov := model.Provider{
		Name:    "p1",
		BaseURL: "https://p1.example/v1",
		Keys: []model.Key{
			{ID: "low", Secret: "l", Priority: 0},
			{ID: "mid", Secret: "m", Priority: 5},
			{ID: "high", Secret: "h", Priority: 10},
		},
	}
	alias := model.Alias{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}
	r, _ := newSelectorsRouter(t, StrategyFillFirst, []model.Provider{prov}, []model.Alias{alias}, map[string]budget.KeyLimits{
		"p1/high": keyLimit(1),
		"p1/mid":  keyLimit(1),
		"p1/low":  keyLimit(1_000_000),
	})

	for i, want := range []string{"high", "mid", "low"} {
		sel, err := r.Select("flash", "", nil)
		if err != nil {
			t.Fatalf("select #%d: %v", i, err)
		}
		if sel.Key != want {
			t.Fatalf("select #%d = %s, want %s", i, sel.Key, want)
		}
		r.Release(sel, 0)
	}
}

func TestDisabledProviderNeverSelected(t *testing.T) {
	disabled := model.Provider{
		Name:     "p1",
		BaseURL:  "https://p1.example/v1",
		Disabled: true,
		Keys:     []model.Key{{ID: "k1", Secret: "s1"}},
	}
	enabled := model.Provider{
		Name:    "p2",
		BaseURL: "https://p2.example/v1",
		Keys:    []model.Key{{ID: "k2", Secret: "s2"}},
	}
	alias := model.Alias{
		Name: "flash",
		Targets: []model.Target{
			{Provider: "p1", Model: "flash-v4"},
			{Provider: "p2", Model: "flash-v4"},
		},
	}
	r, _ := newSelectorsRouter(t, StrategyRoundRobin, []model.Provider{disabled, enabled}, []model.Alias{alias}, map[string]budget.KeyLimits{
		"p1/k1": keyLimit(10),
		"p2/k2": keyLimit(10),
	})
	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.Provider != "p2" {
		t.Fatalf("disabled provider was selected: got %s", sel.Provider)
	}
	r.Release(sel, 0)
}

func TestExcludedModelsFiltersTargets(t *testing.T) {
	prov := model.Provider{
		Name:           "p1",
		BaseURL:        "https://p1.example/v1",
		ExcludedModels: []string{"*flash-v4*"},
		Keys:           []model.Key{{ID: "k1", Secret: "s1"}},
	}
	p2 := model.Provider{
		Name:    "p2",
		BaseURL: "https://p2.example/v1",
		Keys:    []model.Key{{ID: "k2", Secret: "s2"}},
	}
	alias := model.Alias{Name: "flash", Targets: []model.Target{
		{Provider: "p1", Model: "flash-v4"},
		{Provider: "p2", Model: "pro"},
	}}
	r, _ := newSelectorsRouter(t, StrategyRoundRobin, []model.Provider{prov, p2}, []model.Alias{alias}, map[string]budget.KeyLimits{
		"p1/k1": keyLimit(10),
		"p2/k2": keyLimit(10),
	})
	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider != "p2" || sel.Key != "k2" {
		t.Fatalf("excluded model not honored: got %s/%s, want p2/k2", sel.Provider, sel.Key)
	}
	r.Release(sel, 0)

	alias2 := model.Alias{
		Name:           "preview",
		ExcludedModels: []string{"*-preview"},
		Targets: []model.Target{
			{Provider: "p1", Model: "gpt-preview"},
			{Provider: "p2", Model: "gpt-stable"},
		},
	}
	r2, _ := newSelectorsRouter(t, StrategyRoundRobin, []model.Provider{prov, p2}, []model.Alias{alias2}, map[string]budget.KeyLimits{
		"p1/k1": keyLimit(10),
		"p2/k2": keyLimit(10),
	})
	sel2, err := r2.Select("preview", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel2.Provider != "p2" {
		t.Fatalf("alias excluded_models not honored: got %s", sel2.Provider)
	}
	r2.Release(sel2, 0)
}

func TestExcluderSkipsKey(t *testing.T) {
	prov := model.Provider{
		Name:    "p1",
		BaseURL: "https://p1.example/v1",
		Keys: []model.Key{
			{ID: "k1", Secret: "s1"},
			{ID: "k2", Secret: "s2"},
		},
	}
	alias := model.Alias{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}
	r, _ := newSelectorsRouter(t, StrategyRoundRobin, []model.Provider{prov}, []model.Alias{alias}, map[string]budget.KeyLimits{
		"p1/k1": keyLimit(10),
		"p1/k2": keyLimit(10),
	})
	r.SetExcluder(func(provider, key string) bool { return provider == "p1" && key == "k1" })

	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Key != "k2" {
		t.Fatalf("excluded key k1 was selected, got %s", sel.Key)
	}
	r.Release(sel, 0)
}

func TestSelectionCarriesHeadersProxyURL(t *testing.T) {
	prov := model.Provider{
		Name:     "p1",
		BaseURL:  "https://p1.example/v1",
		Headers:  map[string]string{"x-provider": "pv"},
		ProxyURL: "https://provider.proxy",
		Keys: []model.Key{
			{ID: "override", Secret: "o",
				Headers:  map[string]string{"x-key": "kv", "x-provider": "from-key"},
				ProxyURL: "https://key.proxy"},
			{ID: "inherit", Secret: "i"},
		},
	}
	alias := model.Alias{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}
	r, _ := newSelectorsRouter(t, StrategyRoundRobin, []model.Provider{prov}, []model.Alias{alias}, map[string]budget.KeyLimits{
		"p1/override": keyLimit(100),
		"p1/inherit":  keyLimit(100),
	})

	selO, errO := r.Select("flash", "", map[string]bool{"p1/inherit": true})
	if errO != nil {
		t.Fatal(errO)
	}
	if selO.Key != "override" || selO.Headers["x-key"] != "kv" || selO.Headers["x-provider"] != "from-key" || selO.ProxyURL != "https://key.proxy" {
		t.Fatalf("override key not honored: %+v", selO)
	}
	r.Release(selO, 0)

	selI, errI := r.Select("flash", "", map[string]bool{"p1/override": true})
	if errI != nil {
		t.Fatal(errI)
	}
	if selI.Key != "inherit" || selI.Headers["x-provider"] != "pv" || selI.ProxyURL != "https://provider.proxy" {
		t.Fatalf("inherited headers/proxy not applied: %+v", selI)
	}
	r.Release(selI, 0)
}

func newAffinityRouter(t *testing.T, rebindOn string) (*Router, time.Time) {
	t.Helper()
	prov := model.Provider{
		Name:    "p1",
		BaseURL: "https://p1.example/v1",
		Keys:    []model.Key{{ID: "k1", Secret: "s1"}},
	}
	alias := model.Alias{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}
	now := time.Unix(1000, 0)
	reg := registry.New([]model.Provider{prov}, []model.Alias{alias})
	h := health.NewRegistry()
	r := NewRouterWithOpts(reg, h, time.Minute, func() time.Time { return now }, StrategyRoundRobin)
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": keyLimit(1)}, map[string]int{})
	if rebindOn != "" {
		r.SetSessionAffinity(SessionAffinity{Enabled: true, TTL: time.Minute, RebindOn: rebindOn})
	}
	return r, now
}

func TestAffinityRebindQuotaForgetsOnExhaustion(t *testing.T) {
	r, now := newAffinityRouter(t, "")
	sel, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Key != "k1" {
		t.Fatalf("expected k1, got %s", sel.Key)
	}
	r.Release(sel, 0)

	if _, err := r.Select("flash", "sess", nil); err == nil {
		t.Fatal("expected no route after exhausting the only key")
	}
	if _, _, _, ok := r.sticky.Get("sess", now); ok {
		t.Fatal("quota rebind must forget the pin on exhaustion")
	}
}

func TestAffinityRebindAnyKeepsPinOnExhaustion(t *testing.T) {
	r, now := newAffinityRouter(t, "any")
	sel, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Key != "k1" {
		t.Fatalf("expected k1, got %s", sel.Key)
	}
	r.Release(sel, 0)
	if _, err := r.Select("flash", "sess", nil); err == nil {
		t.Fatal("expected no route after exhausting the only key")
	}
	if p, k, _, ok := r.sticky.Get("sess", now); !ok || p != "p1" || k != "k1" {
		t.Fatalf("rebind_on=any must keep the pin, got %s/%s ok=%v", p, k, ok)
	}
}

func TestAffinitySubagentsInheritsParentPin(t *testing.T) {
	prov := model.Provider{
		Name:    "p1",
		BaseURL: "https://p1.example/v1",
		Keys:    []model.Key{{ID: "k1", Secret: "s1"}},
	}
	alias := model.Alias{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}
	now := time.Unix(1000, 0)
	reg := registry.New([]model.Provider{prov}, []model.Alias{alias})
	h := health.NewRegistry()
	r := NewRouterWithOpts(reg, h, time.Minute, func() time.Time { return now }, StrategyRoundRobin)
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": keyLimit(1000)}, map[string]int{})
	r.SetSessionAffinity(SessionAffinity{Enabled: true, TTL: time.Minute, RebindOn: "quota", Subagents: true})

	psel, err := r.Select("flash", "sess:parent:flash", nil)
	if err != nil {
		t.Fatal(err)
	}

	csel, err := r.Select("flash", "sess:parent:flash:child", nil)
	if err != nil {
		t.Fatal(err)
	}
	if csel.Key != "k1" {
		t.Fatalf("child session did not inherit parent credential: child=%s", csel.Key)
	}
	r.Release(psel, 0)
	r.Release(csel, 0)
}
