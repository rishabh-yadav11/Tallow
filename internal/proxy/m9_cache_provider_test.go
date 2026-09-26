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

// M7's second half, and the same defect class as M9.
//
// M9 established that a request no provider served must not be attributed to
// one: it is counted in the global totals and left out of by_provider, so a
// gateway-level routing failure cannot hide behind a real provider's healthy
// error rate. That fix was applied to the routing-failure path. The
// cache-hit path was left with the original defect: it set Provider = "cache",
// which invented a provider bucket that no operator ever configured.
//
// Why that matters concretely, beyond the tidiness argument:
//
//   - by_provider is the map an operator reads to decide which provider is
//     unhealthy. A "cache" row with a growing request count is a peer of the
//     real providers, and its cost is always 0, so it reads as a provider that
//     is being called and never charged.
//   - the store persists the same provider name on every cache-hit row, so the
//     database accumulates a provider that cannot be configured, cannot be
//     given a limit, and cannot be diagnosed.
//   - it grows with cache warmth, so the distortion is worst exactly when the
//     cache is working best.
//
// A cache hit genuinely did not come from a provider, so the truthful
// representation is the same one M9 chose: counted in the totals, absent from
// the per-provider view, with the origin recorded as provenance.

// TestCacheHitsCreateNoProviderBucket is the core regression.
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

	// Warm the cache hard. 20 hits is enough to make a growing synthetic bucket
	// unmistakable in the snapshot.
	doPost(t, h, "", simpleBody)
	for i := 0; i < 20; i++ {
		doPost(t, h, "", simpleBody)
	}

	s := obs.Snapshot()
	if s.Cached != 20 {
		t.Fatalf("expected 20 cache hits, got %d", s.Cached)
	}
	// Exactly one upstream call happened, so exactly one provider request.
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

// TestCacheHitRecordsProvenanceNotAttribution checks that the information the
// synthetic bucket was standing in for is not simply lost. A cache hit should
// still be able to say which route produced the answer it served.
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

	// Prime, so the entry carries its provenance.
	doPost(t, h, "", simpleBody)

	e, hit := c.Get(cacheScope("flash-v4", "", cache.Key(mustNormalize(t, simpleBody))))
	if !hit {
		t.Fatal("the entry was not cached, so provenance cannot be asserted")
	}
	if e.Provider != "p1" {
		t.Fatalf("the cache entry records provenance %q, want the provider that served it (%q)", e.Provider, "p1")
	}
}

// TestCacheHitRowCarriesOriginInRouteReason: the stored row for a cache hit
// should name the route the answer came from, so the operator-facing log still
// shows it even though no provider owns the request.
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

	doPost(t, h, "", simpleBody) // miss: upstream
	doPost(t, h, "", simpleBody) // hit

	// Store writes are queued and flushed by a background worker, so the queue
	// must be drained before the rows can be read. Close would drain it too but
	// would also close the database, so the dedicated flush hook is what a test
	// wants: reading before the drain would race the writer and the assertion
	// would be meaningless.
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
	// And the persisted row must not name a provider that does not exist.
	for _, r := range rows {
		if r.Provider == "cache" {
			t.Fatalf("a persisted row attributes the request to a synthetic %q provider: %+v", "cache", r)
		}
	}
}

// TestRoutingFailureAlsoCreatesNoBucket guards the M9 half against regression
// from the same refactor that touched the cache path, so the two stay
// consistent: neither invents a provider.
func TestRoutingFailureAlsoCreatesNoBucket(t *testing.T) {
	obs := observ.New(true)
	h := newHarness(t, Deps{
		Registry: registry.New(nil, nil), // nothing routable at all
		Router:   newRouter(t, registry.New(nil, nil)),
		Observ:   obs,
	})
	doPost(t, h, "", simpleBody) // unroutable alias

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
