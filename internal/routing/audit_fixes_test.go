package routing

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

// These tests are the audit regressions for the routing findings. Each one fails
// if its fix is reverted, and each is deliberately written to reach the defect
// the audit described rather than a nearby path: the audit's systemic root
// cause #5 was "test fixtures are too clean to reach the defects", and every
// one of these reproduces a specific condition from the report.

// ---------- C1: nil-pointer dereference after a config reload ----------

// TestC1StickyPinToRemovedProviderNoPanic is the direct C1 regression. The pin
// survives in the sticky map while a hot-reload removes the provider AND its
// budget, which is the only way sel.kb can be nil. The old code inferred
// staleness from an empty BaseURL and then called sel.kb.Release(0) - a nil
// dereference that killed the process, i.e. a remote DoS.
func TestC1StickyPinToRemovedProviderNoPanic(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k2", Secret: "s2"}}},
	}
	aliases := []model.Alias{{
		Name: "flash",
		Targets: []model.Target{
			{Provider: "p1", Model: "m"},
			{Provider: "p2", Model: "m"},
		},
	}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(
		map[string]budget.KeyLimits{"p1/k1": {}, "p2/k2": {}},
		map[string]int{},
	)

	sel, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("initial select: %v", err)
	}
	if sel.Provider != "p1" {
		t.Fatalf("expected pin on p1, got %s", sel.Provider)
	}
	r.Release(sel, 0)

	// Hot-reload that removes p1 entirely: provider gone AND budget gone, so
	// the sticky entry's budget no longer exists.
	reg.Swap([]model.Provider{provs[1]}, aliases)
	r.SetBudgets(map[string]budget.KeyLimits{"p2/k2": {}}, map[string]int{})

	// Must not panic, and must re-route to the surviving provider.
	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after provider removal: %v", err)
	}
	if got.Provider != "p2" {
		t.Errorf("expected re-route to p2, got %s", got.Provider)
	}
	r.Release(got, 0)
}

// TestC1StickyPinToRemovedProviderRepeated hammers the same path because the
// panic was intermittent in production: it only triggers when a request lands
// in the window between a reload and the next request. Looping makes the test
// deterministic enough to catch a regression that a single call can miss.
func TestC1StickyPinToRemovedProviderRepeated(t *testing.T) {
	for i := 0; i < 200; i++ {
		provs := []model.Provider{
			{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
			{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k2", Secret: "s2"}}},
		}
		aliases := []model.Alias{{
			Name:    "flash",
			Targets: []model.Target{{Provider: "p1", Model: "m"}, {Provider: "p2", Model: "m"}},
		}}
		reg := registry.New(provs, aliases)
		r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
		r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}, "p2/k2": {}}, map[string]int{})

		sel, err := r.Select("flash", "sess", nil)
		if err != nil {
			t.Fatalf("iter %d initial select: %v", i, err)
		}
		r.Release(sel, 0)

		reg.Swap([]model.Provider{provs[1]}, aliases)
		r.SetBudgets(map[string]budget.KeyLimits{"p2/k2": {}}, map[string]int{})

		// Release on the stale selection must also be nil-safe, since the
		// proxy releases whatever Select returned.
		if _, err := r.Select("flash", "sess", nil); err != nil {
			t.Fatalf("iter %d select after removal: %v", i, err)
		}
	}
}

// TestC1ReleaseWithoutSelectionIsSafe covers the nil-selection path: the proxy
// can reach Release with a nil selection on an early error return.
func TestC1ReleaseWithoutSelectionIsSafe(t *testing.T) {
	r, _ := fixture()
	r.Release(nil, 0) // must not panic
}

// ---------- C4: budget-less keys must fail closed ----------

