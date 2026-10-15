package routing

import (
	"sort"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// acquireKeyOrderStrategy returns the configured strategy, defaulting to
// round-robin. Strategy is one of "round-robin", "weighted-round-robin", or
// "fill-first".
func (r *Router) strategyName() string {
	if r.strategy == "" {
		return "round-robin"
	}
	return r.strategy
}

// keyOrder builds the visit order of a provider's keys for THIS selection,
// interpreted by the configured strategy, rotated so it starts at `start` (a
// per-provider round-robin offset). The returned slice holds key indices into
// p.Keys; the caller iterates it applying the health/cooldown/budget/skip
// filters.
//
//   - round-robin (default): plain order starting at start.
//   - fill-first: keys sorted by Priority (larger first), stable within equal
//     priority, then rotated to start at start so traffic spreads across same-
//     priority keys instead of always hammering the first.
//   - weighted-round-robin: a virtual ring of totalWeight units where each key
//     occupies Weight units; positions from `start` are mapped to keys and
//     deduplicated, so a higher-weight key is visited proportionally more often
//     across calls.
func (r *Router) keyOrder(p *model.Provider, start int) []int {
	n := len(p.Keys)
	if n == 0 {
		return nil
	}
	switch r.strategyName() {
	case "weighted-round-robin":
		return weightedOrder(p, start)
	case "fill-first":
		return fillFirstOrder(p, start)
	default:
		return roundRobinOrder(n, start)
	}
}

func roundRobinOrder(n, start int) []int {
	order := make([]int, n)
	for i := 0; i < n; i++ {
		order[i] = (start + i) % n
	}
	return order
}

func fillFirstOrder(p *model.Provider, start int) []int {
	n := len(p.Keys)
	type pri struct {
		idx int
		p   int
		r   int // round-robin position for stable ordering within a priority.
	}
	items := make([]pri, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, pri{idx: i, p: p.Keys[i].Priority, r: (start + i) % n})
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].p != items[b].p {
			return items[a].p > items[b].p // larger priority first.
		}
		return items[a].r < items[b].r
	})
	order := make([]int, n)
	for i, it := range items {
		order[i] = it.idx
	}
	return order
}

func weightedOrder(p *model.Provider, start int) []int {
	n := len(p.Keys)
	weights := make([]int, n)
	total := 0
	for i := 0; i < n; i++ {
		w := p.Keys[i].Weight
		if w <= 0 {
			w = 1
		}
		weights[i] = w
		total += w
	}
	if total <= 0 {
		return roundRobinOrder(n, start)
	}
	// Map a virtual position in [0,total) to a key via cumulative weights.
	posToKey := func(pos int) int {
		pos = ((pos % total) + total) % total
		acc := 0
		for i, w := range weights {
			acc += w
			if pos < acc {
				return i
			}
		}
		return n - 1
	}
	order := make([]int, 0, n)
	seen := make(map[int]bool, n)
	// Walk the ring from `start`, at most total positions, collecting unique keys
	// so a single acquireKey call has a fallback order that respects weight.
	for step := 0; step < total && len(order) < n; step++ {
		idx := posToKey(start + step)
		if !seen[idx] {
			seen[idx] = true
			order = append(order, idx)
		}
	}
	// If a heavy key was the only unique one seen early, fill remains with the
	// untouched keys so every key can still be reached when the heavy key is
	// filtered out (e.g. on cooldown).
	if len(order) < n {
		for i := 0; i < n && len(order) < n; i++ {
			if !seen[i] {
				order = append(order, i)
			}
		}
	}
	return order
}

// selectionNeedsRebind reports whether a session may rebind from its pinned
// credential under the configured affinity policy. Allowed when the error is not
// quota-driven (any), and only actually rebinds when the sticky path gives up
// for a quota reason under "quota".
func (r *Router) rebindAllowed(cause string) bool {
	if r.affinity.rebindOn == "quota" {
		return cause == "exhausted" // quota only.
	}
	return true // "any" (default) rebinds on any reason.
}
