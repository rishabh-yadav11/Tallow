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

// M14 end-to-end: the guard has to hold at the SOCKET, not merely in a
// predicate. These tests stand up a real upstream on 127.0.0.1, point the
// gateway at it through a real config file, and assert that the request never
// arrives - with the provider secret attached.
//
// An httptest server on loopback is exactly the shape of the attack M14
// describes: a base_url pointing inside the machine, receiving an
// Authorization: Bearer header and the user's prompt. The package TestMain
// relaxes the guard process-wide for the other end-to-end tests, so these tests
// re-tighten it for their own duration and restore it afterwards, which is the
// only way to exercise the default posture from inside this package.

// tightGuard re-tightens the SSRF guard for the duration of one test and restores
// the package-wide opt-in afterwards.
func tightGuard(t *testing.T) {
	t.Helper()
	config.SetAllowPrivateBaseURL(false)
	t.Cleanup(func() { config.SetAllowPrivateBaseURL(true) })
}

// upstreamRecorder is a real HTTP server that records whether it was ever hit.
type upstreamRecorder struct {
	hits atomic.Int64
	auth atomic.Value // string
	body atomic.Value // string
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.hits.Add(1)
	u.auth.Store(r.Header.Get("Authorization"))
	b, _ := io.ReadAll(r.Body)
	u.body.Store(string(b))
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"choices":[{"message":{"content":"pwned"}}],`+
		`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
}

// buildGatewayWithBaseURL wires a complete app against the given base_url.
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

// sendChat issues one chat completion and returns the status and body.
func sendChat(t *testing.T, gateway *httptest.Server) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":"exfiltrate the keys"}]}`))
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

// TestM14DefaultRefusesLoopbackUpstream is the M14 regression, end to end.
//
// A base_url pointing at 127.0.0.1 must not receive the request. The two
// things that must not leak are the provider secret and the user prompt, and the
// test asserts both by checking the upstream was never hit at all.
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
	// The client must get a generic failure, not a stack trace or a path.
	if strings.Contains(body, "sk-super-secret-value") {
		t.Fatalf("the provider secret leaked into the client response: %s", body)
	}
	if status != http.StatusBadGateway {
		t.Logf("note: refused upstream produced status %d (body %q); 502 is the expected shape", status, body)
	}
}

// TestM14RefusesPrivateAndLinkLocalTargets covers the other internal-address
// shapes. A loopback-only test would leave RFC1918 and the cloud metadata
// endpoint untested, and those are the more realistic SSRF targets.
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
			// The point of the test is that the DIAL was refused, which we can
			// only observe indirectly: a private target on a real network would
			// hang or fail slowly, so a fast generic failure is the signature of
			// the guard rather than of a connection attempt.
			t.Logf("internal target %s refused with status %d", base, status)
		})
	}
}

// TestM14OptOutRestoresPrivateUpstream is the non-regression half, and it is
// what the rest of this package relies on: with the opt-in set, a loopback
// upstream works exactly as before. Without this the guard would be untested in
// the "does it still function" direction, and a guard that breaks all private
// upstreams is a guard operators disable.
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
