package app_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// fakeUpstream is an OpenAI-compatible stub that records requests.
type fakeUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
	hits   atomic.Int32
	srv    *httptest.Server
}

func (f *fakeUpstream) handler(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.bodies = append(f.bodies, req)
	f.mu.Unlock()

	if s, _ := req["stream"].(bool); s {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     "x",
		"object": "chat.completion",
		"model":  req["model"],
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3},
	})
}

func (f *fakeUpstream) lastBody() (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil, false
	}
	return f.bodies[len(f.bodies)-1], true
}

func setup(t *testing.T) (*httptest.Server, *fakeUpstream) {
	t.Helper()
	up := &fakeUpstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(up.srv.Close)
	return up.srv, up
}

func startGateway(t *testing.T, upstreamURL string) *httptest.Server {
	dir := t.TempDir()
	const masterKey = "integration-master-key"
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
	if err := ks.Set("pdead:kdead", "sk-dead"); err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`version = 1
[auth]
open = true   # integration harness: no auth exercise, accept any token.

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

[retention]
raw_bodies_days = 5
metadata_days = 30
errors_days = 90

[observability]
enabled = true

[[provider]]
name = "pdead"
base_url = "http://127.0.0.1:1"

  [[provider.key]]
  id = "kdead"
  ref = "pdead:kdead"

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
  supports_tools = true

[[alias]]
name = "failover"
  [[alias.target]]
  provider = "pdead"
  model = "m"
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
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func streaming(t *testing.T, url, body string) string {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		out.WriteString(sc.Text())
	}
	return out.String()
}

func TestEndToEndRoutingAndCache(t *testing.T) {
	upSrv, up := setup(t)
	gw := startGateway(t, upSrv.URL)

	nb := `{"model":"flash","messages":[{"role":"user","content":"hi"}]}`
	resp, got := post(t, gw.URL+"/v1/chat/completions", nb)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	if !strings.Contains(got, "pong") {
		t.Fatalf("missing content: %s", got)
	}
	// The upstream must have received the mapped model, not the alias.
	last, _ := up.lastBody()
	if last["model"] != "flash-v4" {
		t.Fatalf("upstream model = %v, want flash-v4", last["model"])
	}
	hitsAfterFirst := up.hits.Load()

	// Identical request must be served from the exact-match cache.
	_, got2 := post(t, gw.URL+"/v1/chat/completions", nb)
	if !strings.Contains(got2, "pong") {
		t.Fatalf("cached response missing content: %s", got2)
	}
	if up.hits.Load() != hitsAfterFirst {
		t.Fatalf("cache miss: upstream hit count went from %d to %d", hitsAfterFirst, up.hits.Load())
	}
}

func TestEndToEndStreaming(t *testing.T) {
	upSrv, _ := setup(t)
	gw := startGateway(t, upSrv.URL)

	body := `{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	got := streaming(t, gw.URL+"/v1/chat/completions", body)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("stream missing tokens: %q", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("stream missing [DONE]: %q", got)
	}
}

func TestEndToEndFailoverBeforeFirstToken(t *testing.T) {
	upSrv, up := setup(t)
	gw := startGateway(t, upSrv.URL)

	// First target is a dead provider; the request must transparently retry.
	before := up.hits.Load()
	resp, got := post(t, gw.URL+"/v1/chat/completions",
		`{"model":"failover","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("failover status %d: %s", resp.StatusCode, got)
	}
	if !strings.Contains(got, "pong") {
		t.Fatalf("failover missing content: %s", got)
	}
	if up.hits.Load() != before+1 {
		t.Fatalf("expected exactly one upstream hit after failover, hit=%d", up.hits.Load())
	}
}

func TestEndToEndToolPassthrough(t *testing.T) {
	upSrv, up := setup(t)
	gw := startGateway(t, upSrv.URL)

	body := `{"model":"flash","messages":[{"role":"user","content":"use the tool"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}}]}`
	resp, _ := post(t, gw.URL+"/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	last, _ := up.lastBody()
	if _, ok := last["tools"]; !ok {
		t.Fatal("tools not forwarded to upstream")
	}
}

// startGatewayAuth builds a gateway with an explicit auth policy. When open is
// true the gateway accepts any token; when false (the safe default) it requires a
// configured api key. apiKeys, if non-empty, whitelists those tokens.
func startGatewayAuth(t *testing.T, upstreamURL string, open bool, apiKeys []string) *httptest.Server {
	dir := t.TempDir()
	const masterKey = "integration-master-key"
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

	authBlock := "[auth]\nopen = " + boolStr(open) + "\n"
	if len(apiKeys) > 0 {
		authBlock += "api_keys = ["
		for i, k := range apiKeys {
			if i > 0 {
				authBlock += ", "
			}
			authBlock += "\"" + k + "\""
		}
		authBlock += "]\n"
	}

	cfg := fmt.Sprintf(`version = 1
%s[server]
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
  supports_tools = true
`,
		authBlock,
		filepath.Join(dir, "admin.sock"),
		filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"),
		upstreamURL,
	)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)
	return srv
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestAuthClosedRejectsRequests verifies the user-facing fix for issue #6:
// an empty api_keys list with open=false must NOT open the gateway; requests
// without a valid key get 401.
func TestAuthClosedRejectsRequests(t *testing.T) {
	upSrv, _ := setup(t)
	gw := startGatewayAuth(t, upSrv.URL, false, nil)

	resp, _ := post(t, gw.URL+"/v1/chat/completions",
		`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for closed gateway with no token, got %d", resp.StatusCode)
	}
}