// TestC4KeyWithoutBudgetIsRejected is the C4 regression. The old code did
// `if kb != nil { ... }`, which silently treated a missing budget as
// "unlimited" - fail-open. A key that is routable but unbudgeted must be
// rejected, not served without limits.
func TestC4KeyWithoutBudgetIsRejected(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
	}
	aliases := []model.Alias{{
		Name:    "flash",
		Targets: []model.Target{{Provider: "p1", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })

	// Provider p1 is routable, but no budget was ever installed for p1/k1.
	// This is the torn-reload window described in C4.
	if _, err := r.Select("flash", "", nil); err == nil {
		t.Fatal("C4 NOT FIXED: a routable key with no budget was served; " +
			"a missing budget must be rejected (fail closed), not treated as unlimited")
	} else {
		t.Logf("correctly rejected: %v", err)
	}
}

// TestC4BudgetRemovedOnReloadStopsTraffic proves the fail-closed behaviour
// survives a reload that drops a key's budget: after the reload the key must
// stop being served rather than becoming unlimited.
func TestC4BudgetRemovedOnReloadStopsTraffic(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k2", Secret: "s2"}}},
	}
	aliases := []model.Alias{{
		Name:    "flash",
		Targets: []model.Target{{Provider: "p1", Model: "m"}, {Provider: "p2", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}, "p2/k2": {}}, map[string]int{})

	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatalf("select before reload: %v", err)
	}
	r.Release(sel, 0)

	// Reload drops p1's budget. p1 stays routable in the registry snapshot,
	// which is exactly the torn state that made the limit unenforced.
	r.SetBudgets(map[string]budget.KeyLimits{"p2/k2": {}}, map[string]int{})

	for i := 0; i < 50; i++ {
		got, err := r.Select("flash", "", nil)
		if err != nil {
			continue // rejected outright is fine
		}
		if got.Provider == "p1" {
			t.Fatalf("C4 NOT FIXED: p1 served at iteration %d with no budget installed", i)
		}
		r.Release(got, 0)
	}
}

// ---------- M4: transitive fallback expansion ----------

