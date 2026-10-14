package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/routing"
)

// TestStreamKeepaliveEmitsAfterFirstToken drives a real streaming chat
// completion through the handler against a fake SSE upstream whose data lines
// are separated by gaps larger than the keepalive interval, and asserts that
// `: keepalive` comments appear on the wire only after the first data line.
func TestStreamKeepaliveEmitsAfterFirstToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// First data line immediately (necessary to start the heartbeat).
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		f.Flush()
		// Then a gap far longer than the interval so the heartbeat must fire.
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" bye\"}}]}\n\n")
		f.Flush()
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	reg := registry.New(
		[]model.Provider{{Name: "p1", BaseURL: upstream.URL, Keys: []model.Key{{ID: "k1", Secret: "sk-test"}}}},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}},
	)
	h := newHarness(t, Deps{
		Router:          newRouter(t, reg),
		Registry:        reg,
		StreamKeepalive: 40 * time.Millisecond,
	})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(b)
	if n := strings.Count(got, ": keepalive"); n == 0 {
		t.Fatalf("expected keepalive comments in stream, got:\n%s", got)
	}
	// The first data line must still be present before a keepalive may appear.
	firstData := strings.Index(got, "data:")
	firstKeepalive := strings.Index(got, ": keepalive")
	if firstData < 0 || firstKeepalive >= 0 && firstKeepalive < firstData {
		t.Fatalf("keepalive emitted before the first data line:\n%s", got)
	}
}

// TestStreamKeepaliveDisabledWhenIntervalZero pins the default: with the
// interval left unset there must be no keepalive comments in the stream.
func TestStreamKeepaliveDisabledWhenIntervalZero(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		f.Flush()
		time.Sleep(60 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	reg := registry.New(
		[]model.Provider{{Name: "p1", BaseURL: upstream.URL, Keys: []model.Key{{ID: "k1", Secret: "sk-test"}}}},
		[]model.Alias{{Name: "flash", Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}}}},
	)
	h := newHarness(t, Deps{Router: newRouter(t, reg), Registry: reg})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if n := strings.Count(string(b), ": keepalive"); n != 0 {
		t.Fatalf("keepalive emitted with interval zero: %s", b)
	}
}

// TestApplySelectedHeaders checks literal values, `$`-dynamic copies, and that
// a missing downstream header is omitted rather than sent empty.
func TestApplySelectedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	down := http.Header{}
	down.Set("X-Tenant", "acme")
	applySelectedHeaders(req, down, map[string]string{
		"X-Static":  "fixed",
		"X-Tenant":  "$X-Tenant",
		"X-Missing": "$X-Nope",
	})
	if got := req.Header.Get("X-Static"); got != "fixed" {
		t.Fatalf("X-Static = %q, want fixed", got)
	}
	if got := req.Header.Get("X-Tenant"); got != "acme" {
		t.Fatalf("X-Tenant = %q, want acme", got)
	}
	if _, ok := req.Header["X-Missing"]; ok {
		t.Fatalf("X-Missing should be omitted when the downstream header is absent")
	}
}

// TestClientForProxySelection verifies the memoized per-selection proxy clients:
// the shared client is returned as-is for an empty override, "direct" yields a
// proxy-less transport, and a URL yields a transport with a configured proxy.
func TestClientForProxySelection(t *testing.T) {
	h := newHarness(t, Deps{Client: &http.Client{Transport: &http.Transport{}}})

	if c := h.clientFor(&routing.Selection{}); c != h.deps.Client {
		t.Fatalf("empty ProxyURL should return the shared client")
	}
	memoized := func(p string) *http.Client {
		t.Helper()
		return h.clientFor(&routing.Selection{ProxyURL: p})
	}

	direct := memoized("direct")
	if tr, ok := direct.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatalf("direct proxy should yield a nil Proxy, got %#v", direct.Transport)
	}
	proxied := memoized("http://127.0.0.1:9999")
	tr, ok := proxied.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("proxy URL should yield a configured Proxy, got %#v", proxied.Transport)
	}
	// Same override is memoized (same pointer, no fresh transport).
	if memoized("http://127.0.0.1:9999") != proxied {
		t.Fatalf("same ProxyURL should reuse the memoized client")
	}
}
