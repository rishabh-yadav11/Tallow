package app

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/config"
)

func TestH6ServerTimeoutsAreAlwaysBounded(t *testing.T) {
	cases := []struct {
		name   string
		params config.Params
	}{
		{

			name:   "zero read timeout",
			params: config.Params{},
		},
		{
			name:   "negative read timeout",
			params: config.Params{ReadTimeout: -1 * time.Second},
		},
		{
			name:   "tiny read timeout",
			params: config.Params{ReadTimeout: time.Millisecond},
		},
		{

			name:   "very long read timeout",
			params: config.Params{ReadTimeout: 10 * time.Minute},
		},
		{
			name:   "typical default",
			params: config.Params{ReadTimeout: 30 * time.Second},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newProxyServer("127.0.0.1:0", http.NotFoundHandler(), tc.params)
			if srv.ReadHeaderTimeout <= 0 {
				t.Fatalf("H6: ReadHeaderTimeout = %s, which net/http reads as NO LIMIT", srv.ReadHeaderTimeout)
			}
			if srv.ReadHeaderTimeout > 10*time.Second {
				t.Fatalf("H6: ReadHeaderTimeout = %s, want at most 10s", srv.ReadHeaderTimeout)
			}
		})
	}
}

func TestH6DefaultsProduceBoundedServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tallow.toml")
	if err := os.WriteFile(path, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, err := cfg.BuildParams()
	if err != nil {
		t.Fatalf("BuildParams: %v", err)
	}
	srv := newProxyServer("127.0.0.1:0", http.NotFoundHandler(), p)
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("the default config left ReadHeaderTimeout unbounded (%s)", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Fatalf("the default config left ReadTimeout unbounded (%s)", srv.ReadTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatalf("the default config left IdleTimeout unbounded (%s); idle connections would accumulate", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes <= 0 {
		t.Fatalf("H6: MaxHeaderBytes is %d, so net/http's 1 MiB default is not stated explicitly; "+
			"an explicit bound documents the intent and survives a future default change", srv.MaxHeaderBytes)
	}
}

func TestH6RequestBodyCapIsWired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tallow.toml")
	body := "version = 1\n\n[server]\nmax_request_bytes = 1234\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p, err := cfg.BuildParams()
	if err != nil {
		t.Fatalf("BuildParams: %v", err)
	}
	if got := p.MaxBodyBytes; got != 1234 {
		t.Fatalf("max_request_bytes did not reach Params: got %d, want 1234", got)
	}
}