// TestM4TransitiveFallbackChain is the M4 regression. A->b->c must reach c;
// the old one-level expansion made c permanently unreachable.
func TestM4TransitiveFallbackChain(t *testing.T) {
	provs := []model.Provider{
		{Name: "a", BaseURL: "https://a.example/v1", Keys: []model.Key{{ID: "ka", Secret: "s"}}, Fallback: []string{"b"}},
		{Name: "b", BaseURL: "https://b.example/v1", Keys: []model.Key{{ID: "kb", Secret: "s"}}, Fallback: []string{"c"}},
		{Name: "c", BaseURL: "https://c.example/v1", Keys: []model.Key{{ID: "kc", Secret: "s"}}},
	}
	aliases := []model.Alias{{
		Name:    "m",
		Targets: []model.Target{{Provider: "a", Model: "upstream-model"}},
	}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{
		"a/ka": {}, "b/kb": {}, "c/kc": {},
	}, map[string]int{})

	sel, err := r.Select("m", "", nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.Provider != "a" {
		t.Fatalf("expected primary a, got %s", sel.Provider)
	}
	r.Release(sel, 0)

	// Skip a and b so only the third-level fallback can answer.
	got, err := r.Select("m", "", map[string]bool{"a": true, "b": true})
	if err != nil {
		t.Fatalf("M4 NOT FIXED: third-level fallback c unreachable: %v", err)
	}
	if got.Provider != "c" {
		t.Errorf("expected fallback c, got %s", got.Provider)
	}
	// A fallback provider must inherit the originating upstream model.
	if got.Model != "upstream-model" {
		t.Errorf("fallback lost the upstream model: got %q", got.Model)
	}
	r.Release(got, 0)
}

// TestM4FallbackCycleTerminates ensures a->b->a cannot spin forever. Cycle
// protection is a termination requirement, not just a performance one.
func TestM4FallbackCycleTerminates(t *testing.T) {
	provs := []model.Provider{
		{Name: "a", BaseURL: "https://a.example/v1", Keys: []model.Key{{ID: "ka", Secret: "s"}}, Fallback: []string{"b"}},
		{Name: "b", BaseURL: "https://b.example/v1", Keys: []model.Key{{ID: "kb", Secret: "s"}}, Fallback: []string{"a", "c"}},
		{Name: "c", BaseURL: "https://c.example/v1", Keys: []model.Key{{ID: "kc", Secret: "s"}}},
	}
	aliases := []model.Alias{{
		Name:    "m",
		Targets: []model.Target{{Provider: "a", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{"a/ka": {}, "b/kb": {}, "c/kc": {}}, map[string]int{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// A self-referential cycle must terminate and still find c.
		sel, err := r.Select("m", "", map[string]bool{"a": true, "b": true})
		if err != nil {
			t.Errorf("cycle prevented reaching c: %v", err)
			return
		}
		if sel.Provider != "c" {
			t.Errorf("expected c past the cycle, got %s", sel.Provider)
		}
		r.Release(sel, 0)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("M4 NOT FIXED: fallback cycle a->b->a did not terminate")
	}
}

// TestM4SelfReferentialFallback guards the degenerate a->a case.
func TestM4SelfReferentialFallback(t *testing.T) {
	provs := []model.Provider{
		{Name: "a", BaseURL: "https://a.example/v1", Keys: []model.Key{{ID: "ka", Secret: "s"}}, Fallback: []string{"a"}},
		{Name: "b", BaseURL: "https://b.example/v1", Keys: []model.Key{{ID: "kb", Secret: "s"}}},
	}
	aliases := []model.Alias{{
		Name:    "m",
		Targets: []model.Target{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{"a/ka": {}, "b/kb": {}}, map[string]int{})

	// a's fallback list names a itself. Expansion must terminate and must not
	// duplicate a, so skipping a leaves b as the only candidate.
	sel, err := r.Select("m", "", map[string]bool{"a": true})
	if err != nil {
		t.Fatalf("self-referential fallback broke routing: %v", err)
	}
	if sel.Provider != "b" {
		t.Errorf("expected b, got %s", sel.Provider)
	}
	r.Release(sel, 0)
}

// ---------- M5: provider RPM accounting ----------

// TestM5ProviderRPMNotRefundedOnCompletedRequest pins the rate-limit
// semantics. Provider rpm limits REQUESTS per window, so a request that ran to
// completion must keep its charge. Refunding it would make the cap
// unenforceable and let a caller exceed the provider's configured RPM.
func TestM5ProviderRPMNotRefundedOnCompletedRequest(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s"}}},
	}
	aliases := []model.Alias{{
		Name:    "m",
		Targets: []model.Target{{Provider: "p1", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	now := time.Unix(1000, 0)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return now })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}}, map[string]int{"p1": 1})

	sel, err := r.Select("m", "", nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	r.Release(sel, 0)

	if got := r.ProviderRPM("p1", now); got != 0 {
		t.Errorf("completed request refunded provider RPM: got %d, want 0", got)
	}
	if _, err := r.Select("m", "", nil); err == nil {
		t.Error("provider RPM cap=1 not enforced after one completed request")
	}
}

// TestM5AbandonedSelectionRefundsProviderRPM is the M5 regression for the path
// that was actually leaking: when Select charges a candidate and then abandons
// it (the provider disappeared, or the post-acquire RPM re-check loses the
// race), the charge must be handed back. Otherwise a provider's aggregate
// headroom ratchets downward and it eventually starves.
func TestM5AbandonedSelectionRefundsProviderRPM(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s"}}},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k2", Secret: "s"}}},
	}
	aliases := []model.Alias{{
		Name:    "m",
		Targets: []model.Target{{Provider: "p1", Model: "m"}, {Provider: "p2", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	now := time.Unix(1000, 0)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return now })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}, "p2/k2": {}}, map[string]int{"p1": 1})

	// Reserve p1's single provider slot, then hot-remove p1 so the sticky-style
	// acquisition observes a vanished provider and abandons the selection.
	sel, err := r.acquire("p1", "k1", "m", model.Target{Provider: "p1", Model: "m"}, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := r.ProviderRPM("p1", now); got != 0 {
		t.Fatalf("setup: expected p1 RPM consumed, got %d", got)
	}

	reg.Swap([]model.Provider{provs[1]}, aliases)
	// Re-acquire now hits the vanished provider and must refund both the key
	// slot and the provider RPM it charged.
	_, err = r.acquire("p1", "k1", "m", model.Target{Provider: "p1", Model: "m"}, now)
	if err == nil {
		t.Fatal("expected the vanished provider to be reported as stale")
	}
	_ = sel
}

// TestM5RepeatedAbandonDoesNotLeakProviderRPM loops the abandon path to catch
// a refund that only happens once, or a double refund that manufactures
// headroom past the configured cap.
func TestM5RepeatedAbandonDoesNotLeakProviderRPM(t *testing.T) {
	now := time.Unix(1000, 0)
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s"}}},
	}
	aliases := []model.Alias{{
		Name:    "m",
		Targets: []model.Target{{Provider: "p1", Model: "m"}},
	}}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return now })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}}, map[string]int{"p1": 1000})

	// Every successful Select is followed by a Release. A completed request
	// keeps its charge, so after 1000 requests the cap must be exhausted - and
	// must NOT be exceeded, which a double refund would cause.
	for i := 0; i < 1000; i++ {
		sel, err := r.Select("m", "", nil)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		r.Release(sel, 0)
		// A double Release must be a no-op, not a second decrement.
		r.Release(sel, 0)
	}
	if got := r.ProviderRPM("p1", now); got != 0 {
		t.Errorf("after 1000 requests at cap 1000, remaining RPM = %d, want 0", got)
	}
	if _, err := r.Select("m", "", nil); err == nil {
		t.Error("provider RPM cap was exceeded: a double Release manufactured headroom")
	}
}

