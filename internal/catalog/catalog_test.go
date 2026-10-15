package catalog

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func handler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

func TestFetchPopulatesModels(t *testing.T) {
	srv := httptest.NewServer(handler(http.StatusOK, `{"data":[{"id":"a"},{"id":"b"}]}`))
	defer srv.Close()

	f := NewFetcher("p", srv.URL, srv.Client(), true)
	if err := f.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := f.Models()
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("Models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Models = %v, want %v", got, want)
		}
	}
	if f.LastError() != nil {
		t.Fatalf("LastError = %v, want nil", f.LastError())
	}
}

func TestBareArrayShape(t *testing.T) {
	srv := httptest.NewServer(handler(http.StatusOK, `["x",""," y "," z "]`))
	defer srv.Close()

	f := NewFetcher("p", srv.URL, srv.Client(), true)
	if err := f.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := f.Models()
	want := []string{"x", "y", "z"}
	if len(got) != len(want) {
		t.Fatalf("Models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Models = %v, want %v", got, want)
		}
	}
}

func TestRefreshFailureKeepsLastGood(t *testing.T) {
	up := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up {
			w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := NewFetcher("p", srv.URL, srv.Client(), true)
	if err := f.Refresh(); err == nil {
		t.Fatal("expected first refresh to fail against 500")
	}
	if len(f.Models()) != 0 {
		t.Fatalf("first failure must leave an empty catalog, got %v", f.Models())
	}
	if f.LastError() == nil {
		t.Fatal("LastError must be set after a failed refresh")
	}

	up = true
	if err := f.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	up = false
	if err := f.Refresh(); err == nil {
		t.Fatal("expected second refresh to fail")
	}

	got := f.Models()
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("Models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Models = %v, want %v", got, want)
		}
	}
	if f.LastError() == nil {
		t.Fatal("LastError must reflect the failed refresh")
	}
}

func TestRefreshWhenUpstreamDown(t *testing.T) {
	srv := httptest.NewServer(handler(http.StatusOK, `{"data":[{"id":"a"}]}`))
	f := NewFetcher("p", srv.URL, srv.Client(), true)
	if err := f.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.Close()

	if err := f.Refresh(); err == nil {
		t.Fatal("expected refresh against closed upstream to fail")
	}
	if len(f.Models()) != 1 {
		t.Fatalf("last-good catalog lost: %v", f.Models())
	}
	if f.Models()[0] != "a" {
		t.Fatalf("catalog = %v, want [a]", f.Models())
	}
	if f.LastError() == nil {
		t.Fatal("LastError must be set")
	}
}

func TestDisabledFetcherNeverContactsNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatal("disabled fetcher must not contact the network")
	}))
	defer srv.Close()

	f := NewFetcher("p", srv.URL, srv.Client(), false)
	if f.Enabled() {
		t.Fatal("fetcher should report disabled")
	}
	if err := f.Refresh(); err != nil {
		t.Fatalf("disabled Refresh should be a no-op nil, got %v", err)
	}
	if len(f.Models()) != 0 {
		t.Fatalf("disabled fetcher must report empty catalog, got %v", f.Models())
	}
	if f.LastError() != nil {
		t.Fatalf("LastError for disabled fetcher = %v, want nil", f.LastError())
	}
	if called {
		t.Fatal("server was contacted")
	}
}

func fastClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second}
}

func TestConstructionDoesNoNetwork(t *testing.T) {
	srv := httptest.NewServer(handler(http.StatusOK, `{"data":[{"id":"a"}]}`))
	defer srv.Close()

	f := NewFetcher("p", srv.URL, fastClient(), true)
	if len(f.Models()) != 0 {
		t.Fatal("constructed fetcher must start empty")
	}
	if f.LastError() != nil {
		t.Fatal("constructed fetcher must start with no error")
	}
	time.Sleep(50 * time.Millisecond)
	if len(f.Models()) != 0 {
		t.Fatal("fetcher did network work at construction")
	}
}

func TestEmptyObjectDataIsValidCatalog(t *testing.T) {
	srv := httptest.NewServer(handler(http.StatusOK, `{"data":[]}`))
	defer srv.Close()
	f := NewFetcher("p", srv.URL, srv.Client(), true)
	if err := f.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(f.Models()) != 0 {
		t.Fatalf("Models = %v, want empty", f.Models())
	}
	if f.LastError() != nil {
		t.Fatalf("LastError = %v, want nil", f.LastError())
	}
}
