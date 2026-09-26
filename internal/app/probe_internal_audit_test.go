package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

// This file is in-package (package app) so it can drive the unexported probe and
// trimSlash directly. probe is the health-check path: it records breaker
// success or failure from a single 1-token request, so getting its pass/fail
// verdict wrong silently corrupts failover for the whole provider.

// newProbeApp builds the minimum App that probe needs: a registry, a health
// tracker, and a semaphore.
func newProbeApp(t *testing.T, providers []model.Provider, aliases []model.Alias) *App {
	t.Helper()
	a := &App{reg: registry.New(providers, aliases)}
	a.health = health.NewRegistry()
	a.probeSem = make(chan struct{}, 4)
	// healthLoop reads a.client under a.mu and hands it straight to probe, so
	// it must be non-nil even in tests that never construct a real App.
	a.client = newHTTPClient()
	return a
}

// TestProbeRecordsSuccessOn2xx is the happy path. A 200 from the upstream must
// record a success for BOTH the provider and the key, or failover would treat a
// healthy provider as broken.
func TestProbeRecordsSuccessOn2xx(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newProbeApp(t,
		[]model.Provider{{Name: "p1", BaseURL: srv.URL}},
		[]model.Alias{{Name: "al", Targets: []model.Target{{Provider: "p1", Model: "m1"}}}})

	p := model.Provider{Name: "p1", BaseURL: srv.URL}
	k := model.Key{ID: "k1", Secret: "sk-secret"}
	a.probe(a.reg, srv.Client(), p, k, "m1")

	// The request must be a well-formed, minimal chat completion.
	if gotPath != "/chat/completions" {
		t.Errorf("probe path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-secret", gotAuth)
	}
	for _, want := range []string{`"model":"m1"`, `"max_tokens":1`, `"stream":false`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("probe body missing %s: %s", want, gotBody)
		}
	}
	// Both levels must record success. Assert the EXACT state, not merely
	// "not open": a breaker with no recorded result is also "not open", so a
	// weaker assertion would pass even if probe recorded nothing at all, and
	// that is the regression this test exists to catch.
	if s := a.health.Provider("p1").State(time.Now()).String(); s != "closed" {
		t.Errorf("provider state = %q after a 200 probe, want closed", s)
	}
	if s := a.health.Key("p1", "k1").State(time.Now()).String(); s != "closed" {
		t.Errorf("key state = %q after a 200 probe, want closed", s)
	}
}

// TestProbeRecordsFailureOnErrorStatus is the important negative. A 401 from
// the provider must open the breaker, because a bad credential should be taken
// out of rotation immediately rather than after the retry budget.
func TestProbeRecordsFailureOnErrorStatus(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			a := newProbeApp(t,
				[]model.Provider{{Name: "p1", BaseURL: srv.URL}},
				[]model.Alias{{Name: "al", Targets: []model.Target{{Provider: "p1", Model: "m1"}}}})

			p := model.Provider{Name: "p1", BaseURL: srv.URL}
			a.probe(a.reg, srv.Client(), p, model.Key{ID: "k1", Secret: "sk"}, "m1")

			// Repeated probes must eventually open it, proving failures count.
			for i := 0; i < 10; i++ {
				a.probe(a.reg, srv.Client(), p, model.Key{ID: "k1", Secret: "sk"}, "m1")
			}
			if s := a.health.Provider("p1").State(time.Now()).String(); s != "open" {
				t.Errorf("provider state = %q after 11 failed probes at %d, want open",
					s, code)
			}
			if s := a.health.Key("p1", "k1").State(time.Now()).String(); s != "open" {
				t.Errorf("key state = %q after 11 failed probes at %d, want open", s, code)
			}
		})
	}
}

// TestProbeRecordsFailureOnTransportError covers the case where the request
// never completes. A dead provider must still be recorded as failing, or the
// breaker stays closed and every request keeps waiting out the connect timeout.
func TestProbeRecordsFailureOnTransportError(t *testing.T) {
	a := newProbeApp(t,
		[]model.Provider{{Name: "p1", BaseURL: "http://127.0.0.1:1"}},
		[]model.Alias{{Name: "al", Targets: []model.Target{{Provider: "p1", Model: "m1"}}}})

	p := model.Provider{Name: "p1", BaseURL: "http://127.0.0.1:1"}
	// An unreachable address: client.Do returns an error and no response.
	for i := 0; i < 10; i++ {
		a.probe(a.reg, http.DefaultClient, p, model.Key{ID: "k1", Secret: "sk"}, "m1")
	}
	if s := a.health.Provider("p1").State(time.Now()).String(); s == "closed" {
		t.Error("provider stayed closed after 10 probes to an unreachable address")
	}
	if s := a.health.Key("p1", "k1").State(time.Now()).String(); s == "closed" {
		t.Error("key stayed closed after 10 probes to an unreachable address")
	}
}

