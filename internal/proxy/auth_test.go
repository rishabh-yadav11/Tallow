package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandlerAuth exercises the Handler.auth method directly against the
// configured Auth closure, covering missing, invalid, and valid credentials.
func TestHandlerAuth(t *testing.T) {
	h := &Handler{
		deps: Deps{
			Auth: func(token string) bool { return token == "good" },
		},
	}

	// (a) Request lacking an Authorization header -> 401, auth=false.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if h.auth(rec, req) {
		t.Fatalf("auth: expected false for missing Authorization header")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth: expected 401 for missing header, got %d", rec.Code)
	}

	// (b) Invalid token -> 401, auth=false.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer not-good")
	if h.auth(rec, req) {
		t.Fatalf("auth: expected false for invalid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth: expected 401 for invalid token, got %d", rec.Code)
	}

	// (c) Valid "Bearer good" -> true.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer good")
	if !h.auth(rec, req) {
		t.Fatalf("auth: expected true for valid token")
	}
}
