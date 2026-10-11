package proxy

// P3 audit tests for chatCompletions request validation:
//   - a table of malformed bodies (empty, null, a JSON array, a bare string,
//     missing model) all answer 400 and never panic (item 10);
//   - PUT/DELETE/HEAD are answered 405 before any body is read (item 11).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/cache"
)

func newValidationHarness(t *testing.T) *Handler {
	t.Helper()
	reg := oneAliasRegistry("http://upstream.invalid")
	return newHarness(t, Deps{
		Router:       newRouter(t, reg),
		Registry:     reg,
		Cache:        cache.New(64, time.Minute, 0),
		CacheEnabled: true,
		Observ:       nil,
	})
}

func TestMalformedRequestBodiesAre400(t *testing.T) {
	h := newValidationHarness(t)
	for _, tc := range []struct {
		name, body string
	}{
		{"empty", ""},
		{"null", "null"},
		{"array", "[]"},
		{"bare string", `"x"`},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPost(t, h, "", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("body %q: status %d, want 400 (never a panic)",
					tc.body, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("body %q: Content-Type %q, want application/json",
					tc.body, ct)
			}
		})
	}
}

func TestNonPOSTMethodsAre405(t *testing.T) {
	h := newValidationHarness(t)
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var r *http.Request
			if method == http.MethodHead {
				r = httptest.NewRequest(method, "/v1/chat/completions", nil)
			} else {
				r = httptest.NewRequest(method, "/v1/chat/completions", strings.NewReader("{}"))
			}
			h.Routes().ServeHTTP(rec, r)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s: status %d, want 405", method, rec.Code)
			}
		})
	}
}
