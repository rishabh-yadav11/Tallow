package app

import (
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

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

	if got := m["p1"]; got != "p1-mini" {
		t.Errorf("provider p1 mapped to %q, want p1-mini (first-seen alias target)", got)
	}
}
