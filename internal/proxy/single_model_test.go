package proxy

import (
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

func TestSingleModel(t *testing.T) {
	h := &Handler{deps: Deps{Registry: registry.New(
		[]model.Provider{{Name: "p1", Keys: []model.Key{{ID: "k1"}}}},
		[]model.Alias{
			{Name: "single", Targets: []model.Target{
				{Provider: "p1", Model: "m-a"},
				{Provider: "p1", Model: "m-a"},
			}},
			{Name: "multi", Targets: []model.Target{
				{Provider: "p1", Model: "m-a"},
				{Provider: "p1", Model: "m-b"},
			}},
			{Name: "none", Targets: []model.Target{}},
		},
	)}}

	if m, ok := h.singleModel("single"); !ok || m != "m-a" {
		t.Fatalf("single alias: got (%q, %v), want (m-a, true)", m, ok)
	}
	if _, ok := h.singleModel("multi"); ok {
		t.Fatal("multi-model alias must not use the single-model cache fast path")
	}
	if _, ok := h.singleModel("none"); ok {
		t.Fatal("empty alias must not use the cache fast path")
	}
	if _, ok := h.singleModel("missing"); ok {
		t.Fatal("unknown alias must not use the cache fast path")
	}
}
