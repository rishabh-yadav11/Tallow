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

func newProbeApp(t *testing.T, providers []model.Provider, aliases []model.Alias) *App {
	t.Helper()
	a := &App{reg: registry.New(providers, aliases)}
	a.health = health.NewRegistry()
	a.probeSem = make(chan struct{}, 4)

	a.client = newHTTPClient()
	return a
}

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

	if gotPath != "/responses" {
		t.Errorf("probe path = %q, want /responses", gotPath)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-secret", gotAuth)
	}
	for _, want := range []string{`"model":"m1"`, `"max_output_tokens":1`, `"stream":false`, `"input":"ping"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("probe body missing %s: %s", want, gotBody)
		}
	}

	if s := a.health.Provider("p1").State(time.Now()).String(); s != "closed" {
		t.Errorf("provider state = %q after a 200 probe, want closed", s)
	}
	if s := a.health.Key("p1", "k1").State(time.Now()).String(); s != "closed" {
		t.Errorf("key state = %q after a 200 probe, want closed", s)
	}
}

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

func TestProbeRecordsFailureOnTransportError(t *testing.T) {
	a := newProbeApp(t,
		[]model.Provider{{Name: "p1", BaseURL: "http://127.0.0.1:1"}},
		[]model.Alias{{Name: "al", Targets: []model.Target{{Provider: "p1", Model: "m1"}}}})

	p := model.Provider{Name: "p1", BaseURL: "http://127.0.0.1:1"}

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
		if gotPath != "/responses" {
			t.Errorf("base URL %q produced path %q, want /responses", base, gotPath)
		}
		if s := a.health.Provider("p1").State(time.Now()).String(); s == "open" {
			t.Errorf("base URL %q opened the breaker on a 200: %q", base, s)
		}
	}
}

func TestProbeDoesNotLeakKeyOnRedirect(t *testing.T) {
	var leaked atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/responses", http.StatusTemporaryRedirect)
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

func TestProbeSurvivesCancellation(t *testing.T) {
	handlerReleased := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-handlerReleased:
		}
	}))

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

	time.Sleep(time.Duration(ticks) * 5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("healthLoop did not return after its context was cancelled")
	}

	time.Sleep(100 * time.Millisecond)
}

func TestHealthLoopProbesOnlyEligibleProviders(t *testing.T) {
	var probed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newProbeApp(t,
		[]model.Provider{

			{Name: "off", BaseURL: srv.URL, Keys: []model.Key{{ID: "k"}}},

			{Name: "nomodel", BaseURL: srv.URL, HealthCheck: true, Keys: []model.Key{{ID: "k"}}},

			{Name: "on", BaseURL: srv.URL, HealthCheck: true, Keys: []model.Key{{ID: "k"}}},
		},
		[]model.Alias{
			{Name: "al", Targets: []model.Target{{Provider: "on", Model: "m1"}}},
		})

	runHealthLoopTicks(t, a, 1)

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

	if s := a.health.Provider("on").State(time.Now()).String(); s == "open" {
		t.Errorf("the probed provider opened on a 200: %q", s)
	}
}

func TestHealthLoopHonorsPerProviderInterval(t *testing.T) {
	var probed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newProbeApp(t,
		[]model.Provider{

			{Name: "on", BaseURL: srv.URL, HealthCheck: true, HealthInterval: time.Hour,
				Keys: []model.Key{{ID: "k"}}},
		},
		[]model.Alias{
			{Name: "al", Targets: []model.Target{{Provider: "on", Model: "m1"}}},
		})

	runHealthLoopTicks(t, a, 20)

	if n := probed.Load(); n != 1 {
		t.Errorf("%d probes sent across 20 ticks, want 1: the per-provider "+
			"health_interval was not honored", n)
	}
}

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
