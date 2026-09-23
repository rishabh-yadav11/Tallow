package registry

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// Fixture builders return fresh values on every call so tests never share
// mutable backing arrays with each other or with the registry.

func providerA1() model.Provider {
	return model.Provider{
		Name:           "alpha1",
		BaseURL:        "https://alpha1.example/v1",
		Fallback:       []string{"alpha2"},
		HealthCheck:    true,
		HealthInterval: 30 * time.Second,
		RPM:            60,
		Timeout:        10 * time.Second,
		Keys: []model.Key{{
			ID:             "alpha1-key0",
			Secret:         "sk-alpha1",
			Ref:            "keystore://alpha1/0",
			RPM:            120,
			MaxRequests:    1000,
			Window:         time.Minute,
			MaxConcurrent:  5,
			CostLimitCents: 500,
		}},
	}
}

func providerA2() model.Provider {
	return model.Provider{
		Name:           "alpha2",
		BaseURL:        "https://alpha2.example/v1",
		HealthCheck:    false,
		HealthInterval: time.Minute,
		RPM:            30,
		Timeout:        5 * time.Second,
		Keys: []model.Key{{
			ID:             "alpha2-key0",
			Secret:         "sk-alpha2",
			Ref:            "keystore://alpha2/0",
			MaxConcurrent:  2,
			CostLimitCents: 100,
		}},
	}
}

func providerB1() model.Provider {
	return model.Provider{
		Name:           "beta1",
		BaseURL:        "https://beta1.example/v1",
		Fallback:       []string{"beta2"},
		HealthCheck:    true,
		HealthInterval: 15 * time.Second,
		RPM:            90,
		Timeout:        20 * time.Second,
		Keys: []model.Key{{
			ID:             "beta1-key0",
			Secret:         "sk-beta1",
			Ref:            "keystore://beta1/0",
			RPM:            240,
			MaxRequests:    500,
			Window:         2 * time.Minute,
			MaxConcurrent:  8,
			CostLimitCents: 2500,
		}},
	}
}

func providerB2() model.Provider {
	return model.Provider{
		Name:           "beta2",
		BaseURL:        "https://beta2.example/v1",
		HealthCheck:    false,
		HealthInterval: 45 * time.Second,
		RPM:            15,
		Timeout:        3 * time.Second,
		Keys: []model.Key{{
			ID:             "beta2-key0",
			Secret:         "sk-beta2",
			Ref:            "keystore://beta2/0",
			MaxRequests:    50,
			Window:         time.Hour,
			MaxConcurrent:  1,
			CostLimitCents: 10,
		}},
	}
}

func aliasA1() model.Alias {
	return model.Alias{
		Name: "model-a1",
		Targets: []model.Target{
			{
				Provider:       "alpha1",
				Model:          "gpt-a1",
				ContextWindow:  128000,
				SupportsTools:  true,
				SupportsStream: true,
				PriceInputPerM: 0.5, PriceOutputPerM: 1.5,
			},
			{
				Provider:       "alpha2",
				Model:          "gpt-a1-fallback",
				ContextWindow:  64000,
				SupportsVision: true,
				PriceInputPerM: 0.25, PriceOutputPerM: 0.75,
			},
		},
	}
}

func aliasA2() model.Alias {
	return model.Alias{
		Name: "model-a2",
		Targets: []model.Target{{
			Provider:       "alpha2",
			Model:          "gpt-a2",
			ContextWindow:  32000,
			SupportsStream: true,
			PriceInputPerM: 1.0, PriceOutputPerM: 2.0,
		}},
	}
}

func aliasA3() model.Alias {
	return model.Alias{
		Name: "model-a3",
		Targets: []model.Target{{
			Provider:      "alpha1",
			Model:         "gpt-a3",
			ContextWindow: 8192,
		}},
	}
}

func aliasB1() model.Alias {
	return model.Alias{
		Name: "model-b1",
		Targets: []model.Target{
			{
				Provider:       "beta1",
				Model:          "gpt-b1",
				ContextWindow:  200000,
				SupportsTools:  true,
				SupportsVision: true,
				SupportsStream: true,
				PriceInputPerM: 3.0, PriceOutputPerM: 15.0,
			},
			{
				Provider:       "beta2",
				Model:          "gpt-b1-fallback",
				ContextWindow:  16000,
				PriceInputPerM: 0.1, PriceOutputPerM: 0.2,
			},
		},
	}
}