// TestProbeNormalizesTrailingSlashesOnBaseURL pins that a base URL with a
// trailing slash does not produce a double-slash path. An upstream that
// 404s on "//chat/completions" would be recorded as a permanent failure, which
// is exactly the kind of false alarm that takes a healthy provider out of
// rotation.
func TestProbeNormalizesTrailingSlashesOnBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "///"} {
		a := newProbeApp(t,
			[]model.Provider{{Name: "p1", BaseURL: base}},
			[]model.Alias{{Name: "al", Targets: []model.Target{{Provider: "p1", Model: "m1"}}}})

		p := model.Provider{Name: "p1", BaseURL: base}
		a.probe(a.reg, srv.Client(), p, model.Key{ID: "k1", Secret: "sk"}, "m1")
		if gotPath != "/chat/completions" {
			t.Errorf("base URL %q produced path %q, want /chat/completions", base, gotPath)
		}
		if s := a.health.Provider("p1").State(time.Now()).String(); s == "open" {
			t.Errorf("base URL %q opened the breaker on a 200: %q", base, s)
		}
	}
}

// TestProbeDoesNotLeakKeyOnRedirect is a security property. The health
// probe carries the provider's real API key, so if the HTTP client followed a
// redirect to an attacker-controlled host, it would exfiltrate the credential.
// The credential must reach only the first hop.
//
// Note the verdict on the 3xx itself is NOT asserted here: probe treats any
// status below 400 as healthy, so a 302 counts as a success. That is a
// separate, much weaker concern than credential exfiltration, and the redirect
// refusal itself is already pinned end-to-end by TestM2UpstreamClientEndToEnd
// in m2_redirect_test.go. This test covers the probe's use of the client, so
// it uses the PRODUCTION client rather than a hand-rolled one.
func TestProbeDoesNotLeakKeyOnRedirect(t *testing.T) {
	var leaked atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	// The provider redirects to the "attacker" host.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	a := newProbeApp(t,
		[]model.Provider{{Name: "p1", BaseURL: srv.URL}},
		[]model.Alias{{Name: "al", Targets: []model.Target{{Provider: "p1", Model: "m1"}}}})

	p := model.Provider{Name: "p1", BaseURL: srv.URL}
	a.probe(a.reg, newHTTPClient(), p, model.Key{ID: "k1", Secret: "sk-secret"}, "m1")

	if leaked.Load() {
		t.Error("the probe followed a redirect and sent the API key to another host")
	}
}

// TestProbeSurvivesCancellation checks probe does not panic or hang when the
// server it is talking to goes away mid-request. healthLoop cancels on
// shutdown, and a probe in flight at that moment must not crash the process
// or wedge the loop.
//
// The handler blocks until the test releases it, so the probe is genuinely in
// flight when the release happens. This is the real assertion: a wedged probe
// would hold a probeSem slot forever and eventually starve the whole health
// loop, so the bound that matters is "does not hang", not "returns within the
// probe's own 10s timeout". Releasing the handler as soon as the probe is in
// flight keeps the test fast while still exercising the in-flight path.
func TestProbeSurvivesCancellation(t *testing.T) {
	handlerReleased := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-handlerReleased:
		}
	}))
	// Close the server exactly once, after the handler is released, so Close
	// cannot block on a handler that is itself blocked on Close.
	t.Cleanup(func() {
		srv.Close()
	})

	a := newProbeApp(t,
		[]model.Provider{{Name: "p1", BaseURL: srv.URL}},
		[]model.Alias{{Name: "al", Targets: []model.Target{{Provider: "p1", Model: "m1"}}}})

	p := model.Provider{Name: "p1", BaseURL: srv.URL}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.probe(a.reg, srv.Client(), p, model.Key{ID: "k1", Secret: "sk"}, "m1")
	}()

	// The probe must return promptly, well inside its own 10s timeout, once the
	// handler releases. This is the real assertion: a wedged probe would hold a
	// probeSem slot forever and eventually starve the whole health loop, so the
	// bound that matters is "does not hang", not "returns within 10s". Release
	// the handler as soon as the probe is in flight so the test stays fast.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(handlerReleased)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not return; it would hold a probeSem slot forever")
	}
}

