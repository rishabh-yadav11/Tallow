package proxy

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/config"
)

// TestProxyTargetGuardUnderProxy pins the SSRF-under-a-proxy gap: when an HTTP
// proxy is configured, DialControl only sees the proxy's address, so the provider
// host the proxy CONNECTs to must be judged separately. A private target is
// refused with the same client-safe "connection failed" category and the detail
// kept for the log; a public target passes; and with no proxy configured nothing
// extra is resolved (the dialer guard is authoritative on the direct path).
func TestProxyTargetGuardUnderProxy(t *testing.T) {
	proxyURL, err := url.Parse("http://proxy.invalid:3128")
	if err != nil {
		t.Fatal(err)
	}
	proxyUp := func() *http.Client {
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	}

	t.Run("private-resolving target under proxy is refused", func(t *testing.T) {
		// localhost resolves to loopback space, which is not public.
		req, _ := http.NewRequest(http.MethodPost, "https://localhost/chat/completions", nil)
		ferr := proxyTargetGuard(proxyUp(), req)
		if ferr == nil {
			t.Fatal("private target through a proxy was permitted; the proxy must not bypass M14")
		}
		if ferr.msg != "connection failed" {
			t.Errorf("client-safe category = %q, want %q", ferr.msg, "connection failed")
		}
		if !strings.Contains(ferr.private, "ssrf guard: refusing") {
			t.Errorf("the log detail should carry the SSRF refusal, got %q", ferr.private)
		}
	})

	t.Run("public target under proxy passes", func(t *testing.T) {
		// A literal public address resolves without DNS.
		req, _ := http.NewRequest(http.MethodPost, "https://93.184.216.34/chat/completions", nil)
		if ferr := proxyTargetGuard(proxyUp(), req); ferr != nil {
			t.Fatalf("public target through a proxy was refused: %v", ferr)
		}
	})

	t.Run("proxy off does not resolve", func(t *testing.T) {
		direct := &http.Client{Transport: &http.Transport{}} // no Proxy func
		// A private target is allowed to pass the guard un-judged, proving the
		// direct path does no proxy-target resolution (DialControl owns it).
		req, _ := http.NewRequest(http.MethodPost, "https://localhost/chat/completions", nil)
		if ferr := proxyTargetGuard(direct, req); ferr != nil {
			t.Fatalf("direct path must not run the proxy-target guard, got %v", ferr)
		}
	})
}

// TestCheckTargetPublicIsExportedAndReturnsPrivateAddrError proves the exported
// helper surfaces the same typed refusal the dialer path does, so proxy callers
// can recognise the policy through errors.As.
func TestCheckTargetPublicIsExportedAndReturnsPrivateAddrError(t *testing.T) {
	err := config.CheckTargetPublic("localhost")
	if err == nil {
		t.Fatal("CheckTargetPublic(localhost) = nil; a private target must be refused")
	}
	var pae *config.PrivateAddrError
	if !errorsAs(err, &pae) {
		t.Fatalf("expected *PrivateAddrError, got %T: %v", err, err)
	}
	if err := config.CheckTargetPublic("93.184.216.34"); err != nil {
		t.Fatalf("CheckTargetPublic(public IP) = %v, want nil", err)
	}
}

// errorsAs is errors.As specialised so the test reads cleanly.
func errorsAs(err error, target **config.PrivateAddrError) bool {
	for err != nil {
		if e, ok := err.(*config.PrivateAddrError); ok {
			*target = e
			return true
		}
		return false
	}
	return false
}
