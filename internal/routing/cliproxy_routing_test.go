package routing

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

// fixtureWith gives a router with the given strategy and per-key weights.
func fixtureWith(strategy string, weights map[string]int) (*Router, time.Time) {
	provs := []model.Provider{
		{
			Name:    "p1",
			BaseURL: "https://p1.example/v1",
			Keys: []model.Key{
				{ID: "k1", Weight: weights["k1"], Priority: 10},
				{ID: "k2", Weight: weights["k2"], Priority: 1},
			},
		},
	}
	aliases := []model.Alias{
		{Name: "m", Targets: []model.Target{{Provider: "p1", Model: "m1"}}},
	}
	reg := registry.New(provs, aliases)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, func() time.Time { return time.Unix(1000, 0) },
		WithStrategy(strategy))
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {RPM: 10000}, "p1/k2": {RPM: 10000}}, map[string]int{})
	return r, time.Unix(1000, 0)
}

// TestWeightedOrderRoutesProportionally picks a heavy key more often than a
// light one across many selections (no sticky), when the heavy key is not
// exhausted and its budget is large.
func TestWeightedOrderRoutesProportionally(t *testing.T) {
	r, _ := fixtureWith("weighted-round-robin", map[string]int{"k1": 3, "k2": 1})
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		sel, err := r.Select("m", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[sel.Key]++
		r.Release(sel, 0)
	}
	if counts["k1"] <= counts["k2"] {
		t.Fatalf("expected heavy key k1 to dominate weighted routing, got %+v", counts)
	}
}

// TestFillFirstOrdersByPriority routes high-priority keys first until they are
// exhausted, then falls through to lower priority.
func TestFillFirstOrdersByPriority(t *testing.T) {
	r, _ := fixtureWith("fill-first", map[string]int{"k1": 1, "k2": 1})
	counts := map[string]int{}
	for i := 0; i < 50; i++ {
		sel, err := r.Select("m", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[sel.Key]++
		r.Release(sel, 0)
	}
	// k1 has the higher priority and should be picked first when both free.
	if counts["k1"] < counts["k2"] {
		t.Fatalf("expected high priority k1 to lead, got %+v", counts)
	}
}

func TestCooldownTakesCredentialOutOfRotation(t *testing.T) {
	r, now := fixtureWith("round-robin", map[string]int{"k1": 1, "k2": 1})
	r.cd = &Cooldown{enabled: true, offset: time.Minute, now: func() time.Time { return now }, until: map[string]time.Time{}}
	// Prime with a success to avoid sticky interference.
	sel0, err := r.Select("m", "sess", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Release(sel0, 0)

	// Put every key of p1 in cooldown, then a selection must fail with no route.
	r.cd.Schedule("p1/k1")
	r.cd.Schedule("p1/k2")
	if _, err := r.Select("m", "different", nil); err == nil {
		t.Fatal("expected no route when all credentials are in cooldown")
	}
}

func TestRetryRoundsResolution(t *testing.T) {
	provs := []model.Provider{
		{Name: "p1", BaseURL: "https://p1.example/v1", RequestRetry: ptr(3), Keys: []model.Key{{ID: "k1"}, {ID: "k2", RequestRetry: ptr(0)}}},
		{Name: "p2", BaseURL: "https://p2.example/v1", Keys: []model.Key{{ID: "k3"}}},
	}
	reg := registry.New(provs, nil)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, time.Now, WithRequestRetry(-1))
	if got := r.RetryRounds("p1", "k1"); got != 3 {
		t.Fatalf("provider override: got %d want 3", got)
	}
	if got := r.RetryRounds("p1", "k2"); got != 0 {
		t.Fatalf("key override 0: got %d want 0", got)
	}
	// Global default when neither provider nor key overrides.
	r2 := NewRouter(reg, health.NewRegistry(), time.Minute, time.Now, WithRequestRetry(5))
	if got := r2.RetryRounds("p2", "k3"); got != 5 {
		t.Fatalf("global: got %d want 5", got)
	}
}

func TestDecideErrorHonoursRulesAndCooldown(t *testing.T) {
	provs := []model.Provider{
		{
			Name: "p1", BaseURL: "https://p1.example/v1",
			Keys: []model.Key{{ID: "k1"}},
			ErrorRules: []model.ErrorRule{
				{Status: 429, Match: []string{"quota"}, Action: "stop"},
			},
		},
	}
	reg := registry.New(provs, nil)
	r := NewRouter(reg, health.NewRegistry(), time.Minute, time.Now, WithCooldown(true, time.Minute))
	// A 429 whose body matches the stop rule must stop and must NOT schedule a
	// cooldown (stop, not stop-and-cooldown).
	if !r.DecideError("p1", "k1", 429, []byte(`{"error":"quota exceeded"}`)) {
		t.Fatal("expected stop decision")
	}
	if r.cooldownFor().Allow("p1/k1", time.Now()) == false {
		t.Fatal("stop must not schedule a cooldown")
	}
	// A retryable 500 with no rule match should not stop, and should schedule
	// the transient cooldown.
	if r.DecideError("p1", "k1", 500, []byte(`{}`)) {
		t.Fatal("expected continue for unmatched 500")
	}
	if r.cooldownFor().Allow("p1/k1", time.Now()) {
		t.Fatal("transient 500 should have scheduled a cooldown")
	}
}

func TestMatchModelWildcards(t *testing.T) {
	cases := []struct{ pattern, model string; want bool }{
		{"*flash*", "gemini-flash-latest", true},
		{"gemini-*", "gemini-pro", true},
		{"*-preview", "claude-preview", true},
		{"exact", "exact", true},
		{"exact", "exactsuffix", false},
		{"*", "anything", true},
		{"a*b", "aXb", true},
		{"a*b", "ab", true},
		{"a*b", "aXc", false},
	}
	for _, c := range cases {
		if got := matchModel(c.pattern, c.model); got != c.want {
			t.Fatalf("matchModel(%q,%q)=%v want %v", c.pattern, c.model, got, c.want)
		}
	}
}

func ptr(n int) *int { return &n }
