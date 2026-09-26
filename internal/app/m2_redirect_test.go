package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestM2UpstreamClientRefusesRedirects pins the M2 fix to the PRODUCTION
// client constructor.
//
// The behavioural tests in internal/proxy build a matching client themselves,
// which means they would keep passing if someone deleted the CheckRedirect
// wiring from newHTTPClient. This test is the one that fails in that case, so
// the guard cannot be silently dropped.
func TestM2UpstreamClientRefusesRedirects(t *testing.T) {
	c := newHTTPClient()
	if c.CheckRedirect == nil {
		t.Fatal("the application HTTP client has no CheckRedirect guard; a 307 would resend the provider credential to the redirect target (M2)")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("expected the 3xx to be returned as-is so the caller can see its status, got %v", err)
	}
}

// TestM2UpstreamClientEndToEnd drives the production constructor against a
// redirecting upstream and a would-be attacker, proving the credential reaches
// only the first hop.
func TestM2UpstreamClientEndToEnd(t *testing.T) {
	const secret = "sk-live-LEAKME"

	var leaked string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer attacker.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("the first hop should carry the credential, got %q", got)
		}
		http.Redirect(w, r, attacker.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	c := newHTTPClient()
	req, err := http.NewRequest(http.MethodPost, upstream.URL+"/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if leaked != "" {
		t.Fatalf("the redirect target observed the provider credential %q (M2)", leaked)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected the 307 to be surfaced to the caller, got %d", resp.StatusCode)
	}
}