// TestAuthOpenAcceptsAnyToken verifies open=true restores the loopback-trust
// behavior explicitly opted into (e.g. by the integration harness).
func TestAuthOpenAcceptsAnyToken(t *testing.T) {
	upSrv, _ := setup(t)
	gw := startGatewayAuth(t, upSrv.URL, true, nil)

	resp, _ := post(t, gw.URL+"/v1/chat/completions",
		`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for open gateway, got %d", resp.StatusCode)
	}
}

// TestAuthKeyAllowList verifies a configured api_keys allow-list is honored.
func TestAuthKeyAllowList(t *testing.T) {
	upSrv, _ := setup(t)
	gw := startGatewayAuth(t, upSrv.URL, false, []string{"secret-key"})

	// Wrong key -> 401.
	resp, _ := postAuth(t, gw.URL+"/v1/chat/completions", "wrong",
		`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong key, got %d", resp.StatusCode)
	}
	// Correct key -> 200.
	resp, body := postAuth(t, gw.URL+"/v1/chat/completions", "secret-key",
		`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for correct key, got %d: %s", resp.StatusCode, body)
	}
}

func postAuth(t *testing.T, url, token, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postAuth: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestHotReloadUnderLoad exercises the issue #1 race: concurrent config hot-reload
// (which rewrites Router.budgets / observ.enabled under lock) against live
// requests. Under -race this fails before the fix; it must pass after.
func TestHotReloadUnderLoad(t *testing.T) {
	upSrv, _ := setup(t)
	dir := t.TempDir()
	const masterKey = "integration-master-key"
	box, _ := secret.NewBox([]byte(masterKey))
	ks, _ := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	_ = ks.Set("p1:k1", "sk-fake")
	cfgPath := filepath.Join(dir, "config.toml")
	writeCfg := func(open bool) {
		cfg := fmt.Sprintf(`version = 1
[auth]
open = %s

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 8

[secret]
keystore = "%s"

[store]
path = "%s"

[observability]
enabled = true

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
`,
			boolStr(open), filepath.Join(dir, "admin.sock"),
			filepath.Join(dir, "keys.json"), filepath.Join(dir, "tallow.db"), upSrv.URL)
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCfg(true)
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)

	const workers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					resp, _ := post(t, srv.URL+"/v1/chat/completions",
						`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`)
					if resp != nil {
						resp.Body.Close()
					}
				}
			}
		}()
	}
	// Hammer reloads concurrently with requests.
	for i := 0; i < 200; i++ {
		writeCfg(i%2 == 0)
		if err := a.Reload(); err != nil {
			t.Errorf("reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

// freePort reserves and releases a loopback TCP port, returning its number. There
// is a small TOCTOU window before the caller binds it, which is acceptable for a
// test; callers retry if bind fails.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// TestAppRunEndToEnd boots the actual gateway via App.Run (proxy ListenAndServe,
// admin unix socket, health loop, retention loop, config watcher) against a fake
// upstream, serves a real chat request, then cancels the context to verify
// graceful shutdown returns nil. This exercises the run path that the in-process
// httptest.NewServer(a.ProxyHandler()) harness never touches.
func TestAppRunEndToEnd(t *testing.T) {
	upSrv, up := setup(t)
	dir := t.TempDir()
	const masterKey = "integration-master-key"
	box, _ := secret.NewBox([]byte(masterKey))
	ks, _ := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	_ = ks.Set("p1:k1", "sk-fake")

	var baseURL string
	for attempt := 0; attempt < 5; attempt++ {
		port := freePort(t)
		cfg := fmt.Sprintf(`version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:%d"
admin_socket = "%s"
max_concurrent = 8
queue_timeout = "5s"
sticky_ttl = "5s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = true

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
  supports_tools = true
`,
			port, filepath.Join(dir, "admin.sock"),
			filepath.Join(dir, "keys.json"), filepath.Join(dir, "tallow.db"), upSrv.URL)
		cfgPath := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Setenv(secret.EnvMasterKey, masterKey)
		t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

		a, err := app.New(cfgPath, "test")
		if err != nil {
			t.Fatalf("app.New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()

		baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		// Poll /health until the proxy is listening.
		ready := false
		for i := 0; i < 100; i++ {
			req, _ := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					ready = true
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ready {
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("Run returned error on shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("Run did not return after context cancel (graceful shutdown hung)")
				}
			}()
			break
		}
		// Not ready: tear down and retry on a fresh port.
		cancel()
		<-done
	}

	if baseURL == "" {
		t.Fatal("gateway never became ready")
	}

	// Real chat request through the live Run() server. Use a no-keep-alive client
	// so the connection closes promptly and graceful shutdown does not block on it.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	body := new(strings.Builder)
	_, _ = io.Copy(body, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat status %d: %s", resp.StatusCode, body.String())
	}
	if _, ok := up.lastBody(); !ok {
		t.Fatal("upstream did not receive the forwarded request")
	}
}
