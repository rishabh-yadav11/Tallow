package app_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// echoUpstream echoes the requested model and a fixed prompt count in the
// non-streaming response body, so a test can detect whether a response came
// from the upstream or from an (possibly wrong-model) cache entry.
type echoUpstream struct {
	hits atomic.Int32
	srv  *httptest.Server
}

func (e *echoUpstream) handler(w http.ResponseWriter, r *http.Request) {
	e.hits.Add(1)
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "x",
		"object":  "chat.completion",
		"model":   req["model"],
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "model=" + fmt.Sprint(req["model"])}}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

func setupEcho(t *testing.T) *echoUpstream {
	t.Helper()
	e := &echoUpstream{}
	e.srv = httptest.NewServer(http.HandlerFunc(e.handler))
	t.Cleanup(e.srv.Close)
	return e
}

// startGatewayCfg builds and boots a gateway from a fully-specified config body.
func startGatewayCfg(t *testing.T, cfg string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	const masterKey = "e2e-master-key"
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

	// Substitute %ADMIN%, %DB%, %KS% placeholders for temp paths.
	cfg = strings.ReplaceAll(cfg, "%ADMIN%", filepath.Join(dir, "admin.sock"))
	cfg = strings.ReplaceAll(cfg, "%DB%", filepath.Join(dir, "tallow.db"))
	cfg = strings.ReplaceAll(cfg, "%KS%", filepath.Join(dir, "keys.json"))

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)
	return srv
}

// contentOf extracts the assistant message content from a chat completion JSON.
func contentOf(t *testing.T, body string) string {
	t.Helper()
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("parse response: %v (body=%s)", err, body)
	}
	if len(v.Choices) == 0 {
		return ""
	}
	return v.Choices[0].Message.Content
}