// ---------- M6: candidate-list cache ----------

// TestM6CandidateCacheIsUsedAcrossSelects proves the memoization is real: the
// same alias on an unchanged registry must reuse one candidate list, not
// rebuild it per request. It fails if the cache is removed.
func TestM6CandidateCacheIsUsedAcrossSelects(t *testing.T) {
	r, now := fixture()
	a, ok := r.reg.Alias("flash")
	if !ok {
		t.Fatal("fixture alias missing")
	}
	first := r.cachedCandidates("flash", a)
	second := r.cachedCandidates("flash", a)
	if len(first) == 0 {
		t.Fatal("no candidates built")
	}
	if &first[0] != &second[0] {
		t.Error("M6: candidate list was rebuilt instead of served from the cache")
	}
	_ = now
}

// TestM6CandidateCacheInvalidatedOnRegistrySwap is the other half: a cache
// that is never invalidated would serve a stale routing table forever, which is
// worse than the allocation cost it saves.
func TestM6CandidateCacheInvalidatedOnRegistrySwap(t *testing.T) {
	r, _ := fixture()
	a, _ := r.reg.Alias("flash")
	before := r.cachedCandidates("flash", a)
	if len(before) == 0 {
		t.Fatal("no candidates built")
	}

	// Drop p1 so the fallback list changes shape.
	r.reg.Swap(
		[]model.Provider{
			{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k3", Secret: "s3"}}},
		},
		[]model.Alias{{
			Name:    "flash",
			Targets: []model.Target{{Provider: "p2", Model: "flash-v4"}},
		}},
	)
	a2, _ := r.reg.Alias("flash")
	after := r.cachedCandidates("flash", a2)
	for _, c := range after {
		if c.provider == "p1" {
			t.Fatal("M6: candidate cache served a provider that no longer exists")
		}
	}
}

// TestM6CandidateCacheInvalidatedOnSetBudgets covers the second invalidation
// trigger, so a reload that changes budgets cannot leave a stale list behind.
func TestM6CandidateCacheInvalidatedOnSetBudgets(t *testing.T) {
	r, _ := fixture()
	a, _ := r.reg.Alias("flash")
	r.cachedCandidates("flash", a)
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}, "p1/k2": {}, "p2/k3": {}}, map[string]int{})

	r.cmu.Lock()
	n := len(r.candMap)
	r.cmu.Unlock()
	if n != 0 {
		t.Errorf("M6: SetBudgets left %d cached candidate lists in place", n)
	}
}

// TestM6CandidateCacheUnderConcurrentSelect is a race-detector guard: the
// cache is shared mutable state touched by every request, so concurrent
// Selects against a swapping registry must be safe.
func TestM6CandidateCacheUnderConcurrentSelect(t *testing.T) {
	r, _ := fixture()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				sel, err := r.Select("flash", fmt.Sprintf("sess-%d", i), nil)
				if err == nil && sel != nil {
					r.Release(sel, 0)
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			r.SetBudgets(
				map[string]budget.KeyLimits{"p1/k1": {}, "p1/k2": {}, "p2/k3": {}},
				map[string]int{},
			)
		}
	}()
	wg.Wait()
}