func aliasB2() model.Alias {
	return model.Alias{
		Name: "model-b2",
		Targets: []model.Target{{
			Provider:       "beta2",
			Model:          "gpt-b2",
			ContextWindow:  4096,
			SupportsTools:  true,
			PriceInputPerM: 0.05, PriceOutputPerM: 0.1,
		}},
	}
}

func providerSetA() []model.Provider { return []model.Provider{providerA1(), providerA2()} }
func providerSetB() []model.Provider { return []model.Provider{providerB1(), providerB2()} }
func aliasSetA() []model.Alias       { return []model.Alias{aliasA1(), aliasA2()} }
func aliasSetB() []model.Alias       { return []model.Alias{aliasB1(), aliasB2()} }

func TestNewAndLookups(t *testing.T) {
	r := New(providerSetA(), aliasSetA())

	providerRows := []struct {
		name string
		want model.Provider
		ok   bool
	}{
		{name: "alpha1", want: providerA1(), ok: true},
		{name: "alpha2", want: providerA2(), ok: true},
		{name: "missing", ok: false},
		{name: "", ok: false},
	}
	for _, tc := range providerRows {
		t.Run("provider/"+tc.name, func(t *testing.T) {
			got, ok := r.Provider(tc.name)
			if ok != tc.ok {
				t.Fatalf("Provider(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			}
			if !ok {
				if got != nil {
					t.Fatalf("Provider(%q) returned non-nil pointer for unknown name", tc.name)
				}
				return
			}
			if got == nil {
				t.Fatalf("Provider(%q) returned nil pointer with ok=true", tc.name)
			}
			if !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("Provider(%q) = %+v, want %+v", tc.name, *got, tc.want)
			}
		})
	}

	aliasRows := []struct {
		name string
		want model.Alias
		ok   bool
	}{
		{name: "model-a1", want: aliasA1(), ok: true},
		{name: "model-a2", want: aliasA2(), ok: true},
		{name: "missing", ok: false},
		{name: "", ok: false},
	}
	for _, tc := range aliasRows {
		t.Run("alias/"+tc.name, func(t *testing.T) {
			got, ok := r.Alias(tc.name)
			if ok != tc.ok {
				t.Fatalf("Alias(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			}
			if !ok {
				if got != nil {
					t.Fatalf("Alias(%q) returned non-nil pointer for unknown name", tc.name)
				}
				return
			}
			if got == nil {
				t.Fatalf("Alias(%q) returned nil pointer with ok=true", tc.name)
			}
			if !reflect.DeepEqual(*got, tc.want) {
				t.Fatalf("Alias(%q) = %+v, want %+v", tc.name, *got, tc.want)
			}
		})
	}
}

func TestSwapReplacesSnapshot(t *testing.T) {
	r := New(providerSetA(), aliasSetA())

	// Pre-state sanity: the incoming names must not resolve before the swap.
	if p, ok := r.Provider("beta1"); ok || p != nil {
		t.Fatalf("beta1 resolved before Swap: %+v, %v", p, ok)
	}
	if a, ok := r.Alias("model-b1"); ok || a != nil {
		t.Fatalf("model-b1 resolved before Swap: %+v, %v", a, ok)
	}

	r.Swap(providerSetB(), aliasSetB())

	for _, name := range []string{"alpha1", "alpha2"} {
		if p, ok := r.Provider(name); ok || p != nil {
			t.Fatalf("Provider(%q) still resolves after Swap: %+v, %v", name, p, ok)
		}
	}
	for _, name := range []string{"model-a1", "model-a2"} {
		if a, ok := r.Alias(name); ok || a != nil {
			t.Fatalf("Alias(%q) still resolves after Swap: %+v, %v", name, a, ok)
		}
	}

	wantProviders := map[string]model.Provider{
		"beta1": providerB1(),
		"beta2": providerB2(),
	}
	gotProviders := r.Providers()
	if len(gotProviders) != len(wantProviders) {
		t.Fatalf("Providers() len = %d, want %d", len(gotProviders), len(wantProviders))
	}
	for _, got := range gotProviders {
		want, ok := wantProviders[got.Name]
		if !ok {
			t.Fatalf("Providers() returned unexpected provider %q", got.Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Providers()[%q] = %+v, want %+v", got.Name, got, want)
		}
	}

	wantAliases := map[string]model.Alias{
		"model-b1": aliasB1(),
		"model-b2": aliasB2(),
	}
	gotAliases := r.Aliases()
	if len(gotAliases) != len(wantAliases) {
		t.Fatalf("Aliases() len = %d, want %d", len(gotAliases), len(wantAliases))
	}
	for _, got := range gotAliases {
		want, ok := wantAliases[got.Name]
		if !ok {
			t.Fatalf("Aliases() returned unexpected alias %q", got.Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Aliases()[%q] = %+v, want %+v", got.Name, got, want)
		}
	}

	for _, name := range []string{"beta1", "beta2"} {
		p, ok := r.Provider(name)
		if !ok || p == nil {
			t.Fatalf("Provider(%q) not resolvable after Swap (ok=%v)", name, ok)
		}
	}
	for _, name := range []string{"model-b1", "model-b2"} {
		a, ok := r.Alias(name)
		if !ok || a == nil {
			t.Fatalf("Alias(%q) not resolvable after Swap (ok=%v)", name, ok)
		}
	}
}

func TestSwapClearsOnEmpty(t *testing.T) {
	r := New(providerSetA(), aliasSetA())
	r.Swap(nil, nil)

	if p, ok := r.Provider("alpha1"); ok || p != nil {
		t.Fatalf("Provider after clearing swap: %+v, %v", p, ok)
	}
	if a, ok := r.Alias("model-a1"); ok || a != nil {
		t.Fatalf("Alias after clearing swap: %+v, %v", a, ok)
	}
	if got := r.Providers(); len(got) != 0 {
		t.Fatalf("Providers() len = %d, want 0 after clearing swap", len(got))
	}
	if got := r.Aliases(); len(got) != 0 {
		t.Fatalf("Aliases() len = %d, want 0 after clearing swap", len(got))
	}
	if got := r.AliasNames(); len(got) != 0 {
		t.Fatalf("AliasNames() len = %d, want 0 after clearing swap", len(got))
	}
}

func TestProvidersCopySemantics(t *testing.T) {
	r := New(providerSetA(), aliasSetA())

	got := r.Providers()
	if len(got) != 2 {
		t.Fatalf("Providers() len = %d, want 2", len(got))
	}
	mutated := false
	for i := range got {
		if got[i].Name == "alpha1" {
			got[i].BaseURL = "https://tampered.example/v1"
			got[i].RPM = 999999
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("Providers() did not contain alpha1; fixture broken")
	}

	// The registry's stored value must be untouched by the mutation above.
	p, ok := r.Provider("alpha1")
	if !ok || p == nil {
		t.Fatalf("Provider(alpha1) not found after mutating returned copy")
	}
	if !reflect.DeepEqual(*p, providerA1()) {
		t.Fatalf("Provider(alpha1) leaked mutation from Providers() copy: %+v", *p)
	}
	for _, again := range r.Providers() {
		if again.Name == "alpha1" && !reflect.DeepEqual(again, providerA1()) {
			t.Fatalf("Providers() leaked mutation across calls: %+v", again)
		}
	}
}

func TestAliasesCopySemantics(t *testing.T) {
	r := New(providerSetA(), aliasSetA())

	got := r.Aliases()
	if len(got) != 2 {
		t.Fatalf("Aliases() len = %d, want 2", len(got))
	}
	mutated := false
	for i := range got {
		if got[i].Name == "model-a1" {
			got[i].Name = "tampered-alias"
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("Aliases() did not contain model-a1; fixture broken")
	}

	a, ok := r.Alias("model-a1")
	if !ok || a == nil {
		t.Fatalf("Alias(model-a1) not found after mutating returned copy")
	}
	if !reflect.DeepEqual(*a, aliasA1()) {
		t.Fatalf("Alias(model-a1) leaked mutation from Aliases() copy: %+v", *a)
	}
	for _, again := range r.Aliases() {
		if again.Name == "model-a1" && !reflect.DeepEqual(again, aliasA1()) {
			t.Fatalf("Aliases() leaked mutation across calls: %+v", again)
		}
	}
}

func TestSwapCopiesCallerSlices(t *testing.T) {
	// New path: mutating the caller's slice after construction must not
	// change what the registry serves. Fails if New/Swap stored &providers[i].
	callers := providerSetA()
	r := New(callers, aliasSetA())
	callers[0].BaseURL = "https://tampered.example/v1"
	callers[0].RPM = 999999
	p, ok := r.Provider("alpha1")
	if !ok || p == nil {
		t.Fatalf("Provider(alpha1) missing after caller mutated input slice")
	}
	if !reflect.DeepEqual(*p, providerA1()) {
		t.Fatalf("New leaked caller slice mutation into registry: %+v", *p)
	}

	// Swap path: same guarantee for the slice handed to Swap.
	r2 := New(providerSetA(), aliasSetA())
	incoming := providerSetB()
	r2.Swap(incoming, aliasSetB())
	incoming[1].Timeout = 999 * time.Hour
	p2, ok := r2.Provider("beta2")
	if !ok || p2 == nil {
		t.Fatalf("Provider(beta2) missing after caller mutated Swap input")
	}
	if !reflect.DeepEqual(*p2, providerB2()) {
		t.Fatalf("Swap leaked caller slice mutation into registry: %+v", *p2)
	}

	// Alias input slices get the same treatment.
	aliasCallers := aliasSetA()
	r3 := New(providerSetA(), aliasCallers)
	aliasCallers[0].Name = "tampered-alias"
	a, ok := r3.Alias("model-a1")
	if !ok || a == nil {
		t.Fatalf("Alias(model-a1) missing after caller mutated alias input slice")
	}
	if !reflect.DeepEqual(*a, aliasA1()) {
		t.Fatalf("New leaked caller alias-slice mutation into registry: %+v", *a)
	}
}

func TestAliasNames(t *testing.T) {
	r := New(providerSetA(), []model.Alias{aliasA1(), aliasA2(), aliasA3()})

	got := r.AliasNames()
	want := map[string]bool{"model-a1": true, "model-a2": true, "model-a3": true}
	if len(got) != len(want) {
		t.Fatalf("AliasNames() len = %d, want %d: %q", len(got), len(want), got)
	}
	remaining := make(map[string]bool, len(want))
	for k := range want {
		remaining[k] = true
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("AliasNames() returned unexpected name %q", name)
		}
		delete(remaining, name)
	}
	if len(remaining) != 0 {
		for name := range remaining {
			t.Errorf("AliasNames() missing %q", name)
		}
		t.FailNow()
	}
}

func TestConcurrentSwapAndReads(t *testing.T) {
	r := New(providerSetA(), aliasSetA())

	const swaps = 1000
	const readers = 4

	done := make(chan struct{})
	var violations atomic.Int64
	var sawSetA, sawSetB atomic.Bool

	provNamesA := map[string]bool{"alpha1": true, "alpha2": true}
	provNamesB := map[string]bool{"beta1": true, "beta2": true}
	aliasNamesA := map[string]bool{"model-a1": true, "model-a2": true}
	aliasNamesB := map[string]bool{"model-b1": true, "model-b2": true}

	allIn := func(names []string, set map[string]bool) bool {
		for _, n := range names {
			if !set[n] {
				return false
			}
		}
		return true
	}
	// A single read of the full snapshot must never mix the two swapped-in
	// sets: Swap replaces both maps under one lock, so one atomic read sees
	// exactly one partition.
	onePartition := func(names []string, a, b map[string]bool) bool {
		return allIn(names, a) || allIn(names, b)
	}
	provNames := func(ps []model.Provider) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, p.Name)
		}
		return out
	}

	go func() {
		defer close(done)
		pa, pb := providerSetA(), providerSetB()
		aa, ab := aliasSetA(), aliasSetB()
		for i := 0; i < swaps; i++ {
			if i%2 == 0 {
				r.Swap(pb, ab)
			} else {
				r.Swap(pa, aa)
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, name := range []string{"alpha1", "beta1"} {
					if p, ok := r.Provider(name); ok && (p == nil || p.Name != name) {
						violations.Add(1)
					}
				}
				for _, name := range []string{"model-a1", "model-b1"} {
					if a, ok := r.Alias(name); ok && (a == nil || a.Name != name) {
						violations.Add(1)
					}
				}
				if ps := r.Providers(); len(ps) != 2 {
					violations.Add(1)
				} else {
					names := provNames(ps)
					if !onePartition(names, provNamesA, provNamesB) {
						violations.Add(1)
					} else if allIn(names, provNamesA) {
						sawSetA.Store(true)
					} else {
						sawSetB.Store(true)
					}
				}
				if as := r.Aliases(); len(as) != 2 {
					violations.Add(1)
				}
				if ans := r.AliasNames(); len(ans) != 2 {
					violations.Add(1)
				} else if !onePartition(ans, aliasNamesA, aliasNamesB) {
					violations.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if n := violations.Load(); n != 0 {
		t.Fatalf("%d invariant violations during concurrent Swap/reads", n)
	}
	// Soft check that the run genuinely interleaved both snapshots; not a
	// correctness requirement, so only logged.
	if !sawSetA.Load() || !sawSetB.Load() {
		t.Logf("warning: readers only observed one snapshot set (A=%v B=%v)", sawSetA.Load(), sawSetB.Load())
	}
}
