package routing

import (
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
)

// TestBudgetsIsSortedByProviderThenKey pins the ordering of the admin budgets
// view.
//
// Budgets iterates r.budgets, a Go map, so before the sort was added the row
// order was whatever the runtime's randomised hash order produced. That is
// visible to any consumer of the admin API: a dashboard renders these rows in
// the order received, and a client comparing two polls positionally sees a
// spurious change every time, with values appearing to move between rows. An
// operator watching which key is throttled would be misled.
//
// The test is in routing rather than in the app-level suite because the key set
// can be chosen exactly here. The app-level test has one provider with two keys,
// which cannot distinguish a sort that compares provider first from one that
// compares key alone: with a single provider both orderings are identical. That
// gap was found by mutation, where a mutant that sorted on key only survived
// every test. So this fixture uses several providers with overlapping key names,
// where the two orderings genuinely differ.
func TestBudgetsIsSortedByProviderThenKey(t *testing.T) {
	keys := map[string]budget.KeyLimits{
		"zeta/last":   {RPM: 1},
		"alpha/b":     {RPM: 2},
		"alpha/a":     {RPM: 3},
		"mid/z":       {RPM: 4},
		"mid/a":       {RPM: 5},
		"alpha/z":     {RPM: 6},
		"Beta/upper":  {RPM: 7},
		"beta/lower":  {RPM: 8},
		"beta/A":      {RPM: 9},
		"solo/only":   {RPM: 10},
		"p10/x":       {RPM: 11},
		"p2/x":        {RPM: 12},
		"p2/A":        {RPM: 13},
		"p2/2":        {RPM: 14},
		"p2/10":       {RPM: 15},
		"p2/1":        {RPM: 16},
		"another/key": {RPM: 17},
	}
	r := NewRouter(nil, nil, time.Minute, time.Now)
	r.SetBudgets(keys, map[string]int{})

	views := make([][]BudgetStatus, 0, 12)
	for i := 0; i < 12; i++ {
		views = append(views, r.Budgets(time.Now()))
	}

	// Every call must return the same rows in the same order. Running it
	// repeatedly is what makes this a real test of ordering: a single call has a
	// one-in-N chance of accidentally matching the expected order, while twelve
	// calls do not.
	// Plain byte ordering, so uppercase sorts before lowercase. That is the
	// sort.Slice string comparison this uses, and it is deliberate: key ids are
	// operator-chosen, and the only requirement is a stable total order that is
	// the same on every call, not a human-friendly collation. Writing the
	// expectation by hand in ASCII order rather than in a "looks alphabetical"
	// order matters here, because the first draft of this list put "another"
	// before "alpha" and "Beta" after "alpha", which is what a human expects and
	// is not what the code does.
	want := []string{
		"Beta/upper",
		"alpha/a", "alpha/b", "alpha/z",
		"another/key",
		"beta/A", "beta/lower",
		"mid/a", "mid/z",
		"p10/x",
		"p2/1", "p2/10", "p2/2", "p2/A", "p2/x",
		"solo/only", "zeta/last",
	}
	for i, v := range views {
		if len(v) != len(want) {
			t.Fatalf("call %d returned %d rows, want %d", i, len(v), len(want))
		}
		for j, got := range v {
			id := got.Provider + "/" + got.Key
			if id != want[j] {
				t.Fatalf("call %d row %d is %s, want %s\nfull order: %v",
					i, j, id, want[j], ids(v))
			}
		}
	}

	// The sort must be a total order, not a tie-break that happens to work for
	// this input. Two rows equal under the comparator would be reordered
	// arbitrarily, so assert no duplicates and that each id appears exactly once.
	seen := map[string]int{}
	for _, v := range views {
		for _, got := range v {
			seen[got.Provider+"/"+got.Key]++
		}
	}
	for id, n := range seen {
		if n != len(views) {
			t.Errorf("%s appeared in %d of %d calls: the view is not stable",
				id, n, len(views))
		}
	}
	if len(seen) != len(want) {
		t.Errorf("the view contains %d distinct keys, want %d: %v",
			len(seen), len(want), seen)
	}
}

// TestBudgetsSortingHoldsUnderConcurrentSetBudgets checks that the ordering
// survives a reload, because SetBudgets can add keys while the admin API is
// polling. An unsorted result there would reintroduce the same problem in the
// window right after a config change, which is exactly when an operator is
// watching to confirm the change took effect.
func TestBudgetsSortingHoldsUnderConcurrentSetBudgets(t *testing.T) {
	r := NewRouter(nil, nil, time.Minute, time.Now)
	all := map[string]budget.KeyLimits{
		"p1/k1": {RPM: 1}, "p1/k2": {RPM: 1},
		"p2/k1": {RPM: 1}, "p2/k2": {RPM: 1},
		"p3/k1": {RPM: 1}, "p3/k2": {RPM: 1},
	}
	r.SetBudgets(all, map[string]int{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			// Alternate between the full set and a subset, which is what a
			// reload that drops or adds a key looks like.
			if i%2 == 0 {
				r.SetBudgets(all, map[string]int{})
			} else {
				r.SetBudgets(map[string]budget.KeyLimits{
					"p2/k1": {RPM: 1}, "p1/k2": {RPM: 1},
				}, map[string]int{})
			}
		}
	}()

	for i := 0; i < 300; i++ {
		v := r.Budgets(time.Now())
		for j := 1; j < len(v); j++ {
			prev, cur := v[j-1], v[j]
			if prev.Provider > cur.Provider ||
				(prev.Provider == cur.Provider && prev.Key > cur.Key) {
				t.Errorf("iteration %d: budgets are not sorted: %s/%s before %s/%s",
					i, prev.Provider, prev.Key, cur.Provider, cur.Key)
				<-done
				return
			}
		}
	}
	<-done
}

func ids(v []BudgetStatus) []string {
	out := make([]string, 0, len(v))
	for _, s := range v {
		out = append(out, s.Provider+"/"+s.Key)
	}
	return out
}