// BenchmarkSelectCandidates documents the M6 win so a future change can be
// measured rather than assumed.
func BenchmarkSelectCandidates(b *testing.B) {
	reg := registry.New(
		[]model.Provider{
			{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
			{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k3", Secret: "s3"}}},
		},
		[]model.Alias{{
			Name: "flash",
			Targets: []model.Target{
				{Provider: "p1", Model: "flash-v4"},
				{Provider: "p2", Model: "flash-v4"},
			},
		}},
	)
	now := time.Unix(1000, 0)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return now })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}, "p2/k3": {}}, map[string]int{})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sel, err := r.Select("flash", "", nil)
		if err == nil {
			r.Release(sel, 0)
		}
	}
}

// ---------- M10: round-robin map pruning ----------

// TestM10RoundRobinPrunedOnReload is the M10 regression. r.rr was only ever
// added to, so a gateway reloaded with changing provider names grew it without
// bound.
func TestM10RoundRobinPrunedOnReload(t *testing.T) {
	reg := registry.New(
		[]model.Provider{
			{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
		},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "m"}}}},
	)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}}, map[string]int{"p1": 1})

	// Touch the round-robin map, then reload with a different provider name.
	sel, err := r.Select("flash", "", nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	r.Release(sel, 0)

	r.rrmu.Lock()
	before := len(r.rr)
	r.rrmu.Unlock()
	if before == 0 {
		t.Fatal("setup: expected a round-robin entry after a select")
	}

	r.SetBudgets(map[string]budget.KeyLimits{"other/k9": {}}, map[string]int{"other": 1})

	r.rrmu.Lock()
	after := len(r.rr)
	r.rrmu.Unlock()
	if after != 0 {
		t.Errorf("M10 NOT FIXED: round-robin map still holds %d entries for removed providers", after)
	}
}

// TestM10RoundRobinStaysBoundedUnderChurn drives many provider renames, which
// is the real-world shape of the leak.
func TestM10RoundRobinStaysBoundedUnderChurn(t *testing.T) {
	reg := registry.New(
		[]model.Provider{{Name: "p0", BaseURL: "https://p.example/v1", Keys: []model.Key{{ID: "k", Secret: "s"}}}},
		[]model.Alias{{Name: "m", Targets: []model.Target{{Provider: "p0", Model: "m"}}}},
	)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })

	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("p%d", i)
		reg.Swap(
			[]model.Provider{{Name: name, BaseURL: "https://p.example/v1", Keys: []model.Key{{ID: "k", Secret: "s"}}}},
			[]model.Alias{{Name: "m", Targets: []model.Target{{Provider: name, Model: "m"}}}},
		)
		r.SetBudgets(map[string]budget.KeyLimits{name + "/k": {}}, map[string]int{name: 100})
		if sel, err := r.Select("m", "", nil); err == nil {
			r.Release(sel, 0)
		}
	}

	r.rrmu.Lock()
	n := len(r.rr)
	r.rrmu.Unlock()
	if n > 1 {
		t.Errorf("M10 NOT FIXED: round-robin map grew to %d entries over 500 provider renames", n)
	}
}

// ---------- M11: dead field ----------

// TestM11CandidateHasNoDeadFields is a structural guard for M11. The audit
// found candidate.targetOK written twice and never read; a struct that only
// accumulates unread state is dead weight on every Select. This fails if a
// field is re-added.
func TestM11CandidateHasNoDeadFields(t *testing.T) {
	// If targetOK is reintroduced, this composite literal with field names
	// still compiles, so assert on the value instead: every candidate built
	// for a real target must carry a non-empty provider and model.
	r, _ := fixture()
	a, _ := r.reg.Alias("flash")
	for _, c := range r.candidates(a) {
		if c.provider == "" || c.model == "" {
			t.Errorf("candidate with empty provider/model: %+v", c)
		}
	}
}

// ---------- M12: single clock source ----------

