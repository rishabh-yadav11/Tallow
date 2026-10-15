package proxy

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/config"
)

func TestProxyTargetGuardUnderProxy(t *testing.T) {
	proxyURL, err := url.Parse("http://proxy.invalid:3128")
	if err != nil {
		t.Fatal(err)
	}
	proxyUp := func() *http.Client {
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	}

	t.Run("private-resolving target under proxy is refused", func(t *testing.T) {

		req, _ := http.NewRequest(http.MethodPost, "https://localhost/responses", nil)
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

		req, _ := http.NewRequest(http.MethodPost, "https://93.184.216.34/responses", nil)
		if ferr := proxyTargetGuard(proxyUp(), req); ferr != nil {
			t.Fatalf("public target through a proxy was refused: %v", ferr)
		}
	})

	t.Run("proxy off does not resolve", func(t *testing.T) {
		direct := &http.Client{Transport: &http.Transport{}}

		req, _ := http.NewRequest(http.MethodPost, "https://localhost/responses", nil)
		if ferr := proxyTargetGuard(direct, req); ferr != nil {
			t.Fatalf("direct path must not run the proxy-target guard, got %v", ferr)
		}
	})
}

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
