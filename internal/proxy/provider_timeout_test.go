package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/registry"
)

func providerWithTimeoutRegistry(baseURL string, timeout time.Duration) *registry.Registry {
	return registry.New(
		[]model.Provider{{
			Name:    "p1",
			BaseURL: baseURL,
			Timeout: timeout,
			Keys:    []model.Key{{ID: "k1", Secret: "sk-test", RPM: 10000}},
		}},
		[]model.Alias{{
			Name:    "flash",
			Targets: []model.Target{{Provider: "p1", Model: "flash-v4"}},
		}},
	)
}

func TestStreamProviderTimeoutDoesNotCancelBeforeBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\"}\n"))
		fl.Flush()
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"id\":\"2\"}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
		fl.Flush()
	}))
	defer up.Close()

	reg := providerWithTimeoutRegistry(up.URL, 5*time.Second)
	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Observ:   observ.New(true),
	})
	body := `{"model":"flash","stream":true,"input":"hi"}`
	rec := doPost(t, h, "", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("streaming response code = %d, want 200", rec.Code)
	}
	got := rec.Body.String()
	if !strings.Contains(got, `data: {"id":"1"}`) || !strings.Contains(got, `data: {"id":"2"}`) {
		t.Fatalf("client did not receive both data lines; body %q. The provider "+
			"timeout must stay armed across the whole stream, not cancel at send "+
			"return. Got only: %q", got, "")
	}
	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("stream body %q missing [DONE]; the body read was cut short", got)
	}
}

func TestEndlessNonDataStreamTerminatesByProviderTimeout(t *testing.T) {
	const providerTimeout = 300 * time.Millisecond
	done := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(": keep-alive\n"))
				fl.Flush()
			}
		}
	}))
	defer func() { close(done); up.Close() }()

	reg := providerWithTimeoutRegistry(up.URL, providerTimeout)
	h := newHarness(t, Deps{
		Router:   newRouter(t, reg),
		Registry: reg,
		Observ:   observ.New(true),
	})
	body := `{"model":"flash","stream":true,"input":"hi"}`

	const bound = 2 * time.Second
	start := time.Now()
	rec := doPost(t, h, "", body)
	elapsed := time.Since(start)

	if elapsed >= bound {
		t.Fatalf("keep-alive-only stream took %s to terminate, want < %s: the "+
			"provider timeout did not bound the slow drip", elapsed, bound)
	}
	got := rec.Body.String()
	if strings.Contains(got, `data:`) {
		t.Fatalf("expected no data line from a keep-alive-only upstream, got %q", got)
	}
}
