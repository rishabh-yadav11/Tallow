package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/admin"
	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// startGatewayApp builds the real App (config + keystore + upstream) and returns
// the App plus a proxy httptest.Server wired to it, reusing the integration
// harness conventions from app_test.go. The App is what the admin Backend
// methods (ConfigWrite, ReplayRequest, keys) are exercised through.
func startGatewayApp(t *testing.T, upstreamURL string) (*app.App, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	const masterKey = "management-master-key"
	box, err := secret.NewBox([]byte(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("p1:k1", "sk-fake"); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 8
queue_timeout = "5s"
sticky_ttl = "5s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = true

[cache]
enabled = false

[observability]
enabled = true

[[provider]]
name = "p1"
base_url = "%s"

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  rpm = 100

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
`,
		filepath.Join(dir, "admin.sock"),
		filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"),
		upstreamURL,
	)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, masterKey)

	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = a.AdminSocket() })
	return a, srv
}

// TestConfigWriteReloadAndRefuseInvalid verifies the App-level config mutation:
// PATCH writes are validated, persisted, and hot-reloaded; an invalid change is
// refused, leaving the file untouched.
func TestConfigWriteReloadAndRefuseInvalid(t *testing.T) {
	upSrv, _ := setup(t)
	a, _ := startGatewayApp(t, upSrv.URL)

	// Read a section before mutating it.
	orig, err := a.ConfigRead([]string{"server"})
	if err != nil {
		t.Fatalf("ConfigRead server: %v", err)
	}
	if !strings.Contains(string(orig), "127.0.0.1:0") {
		t.Fatalf("expected configured listen in %s", orig)
	}

	// PATCH the server section to bump listen (a value that still validates).
	body := []byte(`{"listen":"127.0.0.1:9","idle_timeout":"60s"}`)
	if err := a.ConfigWrite("PATCH", []string{"server"}, body); err != nil {
		t.Fatalf("ConfigWrite PATCH server: %v", err)
	}

	// The on-disk file must now contain the change.
	raw, _ := os.ReadFile(a.CfgPath)
	if !strings.Contains(string(raw), "idle_timeout") {
		t.Errorf("config file not updated after PATCH: %s", raw)
	}
	// And the runtime reloaded: read back the live config.
	got, err := a.ConfigRead([]string{"server", "idle_timeout"})
	if err != nil {
		t.Fatalf("ConfigRead after patch: %v", err)
	}
	var s string
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("decode idle_timeout: %v", err)
	}
	if s != "60s" {
		t.Errorf("idle_timeout = %q; want 60s", s)
	}

	// A PUT at field granularity replaces a single field.
	if err := a.ConfigWrite("PUT", []string{"server", "listen"}, []byte(`"127.0.0.1:5"`)); err != nil {
		t.Fatalf("ConfigWrite PUT listen: %v", err)
	}
	got, err = a.ConfigRead([]string{"server", "listen"})
	if err != nil {
		t.Fatalf("ConfigRead listen: %v", err)
	}
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("decode listen: %v", err)
	}
	if s != "127.0.0.1:5" {
		t.Errorf("listen = %q; want 127.0.0.1:5", s)
	}

	// An invalid write (alias targeting an unknown provider) must be refused
	// and must leave the file and runtime untouched.
	before, _ := os.ReadFile(a.CfgPath)
	if err := a.ConfigWrite("PUT", []string{"alias"}, []byte(`[{"name":"bad","target":[{"provider":"does-not-exist","model":"m"}]}]`)); err == nil {
		t.Fatal("expected invalid write (unknown provider in alias) to be refused")
	}
	after, _ := os.ReadFile(a.CfgPath)
	if string(before) != string(after) {
		t.Error("failed invalid write still modified the config file")
	}
}

// TestReplayRequestThroughApp re-runs a stored request through the real proxy
// pipeline and verifies the upstream is hit again and the response shape is the
// original chat-completion shape.
func TestReplayRequestThroughApp(t *testing.T) {
	upSrv, up := setup(t)
	a, gw := startGatewayApp(t, upSrv.URL)

	// Send a real request so the raw body is recorded.
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"replay-me"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("original status %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "pong") {
		t.Fatalf("original response missing content: %s", b)
	}

	// Find the recorded request id (async write; poll until visible).
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ms := a.RecentRequests(20)
		if len(ms) > 0 && ms[0].ID != "" {
			id = ms[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("no recorded request id found")
	}

	// Replay through the pipeline; the upstream must be hit again and the body
	// shape must be the original one.
	hitsBefore := up.hits.Load()
	rec := httptest.NewRecorder()
	if err := a.ReplayRequest(rec, id); err != nil {
		t.Fatalf("ReplayRequest: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("replay status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("replay response missing content: %s", rec.Body.String())
	}
	if up.hits.Load() != hitsBefore+1 {
		t.Errorf("replay did not hit upstream exactly once: hits %d -> %d", hitsBefore, up.hits.Load())
	}

	// Replaying an unknown id surfaces the admin not-found error mapping.
	if err := a.ReplayRequest(httptest.NewRecorder(), "no-such-id"); err == nil {
		t.Error("replaying unknown id returned nil error")
	}
}

// TestAdminBackendMethodsCompile guards the interface contract: *App must
// satisfy admin.Backend, which now includes the write endpoints.
func TestAdminBackendMethodsCompile(t *testing.T) {
	var _ admin.Backend = (*app.App)(nil)
	_ = context.Background()
}
