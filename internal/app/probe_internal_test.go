package app

import (
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

// TestAliasModelByProvider verifies the issue #7 fix: the health probe must use a
// model the provider under test actually serves (taken from the alias target whose
// Provider matches), not blindly the first alias's target model.
//
// Scenario: provider "p1" serves model "p1-model" (via alias "a1"); provider "p2"
// serves "p2-model" (via alias "a2"). The first alias in iteration order (a1)
// targets p1, so a naive implementation reusing the first alias target would probe
// p2 with "p1-model" -- wrong. The correct mapping is per-provider.
func TestAliasModelByProvider(t *testing.T) {
	a := &App{}
	a.reg = registry.New(
		[]model.Provider{
			{Name: "p1", BaseURL: "http://p1"},
			{Name: "p2", BaseURL: "http://p2"},
		},
		[]model.Alias{
			{Name: "a1", Targets: []model.Target{{Provider: "p1", Model: "p1-model"}}},
			{Name: "a2", Targets: []model.Target{{Provider: "p2", Model: "p2-model"}}},
		},
	)

	m := a.aliasModelByProvider()
	if got := m["p1"]; got != "p1-model" {
		t.Errorf("provider p1 probed with model %q, want p1-model", got)
	}
	if got := m["p2"]; got != "p2-model" {
		t.Errorf("provider p2 probed with model %q, want p2-model (regression of #7)", got)
	}
}

// TestAliasModelByProviderDistinctAliasesSameProvider guards against silently
// collapsing two different models for the same provider onto the first-seen one
// when aliases target the same provider with different models.
func TestAliasModelByProviderDistinctAliasesSameProvider(t *testing.T) {
	a := &App{}
	a.reg = registry.New(
		[]model.Provider{{Name: "p1", BaseURL: "http://p1"}},
		[]model.Alias{
			{Name: "cheap", Targets: []model.Target{{Provider: "p1", Model: "p1-mini"}}},
			{Name: "pro", Targets: []model.Target{{Provider: "p1", Model: "p1-max"}}},
		},
	)
	m := a.aliasModelByProvider()
	// First-seen model for p1 must be recorded (deterministic: cheap -> p1-mini).
	if got := m["p1"]; got != "p1-mini" {
		t.Errorf("provider p1 mapped to %q, want p1-mini (first-seen alias target)", got)
	}
}