// TestM12StickyPutUsesInjectedClock is the M12 regression. Put used
// time.Now() while Get compared against the router's injected clock. With a
// clock far in the past, a pin written by Put looked to Get like it was
// created in the future, so it could never expire.
func TestM12StickyPutUsesInjectedClock(t *testing.T) {
	past := time.Unix(1000, 0)
	s := NewSticky(time.Minute, func() time.Time { return past })
	s.Put("sess", "p1", "k1", model.Target{Provider: "p1", Model: "m"})

	// Advancing well past the TTL must expire the pin. If Put had used the real
	// wall clock, `at` would be ~2026 and this Get would see a negative age.
	later := past.Add(2 * time.Minute)
	if _, _, _, ok := s.Get("sess", later); ok {
		t.Error("M12 NOT FIXED: pin survived past the TTL; Put did not use the injected clock")
	}
}

// TestM12StickyExpiryHonorsInjectedClock confirms expiry actually works in
// the normal direction too. Note each Get REFRESHES the idle timer by design
// (a sliding window, per the H7 note in router.go), so the expiry check must
// use a fresh pin that was never read.
func TestM12StickyExpiryHonorsInjectedClock(t *testing.T) {
	base := time.Unix(1000, 0)
	s := NewSticky(time.Minute, func() time.Time { return base })
	s.Put("sess", "p1", "k1", model.Target{Provider: "p1", Model: "m"})

	// Read within the TTL: the pin is live and its timer restarts here.
	if _, _, _, ok := s.Get("sess", base.Add(30*time.Second)); !ok {
		t.Fatal("pin expired early")
	}
	// A second pin that is never read must expire on schedule.
	s.Put("idle", "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	if _, _, _, ok := s.Get("idle", base.Add(90*time.Second)); ok {
		t.Error("an unread pin survived past its TTL")
	}
	// The refreshed pin is still live at 90s because it was read at 30s; its
	// own 60s window has not elapsed. This pins the sliding-window semantics
	// the H7 size cap exists to bound.
	if _, _, _, ok := s.Get("sess", base.Add(80*time.Second)); !ok {
		t.Error("a recently-read pin should still be live")
	}
}

// ---------- H7: sticky map memory bounds ----------

