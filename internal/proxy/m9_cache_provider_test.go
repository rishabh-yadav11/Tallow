package proxy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

func TestCacheHitsCreateNoProviderBucket(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	obs := observ.New(true)
	h := newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        c,
		CacheEnabled: true,
		Observ:       obs,
	})

	doPost(t, h, "", simpleBody)
	for i := 0; i < 20; i++ {
		doPost(t, h, "", simpleBody)
	}

	s := obs.Snapshot()
	if s.Cached != 20 {
		t.Fatalf("expected 20 cache hits, got %d", s.Cached)
	}

	if s.Total != 21 {
		t.Fatalf("expected 21 total requests, got %d", s.Total)
	}
	if _, ok := s.ByProvider["cache"]; ok {
		t.Fatalf("a synthetic %q provider bucket exists: %+v", "cache", s.ByProvider["cache"])
	}
	if got := len(s.ByProvider); got != 1 {
		t.Fatalf("by_provider has %d buckets, want exactly the one real provider: %v",
			got, s.ByProvider)
	}
	if s.ByProvider["p1"].Requests != 1 {
		t.Fatalf("the real provider should own only the upstream call, got %+v", s.ByProvider["p1"])
	}
}

func TestCacheHitRecordsProvenanceNotAttribution(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	h := newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        c,
		CacheEnabled: true,
	})

	doPost(t, h, "", simpleBody)

	e, hit := c.Get(cacheScope("flash-v4", "", cache.Key(mustNormalize(t, simpleBody))))
	if !hit {
		t.Fatal("the entry was not cached, so provenance cannot be asserted")
	}
	if e.Provider != "p1" {
		t.Fatalf("the cache entry records provenance %q, want the provider that served it (%q)", e.Provider, "p1")
	}
}

func TestCacheHitRowCarriesOriginInRouteReason(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	st, err := store.Open(store.StoreConfig{Path: filepath.Join(t.TempDir(), "tallow.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	h := newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        c,
		CacheEnabled: true,
		Store:        st,
	})

	doPost(t, h, "", simpleBody)
	doPost(t, h, "", simpleBody)

	st.Flush()

	rows, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	var hitRow string
	for _, r := range rows {
		if r.Status == "cached" {
			hitRow = r.RouteReason
		}
	}
	if hitRow == "" {
		t.Fatalf("no cache-hit row was recorded; rows = %+v", rows)
	}
	if !strings.Contains(hitRow, "cache_hit") {
		t.Fatalf("cache-hit row reason = %q, want it to identify itself as a cache hit", hitRow)
	}
	if !strings.Contains(hitRow, "p1") {
		t.Fatalf("cache-hit row reason = %q, want it to name the route the answer came from (%q)", hitRow, "p1")
	}

	for _, r := range rows {
		if r.Provider == "cache" {
			t.Fatalf("a persisted row attributes the request to a synthetic %q provider: %+v", "cache", r)
		}
	}
}

func TestRoutingFailureAlsoCreatesNoBucket(t *testing.T) {
	obs := observ.New(true)
	h := newHarness(t, Deps{
		Registry: registry.New(nil, nil),
		Router:   newRouter(t, registry.New(nil, nil)),
		Observ:   obs,
	})
	doPost(t, h, "", simpleBody)

	s := obs.Snapshot()
	if s.Total != 1 || s.Errors != 1 {
		t.Fatalf("an unroutable request must still count in the totals: %+v", s)
	}
	if len(s.ByProvider) != 0 {
		t.Fatalf("M9 regression: an unroutable request created provider bucket(s): %v", s.ByProvider)
	}
}

func mustNormalize(t *testing.T, body string) []byte {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	k, err := cache.Normalize(req)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return k
}
