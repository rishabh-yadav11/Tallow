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
		"id":     "x",
		"object": "response",
		"model":  req["model"],
		"output": []any{map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "model=" + fmt.Sprint(req["model"])}},
		}},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
}

func setupEcho(t *testing.T) *echoUpstream {
	t.Helper()
	e := &echoUpstream{}
	e.srv = httptest.NewServer(http.HandlerFunc(e.handler))
	t.Cleanup(e.srv.Close)
	return e
}

func startGatewayCfg(t *testing.T, cfg string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	const masterKey = "e2e-master-key"
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

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

// contentOf extracts the assistant text from a Responses output block.
func contentOf(t *testing.T, body string) string {
	t.Helper()
	var v struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("parse response: %v (body=%s)", err, body)
	}
	for _, o := range v.Output {
		if o.Type != "message" {
			continue
		}
		for _, c := range o.Content {
			if c.Type == "output_text" && c.Text != "" {
				return c.Text
			}
		}
	}
	return ""
}

func TestE2ECacheMultiModelAliasNoCollision(t *testing.T) {
	up := setupEcho(t)

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

	dir := t.TempDir()
	box, _ := secret.NewBox([]byte("e2e-master-key"))
	ks, _ := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	_ = ks.Set("ka", "sk-1")
	_ = ks.Set("kb", "sk-2")

	gw := startGatewayCfg(t, cfg)

	body := `{"model":"mux","input":"hi"}`
	for i := 0; i < 3; i++ {
		resp, got := post(t, gw.URL+"/v1/responses", body)
		if resp.StatusCode != 200 {
			t.Fatalf("iter %d status %d: %s", i, resp.StatusCode, got)
		}
		c := contentOf(t, got)
		if c != "model=model-alpha" && c != "model=model-beta" {
			t.Fatalf("iter %d: unexpected content %q, want a routed model", i, c)
		}
	}

	if up.hits.Load() != 3 {
		t.Fatalf("expected 3 upstream hits for a multi-model alias (cache fast path disabled), got %d", up.hits.Load())
	}
}

type gatewayReload struct {
	srv     *httptest.Server
	app     *app.App
	cfgPath string
}

func (g *gatewayReload) reload(t *testing.T, cfg string) {
	t.Helper()
	if err := os.WriteFile(g.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.app.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

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

func TestE2EHotReloadAppliesNewRPMLimit(t *testing.T) {
	upSrv, _ := setup(t)
	g := startReloadGateway(t, upSrv.URL)

	body := `{"model":"flash","input":"hi"}`

	if resp, got := post(t, g.srv.URL+"/v1/responses", body); resp.StatusCode != 200 {
		t.Fatalf("req1 status %d: %s", resp.StatusCode, got)
	}
	if resp, got := post(t, g.srv.URL+"/v1/responses", body); resp.StatusCode != 200 {
		t.Fatalf("req2 status %d: %s", resp.StatusCode, got)
	}

	g.reload(t, cfgWithRPM(g.cfgPath, upSrv.URL, 1))
	if resp, got := post(t, g.srv.URL+"/v1/responses", body); resp.StatusCode != 200 {
		t.Fatalf("req3 (post-reload) status %d: %s", resp.StatusCode, got)
	}
	if resp, _ := post(t, g.srv.URL+"/v1/responses", body); resp.StatusCode == 200 {
		t.Fatal("req4 should be rejected: post-reload rpm=1 must be enforced, was 200")
	}
}

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

	send := func(label string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses",
			strings.NewReader(`{"model":"flash","input":"hi"}`))
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
	body := `{"model":"flash","input":[{"type":"function_call_output","call_id":"t1","output":"` + big + `"}]}`
	resp, got := post(t, srv.URL+"/v1/responses", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}

	fwd, ok := up.lastBody()
	if !ok {
		t.Fatal("upstream did not receive the request")
	}
	items := fwd["input"].([]any)
	toolItem := items[0].(map[string]any)
	c := toolItem["output"].(string)
	if len(c) > 100 {
		t.Fatalf("upstream tool output length %d exceeds max_tool_output_chars 100", len(c))
	}
	if c == big {
		t.Fatalf("upstream tool output was not truncated at all: %d bytes", len(c))
	}
}

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
