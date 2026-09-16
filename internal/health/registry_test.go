package health

import (
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func TestPruneRemovesStaleBreakers(t *testing.T) {
	r := NewRegistry()
	_ = r.Provider("p1")
	_ = r.Provider("p2")
	_ = r.Key("p1", "k1")
	_ = r.Key("p2", "k2")

	// After pruning to only p1/k1, p2 (and its key) breakers must be gone.
	r.Prune([]model.Provider{
		{Name: "p1", Keys: []model.Key{{ID: "k1"}}},
	})

	r.mu.Lock()
	_, hasP2 := r.provs["p:p2"]
	_, hasK2 := r.keys["p2/k2"]
	_, hasK1 := r.keys["p1/k1"]
	r.mu.Unlock()

	if hasP2 {
		t.Fatal("p2 provider breaker should have been pruned")
	}
	if hasK2 {
		t.Fatal("p2/k2 key breaker should have been pruned")
	}
	if !hasK1 {
		t.Fatal("p1/k1 key breaker should be retained")
	}
}
