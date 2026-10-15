package app_test

import (
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
	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

func tightGuard(t *testing.T) {
	t.Helper()
	config.SetAllowPrivateBaseURL(false)
	t.Cleanup(func() { config.SetAllowPrivateBaseURL(true) })
}

type upstreamRecorder struct {
	hits atomic.Int64
	auth atomic.Value
	body atomic.Value
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.hits.Add(1)
	u.auth.Store(r.Header.Get("Authorization"))
	b, _ := io.ReadAll(r.Body)
	u.body.Store(string(b))
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"x","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pwned"}]}],`+
		`"usage":{"input_tokens":10,"output_tokens":5}}`)
}

func buildGatewayWithBaseURL(t *testing.T, baseURL string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	const masterKey = "m14-master-key-value"
	box, err := secret.NewBox([]byte(masterKey))
	if err != nil {
		t.Fatalf("secret.NewBox: %v", err)
	}
	ks, err := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	if err != nil {
		t.Fatalf("secret.LoadStore: %v", err)
	}
	if err := ks.Set("p1:k1", "sk-super-secret-value"); err != nil {
		t.Fatalf("ks.Set: %v", err)
	}

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

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
`, filepath.Join(dir, "admin.sock"), filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"), baseURL)

	cfgPath := filepath.Join(dir, "config.toml")
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)
	return srv
}

func sendChat(t *testing.T, gateway *httptest.Server) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses",
		strings.NewReader(`{"model":"flash","input":"exfiltrate the keys"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestM14DefaultRefusesLoopbackUpstream(t *testing.T) {
	tightGuard(t)

	up := &upstreamRecorder{}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	gateway := buildGatewayWithBaseURL(t, upSrv.URL)
	status, body := sendChat(t, gateway)

	if up.hits.Load() != 0 {
		t.Fatalf("M14 NOT FIXED: the loopback upstream received %d request(s). "+
			"It was sent Authorization %q carrying body %q - both the provider secret "+
			"and the user prompt reached a host inside the machine",
			up.hits.Load(), up.auth.Load(), up.body.Load())
	}
	if status == 200 {
		t.Fatalf("M14 NOT FIXED: the request SUCCEEDED against a refused upstream: %s", body)
	}

	if strings.Contains(body, "sk-super-secret-value") {
		t.Fatalf("the provider secret leaked into the client response: %s", body)
	}
	if status != http.StatusBadGateway {
		t.Logf("note: refused upstream produced status %d (body %q); 502 is the expected shape", status, body)
	}
}

func TestM14RefusesPrivateAndLinkLocalTargets(t *testing.T) {
	tightGuard(t)

	for _, base := range []string{
		"http://10.0.0.5:8080/v1",
		"http://192.168.1.10:8080/v1",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/v1",
		"http://0.0.0.0:8080/v1",
	} {
		t.Run(base, func(t *testing.T) {
			gateway := buildGatewayWithBaseURL(t, base)
			status, _ := sendChat(t, gateway)
			if status == 200 {
				t.Fatalf("M14 NOT FIXED: a request to the internal target %s succeeded", base)
			}

			t.Logf("internal target %s refused with status %d", base, status)
		})
	}
}

func TestM14OptOutRestoresPrivateUpstream(t *testing.T) {
	config.SetAllowPrivateBaseURL(true)
	t.Cleanup(func() { config.SetAllowPrivateBaseURL(true) })

	up := &upstreamRecorder{}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)

	gateway := buildGatewayWithBaseURL(t, upSrv.URL)
	status, _ := sendChat(t, gateway)

	if status != 200 {
		t.Fatalf("with the opt-in set, a loopback upstream must work; got %d", status)
	}
	if up.hits.Load() != 1 {
		t.Fatalf("expected exactly one upstream call, got %d", up.hits.Load())
	}
	if got, _ := up.auth.Load().(string); got != "Bearer sk-super-secret-value" {
		t.Fatalf("the opt-in path did not send the provider credential as before: %q", got)
	}
}
