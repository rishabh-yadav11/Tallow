package app_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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
