package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/catalog"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

func modelIDs(body map[string]any) map[string]bool {
	ids := map[string]bool{}
	if data, ok := body["data"].([]any); ok {
		for _, el := range data {
			if m, ok := el.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					ids[id] = true
				}
			}
		}
	}
	return ids
}

func getModels(t *testing.T, h *Handler) map[string]bool {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models status %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal /v1/models response: %v", err)
	}
	return modelIDs(body)
}

// TestListModelsCatalogDerived verifies /v1/models returns manual aliases plus
// catalog-derived entries for providers that opted in and have no manual target.
func TestListModelsCatalogDerived(t *testing.T) {
	cm := catalog.NewManager(nil)
	cm.SetModels("prov-a", []string{"alpha", "beta"})
	cm.SetModels("prov-b", []string{"gamma"})

	reg := registry.New(
		[]model.Provider{
			{Name: "prov-a", Catalog: &model.ConfigCatalog{Enabled: true}},
			{Name: "prov-b", Catalog: &model.ConfigCatalog{Enabled: true}},
			{Name: "prov-c", Catalog: &model.ConfigCatalog{Enabled: true}},
		},
		// prov-c is manually declared (manual wins -> no catalog entries).
		[]model.Alias{
			{Name: "manual", Targets: []model.Target{{Provider: "prov-c", Model: "x"}}},
		},
	)
	cm.SetModels("prov-c", []string{"should-not-appear"})

	h := &Handler{deps: Deps{Registry: reg, Catalog: cm}}
	ids := getModels(t, h)

	for _, want := range []string{"manual", "alpha", "beta", "gamma"} {
		if !ids[want] {
			t.Errorf("model %q missing from /v1/models", want)
		}
	}
	if ids["should-not-appear"] {
		t.Error("manual-declared provider prov-c leaked its catalog models")
	}
}

// TestGetModelCatalogDerived ensures getModel resolves catalog entries that are
// not aliases, and still 404s unknown models.
func TestGetModelCatalogDerived(t *testing.T) {
	cm := catalog.NewManager(nil)
	cm.SetModels("prov-a", []string{"alpha"})
	reg := registry.New(
		[]model.Provider{{Name: "prov-a", Catalog: &model.ConfigCatalog{Enabled: true}}},
		[]model.Alias{{Name: "manual", Targets: []model.Target{{Provider: "prov-a", Model: "x"}}}},
	)
	h := &Handler{deps: Deps{Registry: reg, Catalog: cm}}

	req := httptest.NewRequest(http.MethodGet, "/v1/models/alpha", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("getModel alpha: status %d body %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models/unknown", nil)
	rec = httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("getModel unknown: status %d want 404", rec.Code)
	}
}
