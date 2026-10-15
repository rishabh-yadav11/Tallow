package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

type countUpstream struct {
	srv   *httptest.Server
	hits  atomic.Int64
	peak  atomic.Int64
	live  atomic.Int64
	delay time.Duration

	mu     sync.Mutex
	bodies []map[string]any
}

func newCountUpstream(t *testing.T, delay time.Duration) *countUpstream {
	t.Helper()
	u := &countUpstream{delay: delay}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		live := u.live.Add(1)
		for {
			old := u.peak.Load()
			if live <= old || u.peak.CompareAndSwap(old, live) {
				break
			}
		}
		defer u.live.Add(-1)

		if u.delay > 0 {
			time.Sleep(u.delay)
		}
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		u.mu.Lock()
		u.bodies = append(u.bodies, m)
		u.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_x","object":"response","model":%q,`+
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}],`+
			`"usage":{"input_tokens":5,"output_tokens":3}}`, m["model"])
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *countUpstream) lastBody() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return nil
	}
	return u.bodies[len(u.bodies)-1]
}

func newHarness(t *testing.T, d Deps) *Handler {
	t.Helper()
	if d.Client == nil {
		d.Client = &http.Client{

			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if d.MaxConcurrent == 0 {
		d.MaxConcurrent = 32
	}
	h := NewHandler(d)
	return h
}

func oneAliasRegistry(baseURL string) *registry.Registry {
	return registry.New(
		[]model.Provider{{
			Name:    "p1",
			BaseURL: baseURL,
			Keys:    []model.Key{{ID: "k1", Secret: "sk-test", RPM: 10000}},
		}},
		[]model.Alias{{
			Name:    "flash",
			Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}},
		}},
	)
}

func newRouter(t *testing.T, reg *registry.Registry) *routing.Router {
	t.Helper()
	r := routing.NewRouter(reg, health.NewRegistry(), time.Minute, time.Now)
	limits := map[string]budget.KeyLimits{}
	rpm := map[string]int{}
	for _, p := range reg.Providers() {
		rpm[p.Name] = 1_000_000
		for _, k := range p.Keys {
			limits[p.Name+"/"+k.ID] = budget.KeyLimits{
				RPM:             1_000_000,
				MaxRequests:     1_000_000,
				Window:          time.Minute,
				MaxConcurrent:   1_000,
				CostLimitMicros: 1_000_000_000_000,
			}
		}
	}
	r.SetBudgets(limits, rpm)
	return r
}

func doPost(t *testing.T, h *Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

const simpleBody = `{"model":"flash","input":"hi"}`

func (h *Handler) cacheKeyForTest(body string) string {
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		panic(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	k, ok := h.cacheKey(r, req, "flash", false)
	if !ok {
		return ""
	}
	return k
}

func TestC2CacheIsScopedPerTenant(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	h := newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        c,
		CacheEnabled: true,
		Observ:       observ.New(true),
		Auth:         func(tok string) bool { return tok == "key-alice" || tok == "key-bob" },
	})

	if rec := doPost(t, h, "key-alice", simpleBody); rec.Code != http.StatusOK {
		t.Fatalf("alice status %d: %s", rec.Code, rec.Body.String())
	}
	if up.hits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit after alice, got %d", up.hits.Load())
	}

	rec := doPost(t, h, "key-bob", simpleBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob status %d: %s", rec.Code, rec.Body.String())
	}
	if hits := up.hits.Load(); hits != 2 {
		t.Fatalf("C2: bob was served from alice's cache; upstream hits = %d, want 2", hits)
	}

	if rec := doPost(t, h, "key-alice", simpleBody); rec.Code != http.StatusOK {
		t.Fatalf("alice repeat status %d", rec.Code)
	}
	if hits := up.hits.Load(); hits != 2 {
		t.Fatalf("C2: alice's own repeat should be a cache hit; hits = %d, want 2", hits)
	}

	if rec := doPost(t, h, "key-bob", simpleBody); rec.Code != http.StatusOK {
		t.Fatalf("bob repeat status %d", rec.Code)
	}
	if hits := up.hits.Load(); hits != 2 {
		t.Fatalf("C2: bob's own repeat should be a cache hit; hits = %d, want 2", hits)
	}
}

func TestC2CacheKeyNeverContainsTheCredential(t *testing.T) {
	const secret = "key-super-secret-value"
	key := cacheScope("m", secret, "bodyhash")
	if strings.Contains(key, secret) {
		t.Fatalf("cache key embeds the raw credential: %s", key)
	}
	if strings.Contains(key, "m/") && !strings.HasPrefix(key, "m/") {
		t.Fatalf("unexpected key shape: %s", key)
	}

	if key == cacheScope("m", "other", "bodyhash") {
		t.Fatal("two different credentials collided in the same cache namespace")
	}

	if key != cacheScope("m", secret, "bodyhash") {
		t.Fatal("cache key is not stable for the same credential")
	}
}

func TestC2OpenGatewayUsesOneNamedNamespace(t *testing.T) {
	got := cacheScope("m", "", "bodyhash")
	if !strings.Contains(got, anonymousScope) {
		t.Fatalf("open-gateway scope should be self-describing, got %s", got)
	}
	if got == cacheScope("m", "real-token", "bodyhash") {
		t.Fatal("an open-gateway entry could collide with an authenticated tenant's entry")
	}
}

func TestH4AllRoutesRequireAuth(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Observ:   observ.New(true),
		Auth:     func(tok string) bool { return tok == "good" },
	})
	routes := h.Routes()

	for _, path := range []string{
		"/v1/models",
		"/v1/models/flash",
		"/v1/responses",
	} {
		t.Run("unauthenticated "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("H4: %s returned %d to an unauthenticated client, want 401", path, rec.Code)
			}

			if strings.Contains(rec.Body.String(), "flash") {
				t.Fatalf("H4: the 401 body leaked registry content: %s", rec.Body.String())
			}
		})
		t.Run("authenticated "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer good")
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("H4: %s rejected a valid credential", path)
			}
		})
	}
}

func TestH4HealthStaysOpen(t *testing.T) {
	reg := oneAliasRegistry("http://127.0.0.1:1")
	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Auth:     func(string) bool { return false },
	})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/health should stay open for probes, got %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "flash") || strings.Contains(body, "p1") {
		t.Fatalf("/health leaked configuration: %s", body)
	}
}

func TestH8ConcurrentMissesFanOutToOneUpstreamCall(t *testing.T) {
	const n = 40

	up := newCountUpstream(t, 60*time.Millisecond)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	h := newHarness(t, Deps{
		Router:        newRouter(t, reg),
		Registry:      reg,
		Cache:         c,
		CacheEnabled:  true,
		Observ:        observ.New(true),
		MaxConcurrent: n + 8,
	})
	routes := h.Routes()

	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(simpleBody))
			rec := httptest.NewRecorder()
			<-start
			routes.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d got %d, want 200", i, c)
		}
	}
	if hits := up.hits.Load(); hits != 1 {
		t.Fatalf("H8: %d concurrent identical requests produced %d upstream calls, want 1", n, hits)
	}
	if peak := up.peak.Load(); peak != 1 {
		t.Fatalf("H8: peak upstream concurrency was %d, want 1", peak)
	}
}

func TestH8CoalescedFollowersStillGetTheBody(t *testing.T) {
	const n = 8
	up := newCountUpstream(t, 20*time.Millisecond)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	h := newHarness(t, Deps{
		Router:        newRouter(t, reg),
		Registry:      reg,
		Cache:         c,
		CacheEnabled:  true,
		Observ:        observ.New(true),
		MaxConcurrent: n + 4,
	})
	routes := h.Routes()

	var wg sync.WaitGroup
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(simpleBody))
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	for i, b := range bodies {
		if !strings.Contains(b, "pong") {
			t.Fatalf("follower %d received %q, want the upstream content", i, b)
		}
	}
}

func TestH8CoalescingDoesNotMergeDistinctBodies(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	h := newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        c,
		CacheEnabled: true,
		Observ:       observ.New(true),
	})

	a := `{"model":"flash","input":"alpha"}`
	b := `{"model":"flash","input":"beta"}`
	recA := doPost(t, h, "", a)
	recB := doPost(t, h, "", b)
	if recA.Code != http.StatusOK || recB.Code != http.StatusOK {
		t.Fatalf("statuses %d %d", recA.Code, recB.Code)
	}
	if up.hits.Load() != 2 {
		t.Fatalf("distinct bodies must each reach the upstream, hits=%d", up.hits.Load())
	}
}

func TestH8StreamingIsNotCoalesced(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	h := newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        c,
		CacheEnabled: true,
		Observ:       observ.New(true),
	})
	streamBody := `{"model":"flash","stream":true,"input":"hi"}`
	doPost(t, h, "", streamBody)
	doPost(t, h, "", streamBody)
	if up.hits.Load() != 2 {
		t.Fatalf("streaming requests must each reach the upstream, hits=%d", up.hits.Load())
	}

	if s, _ := up.lastBody()["stream"].(bool); !s {
		t.Fatal("the upstream body lost the stream flag")
	}
}

func TestH8CoalescedWaitersDoNotLeakConcurrencySlots(t *testing.T) {
	const n = 24
	up := newCountUpstream(t, 40*time.Millisecond)
	reg := oneAliasRegistry(up.srv.URL)
	r := newRouter(t, reg)
	h := newHarness(t, Deps{
		Router:        r,
		Registry:      reg,
		Cache:         cache.New(64, time.Minute, 0),
		CacheEnabled:  true,
		Observ:        observ.New(true),
		MaxConcurrent: n + 8,
	})
	routes := h.Routes()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(simpleBody))
			routes.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	if hits := up.hits.Load(); hits != 1 {
		t.Fatalf("expected 1 upstream call for %d coalesced callers, got %d", n, hits)
	}
	if inflight := r.InFlight("p1", "k1"); inflight != 0 {
		t.Fatalf("H8: %d concurrency slots still held after the flight settled, want 0", inflight)
	}
}

func TestH8AccountingIsSettledExactlyOnce(t *testing.T) {
	const n = 16
	up := newCountUpstream(t, 40*time.Millisecond)
	reg := registry.New(
		[]model.Provider{{
			Name: "p1", BaseURL: up.srv.URL,
			Keys: []model.Key{{ID: "k1", Secret: "sk-test", RPM: 1_000_000}},
		}},
		[]model.Alias{{
			Name: "flash",
			Targets: []model.Target{{
				Provider: "p1", Model: "flash-v4",

				PriceInputPerM:  1,
				PriceOutputPerM: 2,
			}},
		}},
	)
	c := cache.New(64, time.Minute, 0)
	obs := observ.New(true)
	h := newHarness(t, Deps{
		Router:        newRouter(t, reg),
		Registry:      reg,
		Cache:         c,
		CacheEnabled:  true,
		Observ:        obs,
		MaxConcurrent: n + 8,
	})
	routes := h.Routes()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(simpleBody))
			routes.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	if hits := up.hits.Load(); hits != 1 {
		t.Fatalf("expected 1 upstream call, got %d", hits)
	}

	if got := obs.Snapshot().ByProvider["p1"].CostMicros; got != 11 {
		t.Fatalf("H8: provider cost = %d micro-USD for ONE upstream call, want 11 (%d callers each reported the full cost)",
			got, n)
	}

	if got := obs.Snapshot().CostMicros; got != 11 {
		t.Fatalf("H8: total cost = %d micro-USD, want 11", got)
	}

	s := obs.Snapshot()
	if s.PromptTokens != 5 || s.CompletionTokens != 3 {
		t.Fatalf("H8: tokens = %d/%d, want 5/3 for the single upstream call", s.PromptTokens, s.CompletionTokens)
	}
}

func TestH8FollowersDoNotExtendTheCacheTTL(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	const ttl = 10 * time.Second
	c := cache.New(64, ttl, 0)
	h := newHarness(t, Deps{
		Router:        newRouter(t, reg),
		Registry:      reg,
		Cache:         c,
		CacheEnabled:  true,
		Observ:        observ.New(true),
		MaxConcurrent: 16,
	})

	key := h.cacheKeyForTest(simpleBody)
	if key == "" {
		t.Fatal("the request was not cacheable, so this test would prove nothing")
	}

	blocked := make(chan struct{}, 8)
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})

	doPost(t, h, "", simpleBody)
	leaderExpiry := c.ExpiresAt(key)
	if leaderExpiry.IsZero() {
		t.Fatal("the leader did not write a cache entry")
	}

	c.Delete(key)
	go h.Routes().ServeHTTP(&blockingWriter{
		header: http.Header{}, blocked: blocked, release: releaseA,
	}, newBodyRequest())
	<-blocked
	followerOnlyExpiry := c.ExpiresAt(key)
	if followerOnlyExpiry.IsZero() {
		t.Fatal("the solo follower did not write a cache entry")
	}
	close(releaseA)

	c.Delete(key)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		doPost(t, h, "", simpleBody)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.Routes().ServeHTTP(&blockingWriter{
			header: http.Header{}, blocked: blocked, release: releaseB,
		}, newBodyRequest())
	}()

	<-blocked
	expiryBeforeRelease := c.ExpiresAt(key)
	if expiryBeforeRelease.IsZero() {
		t.Fatal("the leader did not write an entry while the follower was blocked")
	}

	close(releaseB)
	wg.Wait()

	after := c.ExpiresAt(key)
	if after.After(expiryBeforeRelease) {
		t.Fatalf("H8: a follower re-wrote the shared entry and extended its expiry by %s "+
			"(leader wrote %s, a lone follower would write %s, after the flight %s)",
			after.Sub(expiryBeforeRelease), leaderExpiry, followerOnlyExpiry, after)
	}
}

func newBodyRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(simpleBody))
}

type blockingWriter struct {
	header     http.Header
	blocked    chan<- struct{}
	release    <-chan struct{}
	signalled  bool
	statusCode int
	body       strings.Builder
}

func (w *blockingWriter) Header() http.Header  { return w.header }
func (w *blockingWriter) WriteHeader(code int) { w.statusCode = code }
func (w *blockingWriter) Write(p []byte) (int, error) {
	if !w.signalled {
		w.signalled = true
		w.blocked <- struct{}{}
		<-w.release
	}
	return w.body.Write(p)
}

func TestH6OversizedBodyIsRejected(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Observ:   observ.New(true),

		MaxRequestBytes: 4096,
	})

	huge := `{"model":"flash","input":"` +
		strings.Repeat("A", 1<<20) + `"}`
	rec := doPost(t, h, "", huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("H6: oversized body returned %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if up.hits.Load() != 0 {
		t.Fatal("an oversized body must never reach the upstream")
	}
}

func TestH6DefaultBodyLimitIsBounded(t *testing.T) {
	if maxRequestBytesDefault <= 0 {
		t.Fatal("H6: the default request body limit is unbounded")
	}

	if maxRequestBytesDefault != 8<<20 {
		t.Fatalf("H6: default limit is %d, want %d", maxRequestBytesDefault, 8<<20)
	}
}

func TestH6BodyAtExactlyTheLimitIsAccepted(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	const limit = 4096
	h := newHarness(t, Deps{
		Router:          newRouter(t, reg),
		Registry:        reg,
		Observ:          observ.New(true),
		MaxRequestBytes: limit,
	})

	prefix := `{"model":"flash","input":"`
	suffix := `"}`
	pad := limit - len(prefix) - len(suffix)
	body := prefix + strings.Repeat("A", pad) + suffix
	if len(body) != limit {
		t.Fatalf("test body is %d bytes, want exactly %d", len(body), limit)
	}
	if rec := doPost(t, h, "", body); rec.Code != http.StatusOK {
		t.Fatalf("a body exactly at the limit must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
	if up.hits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", up.hits.Load())
	}
}

func TestM3UpstreamBodyDoesNotMutateTheCallersMap(t *testing.T) {
	req := map[string]any{"model": "flash", "input": []any{"hi"}}
	base := baseBody(req)

	got := string(upstreamBody(base, "provider-model-X"))

	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("upstreamBody produced invalid JSON: %v (%s)", err, got)
	}
	if parsed["model"] != "provider-model-X" {
		t.Fatalf("model = %v, want provider-model-X", parsed["model"])
	}
	if req["model"] != "flash" {
		t.Fatalf("M3: upstreamBody mutated the caller's map, model is now %v", req["model"])
	}

	if a, b := string(upstreamBody(base, "p1")), string(upstreamBody(base, "p2")); a == b {
		t.Fatal("two different model names produced the same body")
	}
}

func TestM3RetryAfterFailureUsesTheNewProvidersModel(t *testing.T) {

	live := newCountUpstream(t, 0)
	reg := registry.New(
		[]model.Provider{
			{Name: "pdead", BaseURL: "http://127.0.0.1:1", Keys: []model.Key{{ID: "kdead", Secret: "sk-dead", RPM: 100}}},
			{
				Name: "plive", BaseURL: live.srv.URL,
				Keys: []model.Key{{ID: "klive", Secret: "sk-live", RPM: 100}},
			},
		},
		[]model.Alias{{
			Name: "flash",
			Targets: []model.Target{
				{Provider: "pdead", Model: "dead-model"},
				{Provider: "plive", Model: "live-model"},
			},
		}},
	)
	r := routing.NewRouter(reg, health.NewRegistry(), time.Minute, time.Now)
	r.SetBudgets(map[string]budget.KeyLimits{
		"pdead/kdead": {RPM: 1_000_000, Window: time.Minute, CostLimitMicros: 1_000_000_000_000},
		"plive/klive": {RPM: 1_000_000, Window: time.Minute, CostLimitMicros: 1_000_000_000_000},
	}, map[string]int{"pdead": 1_000_000, "plive": 1_000_000})

	h := newHarness(t, Deps{
		Router:   r,
		Registry: reg,
		Observ:   observ.New(true),
	})
	rec := doPost(t, h, "", simpleBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if live.hits.Load() != 1 {
		t.Fatalf("expected the live provider to be retried, hits=%d", live.hits.Load())
	}
	last := live.lastBody()
	if last["model"] != "live-model" {
		t.Fatalf("M3: the retry sent model %v, want live-model (the dead provider's model leaked through)", last["model"])
	}
}

func TestM3SplicePreservesEveryOtherField(t *testing.T) {
	req := map[string]any{
		"model":       "flash",
		"stream":      true,
		"temperature": 0.7,
		"input":       []any{map[string]any{"role": "user", "content": "hi"}},
		"user":        "alice",
		"metadata":    map[string]any{"k": "v"},
	}
	base := baseBody(req)
	var parsed map[string]any
	if err := json.Unmarshal(upstreamBody(base, "real-model"), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, k := range []string{"stream", "temperature", "input", "user", "metadata"} {
		if _, ok := parsed[k]; !ok {
			t.Fatalf("M3: splicing the model dropped the %q field", k)
		}
	}
	if parsed["model"] != "real-model" {
		t.Fatalf("model = %v", parsed["model"])
	}
}

func TestM3SpliceEscapesAwkwardModelNames(t *testing.T) {
	req := map[string]any{"model": "flash", "input": []any{"hi"}}
	base := baseBody(req)
	for _, name := range []string{`we"ird`, `back\slash`, "new\nline", "中文模型", ""} {
		var parsed map[string]any
		raw := upstreamBody(base, name)
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("model %q produced invalid JSON: %v (%s)", name, err, raw)
		}
		if parsed["model"] != name {
			t.Fatalf("model %q round-tripped to %v", name, parsed["model"])
		}
	}
}

func TestM3SpliceIgnoresNestedModelFields(t *testing.T) {
	req := map[string]any{
		"model": "flash",
		"input": []any{map[string]any{
			"role": "user",

			"content": map[string]any{"model": "decoy", "x": 1},
		}},
	}
	base := baseBody(req)
	var parsed map[string]any
	if err := json.Unmarshal(upstreamBody(base, "real-model"), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["model"] != "real-model" {
		t.Fatalf("top-level model = %v, want real-model (a nested decoy was matched)", parsed["model"])
	}
}

func TestM7CacheHitReportsNoTokensOrCost(t *testing.T) {
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
	doPost(t, h, "", simpleBody)
	doPost(t, h, "", simpleBody)

	s := obs.Snapshot()
	if s.Cached != 2 {
		t.Fatalf("expected 2 cache hits, got %d", s.Cached)
	}
	if s.Total != 3 {
		t.Fatalf("expected 3 total requests, got %d", s.Total)
	}

	if s.PromptTokens != 5 || s.CompletionTokens != 3 {
		t.Fatalf("M7: tokens = %d/%d, want 5/3 (cache hits re-added the original request's tokens)",
			s.PromptTokens, s.CompletionTokens)
	}

	if _, ok := s.ByProvider["cache"]; ok {
		t.Fatalf("M7: by_provider contains a synthetic %q bucket (%+v); a cache hit is not a "+
			"provider and must not be attributed to one", "cache", s.ByProvider["cache"])
	}

	real := s.ByProvider["p1"]
	if real.Requests != 1 {
		t.Fatalf("the real provider should own the single upstream call, got %+v", real)
	}

	for name := range s.ByProvider {
		if name != "p1" {
			t.Fatalf("by_provider contains a bucket for %q, which is not a configured provider; "+
				"a request that no provider served must not be attributed to one (M9/M7)", name)
		}
	}
}

func TestM7CacheStillTracksEffectiveness(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	c := cache.New(64, time.Minute, 0)
	h := newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        c,
		CacheEnabled: true,
		Observ:       observ.New(true),
	})
	doPost(t, h, "", simpleBody)
	doPost(t, h, "", simpleBody)
	hit, miss, _, _ := c.Stats()
	if hit != 1 || miss != 1 {
		t.Fatalf("cache stats hit=%d miss=%d, want 1/1", hit, miss)
	}
}

func TestM9NoRouteDoesNotInventAProvider(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	obs := observ.New(true)
	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Observ:   obs,
	})

	rec := doPost(t, h, "", `{"model":"no-such-alias","input":"hi"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for an unroutable alias, got %d", rec.Code)
	}
	s := obs.Snapshot()
	if s.Total != 1 || s.Errors != 1 {
		t.Fatalf("the failure must be counted globally: total=%d errors=%d", s.Total, s.Errors)
	}
	if phantom, ok := s.ByProvider[""]; ok {
		t.Fatalf("M9: a phantom %q provider bucket was created: %+v", "", phantom)
	}
	if len(s.ByProvider) != 0 {
		t.Fatalf("M9: a provider was invented for a routing failure: %+v", s.ByProvider)
	}
}

func TestM9RealProviderFailureIsStillAttributed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	reg := oneAliasRegistry(srv.URL)
	obs := observ.New(true)
	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Observ:   obs,
	})
	rec := doPost(t, h, "", simpleBody)
	if rec.Code == http.StatusOK {
		t.Fatal("expected the upstream 401 to be surfaced")
	}
	s := obs.Snapshot()
	p, ok := s.ByProvider["p1"]
	if !ok {
		t.Fatalf("M9: a real provider failure was not attributed: %+v", s.ByProvider)
	}
	if p.Requests != 1 || p.Errors != 1 {
		t.Fatalf("M9: p1 stats = %+v, want 1 request / 1 error", p)
	}
}

func TestM9StoreStillRecordsTheRoutingError(t *testing.T) {
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	st, err := store.Open(store.StoreConfig{
		Path:        filepath.Join(t.TempDir(), "tallow.db"),
		RollupEvery: time.Hour,
		VacuumEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Store:    st,
		Observ:   observ.New(true),
	})
	rec := doPost(t, h, "", `{"model":"no-such-alias","input":"hi"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d", rec.Code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err := st.RecentRequests(10)
		if err != nil {
			t.Fatalf("read recent requests: %v", err)
		}
		if len(rows) == 1 {
			if rows[0].Status != "error" {
				t.Fatalf("M9: the request was not recorded as an error: %+v", rows[0])
			}
			if rows[0].Err == "" {
				t.Fatal("M9: the routing error message was dropped entirely")
			}
			if rows[0].Provider != "" {
				t.Fatalf("M9: a provider was invented for the stored row: %q", rows[0].Provider)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("M9: the routing failure was never persisted (rows=%d)", len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
