package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func enabled(p model.Provider) model.Provider {
	p.Catalog = &model.ConfigCatalog{Enabled: true}
	return p
}

// TestFetchAndParse exercises fetchDefault against a real httptest server.
func TestFetchAndParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":""},{"id":"c"}]}`))
	}))
	defer srv.Close()

	m := NewManager(srv.Client())
	m.Refresh(context.Background(), []model.Provider{
		enabled(model.Provider{Name: "p1", BaseURL: srv.URL}),
	})

	got, ok := m.Models("p1")
	if !ok {
		t.Fatal("expected catalog for p1")
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models: got %v, want %v", got, want)
	}
}

// TestCustomURL verifies an explicit catalog URL overrides the derived /models.
func TestCustomURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := model.Provider{Name: "p1", BaseURL: "http://unused", Catalog: &model.ConfigCatalog{Enabled: true, URL: srv.URL + "/custom"}}
	m := NewManager(srv.Client())
	m.Refresh(context.Background(), []model.Provider{p})
	if gotPath != "/custom" {
		t.Fatalf("custom URL not honored: got path %q", gotPath)
	}
	if _, ok := m.Models("p1"); !ok {
		t.Fatal("custom catalog not stored")
	}
}

// TestDisabledNoNetwork verifies zero remote calls when no provider opts in.
func TestDisabledNoNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	m := NewManager(srv.Client())
	// No provider has Catalog.Enabled.
	m.Refresh(context.Background(), []model.Provider{{Name: "p1", BaseURL: srv.URL}})
	if called {
		t.Fatal("a disabled catalog should never trigger a remote call")
	}
	if _, ok := m.Models("p1"); ok {
		t.Fatal("a provider with no catalog must not have models")
	}
}

// TestExcludedFiltered verifies that ExcludedModels wildcards are applied to the
// fetched catalog at store time.
func TestExcludedFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gemini-flash"},{"id":"gemini-pro"},{"id":"claude-haiku-preview"},{"id":"claude-sonnet"}]}`))
	}))
	defer srv.Close()
	p := enabled(model.Provider{
		Name:           "p1",
		BaseURL:        srv.URL,
		ExcludedModels: []string{"*flash*", "gemini-*", "*-preview"},
	})
	m := NewManager(srv.Client())
	m.Refresh(context.Background(), []model.Provider{p})

	got, _ := m.Models("p1")
	want := []string{"claude-sonnet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded filtering: got %v, want %v", got, want)
	}
}

// TestRefreshFailureKeepsLast verifies a failed refresh keeps the prior valid
// catalog.
func TestRefreshFailureKeepsLast(t *testing.T) {
	count := 0
	m := NewManager(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if count == 2 {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"x"}]}`))
	}))
	defer srv.Close()
	m.client = srv.Client()

	p := enabled(model.Provider{Name: "p1", BaseURL: srv.URL})

	m.Refresh(context.Background(), []model.Provider{p}) // first fetch -> stores "x"

	// Second refresh hits the 500 and must keep "x".
	m.Refresh(context.Background(), []model.Provider{p})

	got, ok := m.Models("p1")
	if !ok {
		t.Fatal("catalog should survive a failed refresh")
	}
	if !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("expected last valid catalog [x], got %v", got)
	}
}

// TestMatchModel covers the wildcard semantics.
func TestMatchModel(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		pat    []string
		expect bool
	}{
		{"exact", "gpt-4o", []string{"gpt-4o"}, true},
		{"exact no", "gpt-4o", []string{"gpt-4o-mini"}, false},
		{"prefix", "gemini-flash", []string{"gemini-*"}, true},
		{"prefix no", "claude-flash", []string{"gemini-*"}, false},
		{"suffix", "claude-3-preview", []string{"*-preview"}, true},
		{"suffix no", "claude-3", []string{"*-preview"}, false},
		{"substring", "gemini-flash-lite", []string{"*flash*"}, true},
		{"substring no", "gemini-pro", []string{"*flash*"}, false},
		{"multi matches one", "x-flash-x", []string{"gemini-*", "*flash*"}, true},
		{"none match", "claude-sonnet", []string{"gemini-*", "*-preview"}, false},
		{"empty pattern list", "gpt-4o", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchModel(tc.model, tc.pat); got != tc.expect {
				t.Fatalf("MatchModel(%q, %v) = %v, want %v", tc.model, tc.pat, got, tc.expect)
			}
		})
	}
}

// TestModelsEmptyDistinct verifies the bool distinguishes an empty fetched
// catalog from one that was never fetched.
func TestModelsEmptyDistinct(t *testing.T) {
	m := NewManager(nil)
	if _, ok := m.Models("missing"); ok {
		t.Fatal("a never-fetched provider must report ok=false")
	}
	// Stub the fetch to return an empty list.
	m.fetch = func(context.Context, model.Provider, string) ([]string, error) {
		return nil, nil
	}
	m.Refresh(context.Background(), []model.Provider{
		enabled(model.Provider{Name: "p1"}),
	})
	if got, ok := m.Models("p1"); !ok || len(got) != 0 {
		t.Fatalf("empty catalog should report ok=true with empty list, got %v %v", got, ok)
	}
}

// TestRefreshNeverFetchesDisabled makes sure a disabled catalog is skipped even
// when a fetch stub is installed (no remote call by default).
func TestRefreshNeverFetchesDisabled(t *testing.T) {
	called := false
	m := NewManager(nil)
	m.fetch = func(context.Context, model.Provider, string) ([]string, error) {
		called = true
		return nil, errors.New("boom")
	}
	m.Refresh(context.Background(), []model.Provider{{Name: "p1"}}) // no enabled catalog
	if called {
		t.Fatal("disabled catalog should never invoke the fetcher")
	}
}