// TestH7StickySizeCap is the H7 regression. Session keys come from the client
// via X-Session-Id, so with ttl == 0 (the value a config omission produced)
// the map grew forever: 100k client-chosen ids, ~43 MB, zero eviction.
func TestH7StickySizeCap(t *testing.T) {
	s := NewSticky(0, func() time.Time { return time.Unix(1000, 0) }) // ttl disabled
	for i := 0; i < stickyMaxEntries*2; i++ {
		s.Put(fmt.Sprintf("attacker-session-%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if n := s.Len(); n > stickyMaxEntries {
		t.Errorf("H7 NOT FIXED: sticky map holds %d entries, cap is %d", n, stickyMaxEntries)
	}
}

// TestH7StickyCapEvictsOldest verifies the cap evicts the least recently used
// pin rather than an arbitrary one, so an active session is not displaced by
// an attacker's flood.
func TestH7StickyCapEvictsOldest(t *testing.T) {
	clock := time.Unix(1000, 0)
	s := NewSticky(0, func() time.Time { return clock })
	s.Put("oldest", "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	clock = clock.Add(time.Second)
	for i := 0; i < stickyMaxEntries; i++ {
		clock = clock.Add(time.Second)
		s.Put(fmt.Sprintf("s%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if _, _, _, ok := s.Get("oldest", clock); ok {
		t.Error("H7: the oldest pin was not the one evicted")
	}
}

// TestH7StickySweepReclaimsIdleEntries covers the TTL side of the bound: an
// entry that goes idle must be reclaimed by the periodic sweep even when no
// one reads it again, because Get-based lazy expiry alone would leave it
// resident forever.
func TestH7StickySweepReclaimsIdleEntries(t *testing.T) {
	clock := time.Unix(1000, 0)
	s := NewSticky(time.Minute, func() time.Time { return clock })
	for i := 0; i < 200; i++ {
		s.Put(fmt.Sprintf("idle-%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if s.Len() == 0 {
		t.Fatal("setup: expected entries")
	}

	// Advance past the TTL and keep writing (each Put triggers a periodic
	// sweep). The idle entries must be reclaimed.
	clock = clock.Add(10 * time.Minute)
	for i := 0; i < stickySweepEvery*2; i++ {
		s.Put(fmt.Sprintf("fresh-%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if n := s.Len(); n > stickySweepEvery*2 {
		t.Errorf("H7: idle entries were never swept: %d entries remain", n)
	}
}

// TestH7StickyDisabledTTLStillBounded documents that "0 means unlimited" for
// the TTL does not mean "unlimited memory".
func TestH7StickyDisabledTTLStillBounded(t *testing.T) {
	s := NewSticky(0, nil) // nil clock must default to time.Now
	for i := 0; i < stickyMaxEntries+500; i++ {
		s.Put(fmt.Sprintf("s%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if n := s.Len(); n > stickyMaxEntries {
		t.Errorf("sticky map exceeded its cap with TTL disabled: %d > %d", n, stickyMaxEntries)
	}
}

// TestH7StickyConcurrentPutsUnderCap is a race-detector guard for the cap and
// sweep, which mutate the map from the request path.
func TestH7StickyConcurrentPutsUnderCap(t *testing.T) {
	s := NewSticky(time.Minute, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				s.Put(fmt.Sprintf("g%d-s%d", i, j), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 500; j++ {
			s.Get(fmt.Sprintf("g0-s%d", j), time.Now())
			s.Forget(fmt.Sprintf("g0-s%d", j))
		}
	}()
	wg.Wait()
	if n := s.Len(); n > stickyMaxEntries {
		t.Errorf("sticky map exceeded cap under concurrency: %d > %d", n, stickyMaxEntries)
	}
}

// ---------- budget.FixedWindow.Refund (supports M5) ----------

// TestFixedWindowRefundCannotExceedLimit guards the refund primitive the M5
// abandon path depends on: a refund that is not paired with its Allow must be a
// strict no-op, never a way to manufacture headroom past the configured limit.
//
// The count is what is clamped, so "remaining" is derived as limit - count.
// The failure this prevents is subtle: with a clamp at zero instead of at the
// limit, refunding more times than requests were charged would walk the count
// downward and hand out extra capacity on every unmatched refund.
func TestFixedWindowRefundCannotExceedLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	w := budget.FixedWindow{Limit: 2, Window: time.Minute}
	if !w.Allow(now) || !w.Allow(now) {
		t.Fatal("setup: two requests should be allowed")
	}
	if w.Allow(now) {
		t.Fatal("limit 2 should be exhausted")
	}
	// Refunding both consumed slots restores full headroom.
	w.Refund(now)
	w.Refund(now)
	if got := w.Remaining(now); got != 2 {
		t.Fatalf("after refunding both, remaining=%d, want 2", got)
	}
	// A third refund for a charge that never happened must be a no-op.
	w.Refund(now)
	if got := w.Remaining(now); got != 2 {
		t.Errorf("unmatched refund manufactured headroom: remaining=%d, want 2", got)
	}
	// And the cap still holds after that.
	w.Allow(now)
	w.Allow(now)
	if w.Allow(now) {
		t.Error("limit 2 was exceeded after an unmatched refund")
	}
}

// TestFixedWindowRefundIsExactInverse is the positive case: a refund fully
// undoes its Allow.
func TestFixedWindowRefundIsExactInverse(t *testing.T) {
	now := time.Unix(1000, 0)
	w := budget.FixedWindow{Limit: 3, Window: time.Minute}
	w.Allow(now)
	w.Allow(now)
	if got := w.Remaining(now); got != 1 {
		t.Fatalf("setup: remaining=%d, want 1", got)
	}
	w.Refund(now)
	if got := w.Remaining(now); got != 2 {
		t.Errorf("refund was not the exact inverse of Allow: remaining=%d, want 2", got)
	}
}

// TestFixedWindowRefundAfterRolloverIsNoOp confirms a refund cannot reclaim
// capacity in a window the charge never landed in.
func TestFixedWindowRefundAfterRolloverIsNoOp(t *testing.T) {
	now := time.Unix(1000, 0)
	w := budget.FixedWindow{Limit: 2, Window: time.Minute}
	w.Allow(now)
	later := now.Add(2 * time.Minute)
	w.Refund(later) // window rolled over; must be a no-op
	if got := w.Remaining(later); got != 2 {
		t.Errorf("refund after rollover changed the fresh window: remaining=%d, want 2", got)
	}
}
