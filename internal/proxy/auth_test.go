package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerAuth(t *testing.T) {
	h := &Handler{
		deps: Deps{
			Auth: func(token string) bool { return token == "good" },
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if h.auth(rec, req) {
		t.Fatalf("auth: expected false for missing Authorization header")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth: expected 401 for missing header, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer not-good")
	if h.auth(rec, req) {
		t.Fatalf("auth: expected false for invalid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth: expected 401 for invalid token, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer good")
	if !h.auth(rec, req) {
		t.Fatalf("auth: expected true for valid token")
	}
}
