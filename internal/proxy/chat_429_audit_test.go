package proxy

// P2 audit tests for the 429 path and the non-bool stream field:
//   - a provider-level RPM=1 budget turns the second request into a 429 with a
//     Retry-After header, on BOTH the buffered (cache) path and the streaming
//     (serveUncached) path (item 4);
//   - a full concurrency queue answers 429 rate_limit_error at the queue gate
//     (item 5);
//   - a non-bool `"stream":"true"` field is coerced to false and takes the
//     non-streaming path without panicking (item 6).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

func TestProviderRPM429EndToEnd(t *testing.T) {
	// A provider-level RPM cap of 1 lets the first request through and must make
	// the second a 429 rate_limit_error with a usable Retry-After. The 429 path
	// is exercised on both the buffered (cacheable) path and the streaming
	// (serveUncached) path, since the two write the error in different places.
	for _, tc := range []struct {
		name, first, second string
		up                  func(t *testing.T) *countUpstream
	}{
		// The buffered path reads a plain JSON body; the streaming path needs a
		// real SSE stream so the first request actually succeeds. The second
		// body differs from the first in both cases, so the second request is a
		// cache miss and actually reaches routeAndBuffer / serveUncached instead
		// of being served from the cache populated by the first.
		{"buffered", simpleBody,
			`{"model":"flash","messages":[{"role":"user","content":"hi again"}]}`,
			func(t *testing.T) *countUpstream { return newCountUpstream(t, 0) }},
		{"streaming", `{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			`{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi again"}]}`,
			newSSEUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := tc.up(t)
			reg := oneAliasRegistry(up.srv.URL)
			r := routing.NewRouter(reg, health.NewRegistry(), time.Minute, time.Now)
			r.SetBudgets(map[string]budget.KeyLimits{
				"p1/k1": {RPM: 10000, MaxRequests: 10000, Window: time.Minute, MaxConcurrent: 100},
			}, map[string]int{"p1": 1}) // provider RPM cap = 1.
			h := newHarness(t, Deps{
				Router:        r,
				Registry:      reg,
				Cache:         cache.New(64, time.Minute, 0),
				CacheEnabled:  true,
				Observ:        observ.New(true),
				MaxConcurrent: 64,
			})

			if rec := doPost(t, h, "", tc.first); rec.Code != http.StatusOK {
				t.Fatalf("first request: status %d: %s", rec.Code, rec.Body.String())
			}
			rec := doPost(t, h, "", tc.second)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("second request: status %d, want 429: a provider budget "+
					"RPM cap of 1 must throttle the next request", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			var payload struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("429 body is not JSON: %v (%q)", err, rec.Body.String())
			}
			if payload.Error.Type != "rate_limit_error" {
				t.Fatalf("error.type = %q, want rate_limit_error: the client "+
					"must be able to branch on the body of a rate limit",
					payload.Error.Type)
			}
			ra := rec.Header().Get("Retry-After")
			secs, err := strconv.Atoi(ra)
			if err != nil || secs < 1 {
				t.Fatalf("Retry-After = %q, want an integer >= 1 so a client "+
					"knows when to come back", ra)
			}
		})
	}
}

// newSSEUpstream returns a countUpstream-shaped fake whose handler streams a
// real SSE completion, so a streaming request succeeds on the first token.
func newSSEUpstream(t *testing.T) *countUpstream {
	t.Helper()
	u := &countUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func TestQueueFullOverCapacityReturns429(t *testing.T) {
	// A full concurrency queue must answer 429 at the queue gate (chat.go:66):
	// request A holds the single slot blocked on a slow upstream, and request B
	// exceeds QueueTimeout waiting for the next slot.
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","model":"flash-v4","choices":[],"usage":{}}`)
	}))
	defer srv.Close()

	reg := oneAliasRegistry(srv.URL)
	h := newHarness(t, Deps{
		Router:        newRouter(t, reg),
		Registry:      reg,
		Cache:         cache.New(64, time.Minute, 0),
		MaxConcurrent: 1,
		QueueTimeout:  150 * time.Millisecond,
	})

	aDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { aDone <- doPost(t, h, "", simpleBody) }()
	<-entered // A owns the only slot and is blocked upstream.

	recB := doPost(t, h, "", simpleBody)
	if recB.Code != http.StatusTooManyRequests {
		t.Fatalf("request B with a full queue: status %d, want 429 "+
			"rate_limit_error", recB.Code)
	}
	if ct := recB.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("over-capacity Content-Type = %q, want application/json", ct)
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recB.Body.Bytes(), &payload); err != nil {
		t.Fatalf("over-capacity body is not JSON: %v (%q)", err, recB.Body.String())
	}
	if payload.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", payload.Error.Type)
	}

	close(release)
	recA := <-aDone
	if recA.Code != http.StatusOK {
		t.Fatalf("request A after the queue freed: status %d: %s",
			recA.Code, recA.Body.String())
	}
}

func TestNonBoolStreamFieldCoercesToFalse(t *testing.T) {
	// A string `"stream":"true"` fails the bool type assertion and is coerced to
	// false, so the request is treated as non-streaming (no panic). The
	// observable tell: a non-stream request is cacheable, so two identical
	// requests produce exactly one upstream call, and the response is served as
	// application/json rather than text/event-stream.
	up := newCountUpstream(t, 0)
	reg := oneAliasRegistry(up.srv.URL)
	h := newHarness(t, Deps{
		Router:        newRouter(t, reg),
		Registry:      reg,
		Cache:         cache.New(64, time.Minute, 0),
		CacheEnabled:  true,
		Observ:        observ.New(true),
		MaxConcurrent: 64,
	})

	body := `{"model":"flash","stream":"true","messages":[{"role":"user","content":"hi"}]}`
	rec1 := doPost(t, h, "", body)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: status %d: %s", rec1.Code, rec1.Body.String())
	}
	if ct := rec1.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("a string stream field must take the non-streaming path, got "+
			"Content-Type %q", ct)
	}
	rec2 := doPost(t, h, "", body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("repeat request: status %d: %s", rec2.Code, rec2.Body.String())
	}
	if hits := up.hits.Load(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1: the second request should have been "+
			"served from cache, which only happens when stream coerced to false "+
			"(a stream request is never cached)", hits)
	}
}
