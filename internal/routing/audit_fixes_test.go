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

	reg.Swap([]model.Provider{provs[1]}, aliases)
	r.SetBudgets(map[string]budget.KeyLimits{"p2/k2": {}}, map[string]int{})

	got, err := r.Select("flash", "sess", nil)
	if err != nil {
		t.Fatalf("select after provider removal: %v", err)
	}
	if got.Provider != "p2" {
		t.Errorf("expected re-route to p2, got %s", got.Provider)
	}
	r.Release(got, 0)
}

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

		if _, err := r.Select("flash", "sess", nil); err != nil {
			t.Fatalf("iter %d select after removal: %v", i, err)
		}
	}
}

func TestC1ReleaseWithoutSelectionIsSafe(t *testing.T) {
	r, _ := fixture()
	r.Release(nil, 0)
}

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

	if _, err := r.Select("flash", "", nil); err == nil {
		t.Fatal("C4 NOT FIXED: a routable key with no budget was served; " +
			"a missing budget must be rejected (fail closed), not treated as unlimited")
	} else {
		t.Logf("correctly rejected: %v", err)
	}
}

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

	r.SetBudgets(map[string]budget.KeyLimits{"p2/k2": {}}, map[string]int{})

	for i := 0; i < 50; i++ {
		got, err := r.Select("flash", "", nil)
		if err != nil {
			continue
		}
		if got.Provider == "p1" {
			t.Fatalf("C4 NOT FIXED: p1 served at iteration %d with no budget installed", i)
		}
		r.Release(got, 0)
	}
}

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

	got, err := r.Select("m", "", map[string]bool{"a": true, "b": true})
	if err != nil {
		t.Fatalf("M4 NOT FIXED: third-level fallback c unreachable: %v", err)
	}
	if got.Provider != "c" {
		t.Errorf("expected fallback c, got %s", got.Provider)
	}

	if got.Model != "upstream-model" {
		t.Errorf("fallback lost the upstream model: got %q", got.Model)
	}
	r.Release(got, 0)
}

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

	sel, err := r.Select("m", "", map[string]bool{"a": true})
	if err != nil {
		t.Fatalf("self-referential fallback broke routing: %v", err)
	}
	if sel.Provider != "b" {
		t.Errorf("expected b, got %s", sel.Provider)
	}
	r.Release(sel, 0)
}

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

	sel, err := r.acquire("p1", "k1", "m", model.Target{Provider: "p1", Model: "m"}, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := r.ProviderRPM("p1", now); got != 0 {
		t.Fatalf("setup: expected p1 RPM consumed, got %d", got)
	}

	reg.Swap([]model.Provider{provs[1]}, aliases)

	_, err = r.acquire("p1", "k1", "m", model.Target{Provider: "p1", Model: "m"}, now)
	if err == nil {
		t.Fatal("expected the vanished provider to be reported as stale")
	}
	_ = sel
}

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

	for i := 0; i < 1000; i++ {
		sel, err := r.Select("m", "", nil)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		r.Release(sel, 0)

		r.Release(sel, 0)
	}
	if got := r.ProviderRPM("p1", now); got != 0 {
		t.Errorf("after 1000 requests at cap 1000, remaining RPM = %d, want 0", got)
	}
	if _, err := r.Select("m", "", nil); err == nil {
		t.Error("provider RPM cap was exceeded: a double Release manufactured headroom")
	}
}

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

func TestM6CandidateCacheInvalidatedOnRegistrySwap(t *testing.T) {
	r, _ := fixture()
	a, _ := r.reg.Alias("flash")
	before := r.cachedCandidates("flash", a)
	if len(before) == 0 {
		t.Fatal("no candidates built")
	}

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

func TestM10RoundRobinPrunedOnReload(t *testing.T) {
	reg := registry.New(
		[]model.Provider{
			{Name: "p1", BaseURL: "https://p1.example/v1", Keys: []model.Key{{ID: "k1", Secret: "s1"}}},
		},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "m"}}}},
	)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) })
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {}}, map[string]int{"p1": 1})

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

func TestM11CandidateHasNoDeadFields(t *testing.T) {

	r, _ := fixture()
	a, _ := r.reg.Alias("flash")
	for _, c := range r.candidates(a) {
		if c.provider == "" || c.model == "" {
			t.Errorf("candidate with empty provider/model: %+v", c)
		}
	}
}

