package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/resilience"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

func jsonMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, body)
	}
	return m
}

func TestResponsesForwardUpstreamPath(t *testing.T) {
	var upstreamPath, upstreamBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1016)
		n, _ := r.Body.Read(buf)
		upstreamBody = string(buf[:n])
		upstreamPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_x","object":"response","model":"flash-v4","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer up.Close()

	reg := oneAliasRegistry(up.URL)
	h := newHarness(t, Deps{Router: newRouter(t, reg), Registry: reg, Observ: observ.New(true)})

	rec := doPost(t, h, "", simpleBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", upstreamPath)
	}
	sent := jsonMap(t, upstreamBody)
	if sent["model"] != "flash-v4" {
		t.Errorf("forwarded model = %v, want flash-v4 (the alias must map to the upstream model)", sent["model"])
	}
	if sent["stream"] != nil {
		t.Errorf("non-stream request must not gain a stream flag: %v", sent["stream"])
	}
}

func TestForceMappingNonStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_x","object":"response","model":"provider-real-name","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer up.Close()

	reg := registry.New(
		[]model.Provider{{Name: "p1", BaseURL: up.URL, Keys: []model.Key{{ID: "k1", Secret: "sk", RPM: 10000}}}},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4", ForceMapping: true}}}},
	)
	h := newHarness(t, Deps{Router: newRouter(t, reg), Registry: reg, Observ: observ.New(true)})

	rec := doPost(t, h, "", simpleBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := jsonMap(t, rec.Body.String())["model"]; got != "flash" {
		t.Errorf("response model = %v, want the alias %q under ForceMapping", got, "flash")
	}
}

func TestForceMappingStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"model\":\"provider-real-name\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer up.Close()

	reg := registry.New(
		[]model.Provider{{Name: "p1", BaseURL: up.URL, Keys: []model.Key{{ID: "k1", Secret: "sk", RPM: 10000}}}},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4", ForceMapping: true}}}},
	)
	h := newHarness(t, Deps{Router: newRouter(t, reg), Registry: reg, Observ: observ.New(true)})

	rec := doPost(t, h, "", `{"model":"flash","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"delta":"hi"`) {
		t.Errorf("stream body lost the delta event: %q", body)
	}
	if !strings.Contains(body, `"type":"response.completed","model":"flash"`) {
		t.Errorf("completed event model was not rewritten to the alias: %q", body)
	}
	if strings.Contains(body, "provider-real-name") {
		t.Errorf("stream leaked the upstream real model name under ForceMapping: %q", body)
	}
}

func TestKeepaliveEmitsCommentsWhileIdle(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n")
		fl.Flush()
		time.Sleep(120 * time.Millisecond)
		fmt.Fprint(w, "data: [DONE]\n")
		fl.Flush()
	}))
	defer up.Close()

	reg := oneAliasRegistry(up.URL)
	h := newHarness(t, Deps{
		Router:    newRouter(t, reg),
		Registry:  reg,
		Observ:    observ.New(true),
		Keepalive: 40 * time.Millisecond,
	})

	rec := doPost(t, h, "", `{"model":"flash","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ": keepalive") {
		t.Errorf("an idle stream with Keepalive set must emit keepalive comments: %q", rec.Body.String())
	}
}

func TestResilienceCooldownExcludesCredentialAfterStop(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer up.Close()

	reg := registry.New(
		[]model.Provider{{Name: "p1", BaseURL: up.URL, Keys: []model.Key{{
			ID: "k1", Secret: "sk", RPM: 10000,
			ErrorRules: []model.ErrorRule{{Status: 429, Action: "stop-and-cooldown"}},
		}}}},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}},
	)
	r := routing.NewRouter(reg, health.NewRegistry(), time.Minute, time.Now)
	r.SetBudgets(map[string]budget.KeyLimits{"p1/k1": {RPM: 10000, Window: time.Minute, CostLimitMicros: 1e12}}, map[string]int{"p1": 10000})

	mgr := resilience.NewManager(resilience.RetryPolicy{MaxRetryCredentials: 1}, "", nil)
	r.SetExcluder(mgr.Excludes())

	h := newHarness(t, Deps{Router: r, Registry: reg, Observ: observ.New(true), Resilience: mgr})

	rec := doPost(t, h, "", simpleBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the upstream 429 to surface, got %d", rec.Code)
	}
	if !mgr.IsCooled("p1", "k1") {
		t.Fatal("stop-and-cooldown must cool the failing credential")
	}

	rec2 := doPost(t, h, "", `{"model":"flash","input":"again"}`)
	if rec2.Code != http.StatusBadGateway {
		t.Errorf("a cooled credential must be excluded from routing; got %d: %s", rec2.Code, rec2.Body.String())
	}
}