// TestE2ECacheMultiModelAliasNoCollision verifies the fix: an alias whose
// targets resolve to distinct upstream models must NOT be served from a
// cross-model cache entry. Repeated identical requests all reach the upstream
// (the exact-match cache fast path is disabled for multi-model aliases), and
// each response reflects the routed model.
func TestE2ECacheMultiModelAliasNoCollision(t *testing.T) {
	up := setupEcho(t)
	// One alias "mux" with two targets on distinct providers (same upstream)
	// carrying different models. No key-level RPM caps, so both are healthy.
	cfg := fmt.Sprintf(`version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%%ADMIN%%"
max_concurrent = 8
queue_timeout = "5s"
sticky_ttl = "60s"

[secret]
keystore = "%%KS%%"

[store]
path = "%%DB%%"
raw_bodies = false

[cache]
enabled = true
ttl = "1m"

[observability]
enabled = false

[[provider]]
name = "pa"
base_url = "%s"
  [[provider.key]]
  id = "ka"
  ref = "ka"

[[provider]]
name = "pb"
base_url = "%s"
  [[provider.key]]
  id = "kb"
  ref = "kb"

[[alias]]
name = "mux"
  [[alias.target]]
  provider = "pa"
  model = "model-alpha"
  [[alias.target]]
  provider = "pb"
  model = "model-beta"
`, up.srv.URL, up.srv.URL)

	// Seed the keystore with the two key refs.
	dir := t.TempDir()
	box, _ := secret.NewBox([]byte("e2e-master-key"))
	ks, _ := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	_ = ks.Set("ka", "sk-1")
	_ = ks.Set("kb", "sk-2")

	gw := startGatewayCfg(t, cfg)

	body := `{"model":"mux","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 3; i++ {
		resp, got := post(t, gw.URL+"/v1/chat/completions", body)
		if resp.StatusCode != 200 {
			t.Fatalf("iter %d status %d: %s", i, resp.StatusCode, got)
		}
		c := contentOf(t, got)
		if c != "model=model-alpha" && c != "model=model-beta" {
			t.Fatalf("iter %d: unexpected content %q, want a routed model", i, c)
		}
	}
	// Because "mux" is a multi-model alias, the cache fast path is disabled:
	// every identical request must reach the upstream (no cross-model serve).
	if up.hits.Load() != 3 {
		t.Fatalf("expected 3 upstream hits for a multi-model alias (cache fast path disabled), got %d", up.hits.Load())
	}
}

// gatewayReload is a gateway handle that lets a test rewrite the config file and
// trigger a hot reload, to exercise reload-applied limit changes end to end.
type gatewayReload struct {
	srv     *httptest.Server
	app     *app.App
	cfgPath string
}

// reload rewrites cfgPath and calls App.Reload.
func (g *gatewayReload) reload(t *testing.T, cfg string) {
	t.Helper()
	if err := os.WriteFile(g.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.app.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

// startReloadGateway boots an App and returns a reloadable handle. baseURL is
// substituted as %UPSTREAM% in the initial config.
func startReloadGateway(t *testing.T, upstreamURL string) *gatewayReload {
	t.Helper()
	dir := t.TempDir()
	const masterKey = "e2e-master-key"
	box, _ := secret.NewBox([]byte(masterKey))
	ks, _ := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	_ = ks.Set("p1:k1", "sk-fake")
	cfgPath := filepath.Join(dir, "config.toml")

	mkCfg := func(rpm int) string {
		return fmt.Sprintf(`version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 8
queue_timeout = "5s"
sticky_ttl = "30s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = false

# Cache must be off: identical repeat bodies would be served from cache
# before routing, bypassing the budget enforcement under test.
[cache]
enabled = false

[observability]
enabled = false

[[provider]]
name = "p1"
base_url = "%s"

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  rpm = %d

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
`, filepath.Join(dir, "admin.sock"), filepath.Join(dir, "keys.json"),
			filepath.Join(dir, "tallow.db"), upstreamURL, rpm)
	}

	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })
	// Start with rpm=0 (unlimited). A zero limit consumes no window count, so
	// the warmup requests leave the key's live window at zero and the reload
	// below cleanly installs the tightened limit from a fresh slate.
	if err := os.WriteFile(cfgPath, []byte(mkCfg(0)), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)
	return &gatewayReload{srv: srv, app: a, cfgPath: cfgPath}
}

// TestE2EHotReloadAppliesNewRPMLimit verifies the SetBudgets fix: after a hot
// reload tightens a key's RPM, the newly-loaded limit is enforced on subsequent
// requests (previously reloaded limit changes were silently ignored).
func TestE2EHotReloadAppliesNewRPMLimit(t *testing.T) {
	upSrv, _ := setup(t)
	g := startReloadGateway(t, upSrv.URL)

	body := `{"model":"flash","messages":[{"role":"user","content":"hi"}]}`
	// Two requests succeed under rpm=0 (unlimited, no window consumption).
	if resp, got := post(t, g.srv.URL+"/v1/chat/completions", body); resp.StatusCode != 200 {
		t.Fatalf("req1 status %d: %s", resp.StatusCode, got)
	}
	if resp, got := post(t, g.srv.URL+"/v1/chat/completions", body); resp.StatusCode != 200 {
		t.Fatalf("req2 status %d: %s", resp.StatusCode, got)
	}

	// Reload tightening the key to rpm=1: live windows survive SetBudgets, so
	// the third request consumes the single RPM slot and the fourth must be
	// rejected (previously reloaded limit changes were silently ignored, so
	// the fourth request would have succeeded).
	g.reload(t, cfgWithRPM(g.cfgPath, upSrv.URL, 1))
	if resp, got := post(t, g.srv.URL+"/v1/chat/completions", body); resp.StatusCode != 200 {
		t.Fatalf("req3 (post-reload) status %d: %s", resp.StatusCode, got)
	}
	if resp, _ := post(t, g.srv.URL+"/v1/chat/completions", body); resp.StatusCode == 200 {
		t.Fatal("req4 should be rejected: post-reload rpm=1 must be enforced, was 200")
	}
}

// TestE2EStickyProviderRPMEnforced verifies the acquire fix: a session pinned
// via sticky affinity still respects the aggregate provider-level RPM cap. With
// a provider cap of 1, the first request succeeds and installs the pin; the
// second request on the same session must be rejected because the provider cap
// is exhausted (previously the sticky path bypassed the provider cap entirely).
func TestE2EStickyProviderRPMEnforced(t *testing.T) {
	upSrv, _ := setup(t)
	dir := t.TempDir()
	const masterKey = "e2e-master-key"
	box, _ := secret.NewBox([]byte(masterKey))
	ks, _ := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	_ = ks.Set("p1:k1", "sk-fake")
	_ = ks.Set("p1:k2", "sk-fake")

	cfg := fmt.Sprintf(`version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 8
queue_timeout = "5s"
sticky_ttl = "60s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = false

# Cache off so repeat session requests reach routing and the provider cap.
[cache]
enabled = false

[observability]
enabled = false

[[provider]]
name = "p1"
base_url = "%s"
rpm = 1

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  [[provider.key]]
  id = "k2"
  ref = "p1:k2"

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
`, filepath.Join(dir, "admin.sock"), filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"), upSrv.URL)
	cfgPath := filepath.Join(dir, "config.toml")
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)

	// Same session on both requests so the second is sticky-pinned.
	send := func(label string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Session-Id", "sticky-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	if resp := send("req1"); resp.StatusCode != 200 {
		t.Fatalf("req1 status %d, want 200 (consumes the single provider RPM slot)", resp.StatusCode)
	}
	if resp := send("req2"); resp.StatusCode == 200 {
		t.Fatal("req2 should be rejected: provider-level RPM cap=1 must hold on the sticky path, got 200")
	}
}

// TestE2ECompactionBoundsToolOutput verifies the compact fix: a tool message
// whose output exceeds MaxToolOutputChars is truncated (including the marker)
// so the forwarded body stays at or under the configured bound.
func TestE2ECompactionBoundsToolOutput(t *testing.T) {
	upSrv, up := setup(t)
	dir := t.TempDir()
	const masterKey = "e2e-master-key"
	box, _ := secret.NewBox([]byte(masterKey))
	ks, _ := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	_ = ks.Set("p1:k1", "sk-fake")

	cfg := fmt.Sprintf(`version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 8
queue_timeout = "5s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = false

[compaction]
enabled = true
max_tool_output_chars = 100

[observability]
enabled = false

[[provider]]
name = "p1"
base_url = "%s"

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
`, filepath.Join(dir, "admin.sock"), filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"), upSrv.URL)
	cfgPath := filepath.Join(dir, "config.toml")
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)

	big := strings.Repeat("x", 5000)
	body := `{"model":"flash","messages":[{"role":"tool","tool_call_id":"t1","content":"` + big + `"}]}`
	resp, got := post(t, srv.URL+"/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	// Inspect what the upstream actually received; it must be <= 100 chars.
	fwd, ok := up.lastBody()
	if !ok {
		t.Fatal("upstream did not receive the request")
	}
	msgs := fwd["messages"].([]any)
	toolMsg := msgs[0].(map[string]any)
	c := toolMsg["content"].(string)
	if len(c) > 100 {
		t.Fatalf("upstream tool content length %d exceeds max_tool_output_chars 100", len(c))
	}
}

// cfgWithRPM rewrites g.cfgPath with the given key RPM and returns the new text.
func cfgWithRPM(cfgPath, upstreamURL string, rpm int) string {
	dir := filepath.Dir(cfgPath)
	return fmt.Sprintf(`version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 8
queue_timeout = "5s"
sticky_ttl = "30s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = false

# Cache must be off: identical repeat bodies would be served from cache
# before routing, bypassing the budget enforcement under test.
[cache]
enabled = false

[observability]
enabled = false

[[provider]]
name = "p1"
base_url = "%s"

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  rpm = %d

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
`, filepath.Join(dir, "admin.sock"), filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"), upstreamURL, rpm)
}