func TestM12StickyPutUsesInjectedClock(t *testing.T) {
	past := time.Unix(1000, 0)
	s := NewSticky(time.Minute, func() time.Time { return past })
	s.Put("sess", "p1", "k1", model.Target{Provider: "p1", Model: "m"})

	later := past.Add(2 * time.Minute)
	if _, _, _, ok := s.Get("sess", later); ok {
		t.Error("M12 NOT FIXED: pin survived past the TTL; Put did not use the injected clock")
	}
}

func TestM12StickyExpiryHonorsInjectedClock(t *testing.T) {
	base := time.Unix(1000, 0)
	s := NewSticky(time.Minute, func() time.Time { return base })
	s.Put("sess", "p1", "k1", model.Target{Provider: "p1", Model: "m"})

	if _, _, _, ok := s.Get("sess", base.Add(30*time.Second)); !ok {
		t.Fatal("pin expired early")
	}

	s.Put("idle", "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	if _, _, _, ok := s.Get("idle", base.Add(90*time.Second)); ok {
		t.Error("an unread pin survived past its TTL")
	}

	if _, _, _, ok := s.Get("sess", base.Add(80*time.Second)); !ok {
		t.Error("a recently-read pin should still be live")
	}
}

func TestH7StickySizeCap(t *testing.T) {
	s := NewSticky(0, func() time.Time { return time.Unix(1000, 0) })
	for i := 0; i < stickyMaxEntries*2; i++ {
		s.Put(fmt.Sprintf("attacker-session-%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if n := s.Len(); n > stickyMaxEntries {
		t.Errorf("H7 NOT FIXED: sticky map holds %d entries, cap is %d", n, stickyMaxEntries)
	}
}

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

func TestH7StickySweepReclaimsIdleEntries(t *testing.T) {
	clock := time.Unix(1000, 0)
	s := NewSticky(time.Minute, func() time.Time { return clock })
	for i := 0; i < 200; i++ {
		s.Put(fmt.Sprintf("idle-%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if s.Len() == 0 {
		t.Fatal("setup: expected entries")
	}

	clock = clock.Add(10 * time.Minute)
	for i := 0; i < stickySweepEvery*2; i++ {
		s.Put(fmt.Sprintf("fresh-%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if n := s.Len(); n > stickySweepEvery*2 {
		t.Errorf("H7: idle entries were never swept: %d entries remain", n)
	}
}

func TestH7StickyDisabledTTLStillBounded(t *testing.T) {
	s := NewSticky(0, nil)
	for i := 0; i < stickyMaxEntries+500; i++ {
		s.Put(fmt.Sprintf("s%d", i), "p1", "k1", model.Target{Provider: "p1", Model: "m"})
	}
	if n := s.Len(); n > stickyMaxEntries {
		t.Errorf("sticky map exceeded its cap with TTL disabled: %d > %d", n, stickyMaxEntries)
	}
}

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

func TestFixedWindowRefundCannotExceedLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	w := budget.FixedWindow{Limit: 2, Window: time.Minute}
	if !w.Allow(now) || !w.Allow(now) {
		t.Fatal("setup: two requests should be allowed")
	}
	if w.Allow(now) {
		t.Fatal("limit 2 should be exhausted")
	}

	w.Refund(now)
	w.Refund(now)
	if got := w.Remaining(now); got != 2 {
		t.Fatalf("after refunding both, remaining=%d, want 2", got)
	}

	w.Refund(now)
	if got := w.Remaining(now); got != 2 {
		t.Errorf("unmatched refund manufactured headroom: remaining=%d, want 2", got)
	}

	w.Allow(now)
	w.Allow(now)
	if w.Allow(now) {
		t.Error("limit 2 was exceeded after an unmatched refund")
	}
}

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

func TestFixedWindowRefundAfterRolloverIsNoOp(t *testing.T) {
	now := time.Unix(1000, 0)
	w := budget.FixedWindow{Limit: 2, Window: time.Minute}
	w.Allow(now)
	later := now.Add(2 * time.Minute)
	w.Refund(later)
	if got := w.Remaining(later); got != 2 {
		t.Errorf("refund after rollover changed the fresh window: remaining=%d, want 2", got)
	}
}