// runHealthLoopTicks drives healthLoop for a bounded number of ticks with a
// short tick interval, then cancels it. It exists so the loop's scheduling
// decisions can be observed without waiting out the production 15s tick.
func runHealthLoopTicks(t *testing.T, a *App, ticks int) {
	t.Helper()
	old := healthTick
	healthTick = 5 * time.Millisecond
	t.Cleanup(func() { healthTick = old })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.healthLoop(ctx)
	}()

	// Wait for at least `ticks` ticks to have been processed, then stop.
	// A probe is dispatched in its own goroutine, so also give those a moment.
	time.Sleep(time.Duration(ticks) * 5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("healthLoop did not return after its context was cancelled")
	}
	// Let any probe goroutine the loop dispatched finish and settle.
	time.Sleep(100 * time.Millisecond)
}

// TestHealthLoopProbesOnlyEligibleProviders exercises the loop's scheduling.
// Two skips are load-bearing: a provider with health_check off must never be
// probed, and a provider with no alias model has nothing to probe WITH, so
// probing it would send an empty model and guarantee a failure, opening the
// breaker on a perfectly healthy provider at startup.
//
// The third provider is the positive control: a loop that skipped everything
// would otherwise pass this test, and the skip assertions would be vacuous.
func TestHealthLoopProbesOnlyEligibleProviders(t *testing.T) {
	var probed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newProbeApp(t,
		[]model.Provider{
			// health_check defaults to false, so this must be skipped.
			{Name: "off", BaseURL: srv.URL, Keys: []model.Key{{ID: "k"}}},
			// health_check on, but no alias target names a model for it, so the
			// probe model is empty and it must be skipped too.
			{Name: "nomodel", BaseURL: srv.URL, HealthCheck: true, Keys: []model.Key{{ID: "k"}}},
			// health_check on WITH a model, so this one must actually be probed.
			{Name: "on", BaseURL: srv.URL, HealthCheck: true, Keys: []model.Key{{ID: "k"}}},
		},
		[]model.Alias{
			{Name: "al", Targets: []model.Target{{Provider: "on", Model: "m1"}}},
		})

	// One tick is enough: the eligible provider is probed on the first tick, and
	// the per-provider interval check suppresses it on later ones.
	runHealthLoopTicks(t, a, 1)

	// Exactly the eligible provider's single key was probed, and nothing else.
	// Any probe at all for "off" or "nomodel" would break the count, since each
	// has exactly one key and would add one.
	if n := probed.Load(); n != 1 {
		t.Errorf("%d probes sent, want 1: only the provider with health_check on "+
			"and a known model may be probed", n)
	}
	if s := a.health.Provider("off").State(time.Now()).String(); s == "open" {
		t.Error("a provider with health_check off was marked unhealthy by the probe loop")
	}
	if s := a.health.Provider("nomodel").State(time.Now()).String(); s == "open" {
		t.Error("a provider with no alias model was marked unhealthy by the probe loop")
	}
	// The probed provider must be healthy, proving the probe really reached it.
	if s := a.health.Provider("on").State(time.Now()).String(); s == "open" {
		t.Errorf("the probed provider opened on a 200: %q", s)
	}
}

// TestHealthLoopHonorsPerProviderInterval pins the interval throttle. A
// provider probed every tick would burn its API quota continuously, and the
// interval is the only thing preventing that. A second tick must not re-probe.
func TestHealthLoopHonorsPerProviderInterval(t *testing.T) {
	var probed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newProbeApp(t,
		[]model.Provider{
			// health_interval of 1h, far longer than the test runs.
			{Name: "on", BaseURL: srv.URL, HealthCheck: true, HealthInterval: time.Hour,
				Keys: []model.Key{{ID: "k"}}},
		},
		[]model.Alias{
			{Name: "al", Targets: []model.Target{{Provider: "on", Model: "m1"}}},
		})

	// 20 ticks at 5ms is 100ms of loop time, still far under the 1h interval.
	runHealthLoopTicks(t, a, 20)

	if n := probed.Load(); n != 1 {
		t.Errorf("%d probes sent across 20 ticks, want 1: the per-provider "+
			"health_interval was not honored", n)
	}
}

// TestHealthLoopStopsOnContextCancellation checks the loop honours shutdown.
// Run cancels the root context on SIGINT/SIGTERM, and healthLoop returning on
// ctx.Done() is what lets Run return instead of hanging.
func TestHealthLoopStopsOnContextCancellation(t *testing.T) {
	a := newProbeApp(t, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := a.healthLoop(ctx)
		if err == nil {
			t.Error("healthLoop returned nil on cancellation, want ctx.Err()")
		} else if err != context.Canceled {
			t.Errorf("healthLoop returned %v, want context.Canceled", err)
		}
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("healthLoop did not stop when its context was cancelled")
	}
}
