package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// This file exercises the gateway the way an operator does: a real App.Run
// with a live proxy listener, a real admin Unix domain socket, real HTTP
// requests over that socket, and a real config file rewritten on disk to force
// a hot reload.
//
// TestAppRunEndToEnd already proves a chat request survives a full Run
// lifecycle. What it does not touch is the control plane, and that is where
// the operator-visible guarantees live: the admin socket must actually serve
// over its Unix socket, /reload must take effect on a live gateway rather than
// only in-process, secrets must never cross the socket, and a provider that
// starts failing must be taken out of rotation and put back.
//
// Every assertion here is made against a real response from a real socket. No
// App method is called directly, so a view that renders correctly in-process
// but is unreachable over the socket would still fail here.

// adminRequest performs a request over the admin Unix socket and returns the
// status and body. Dialing the socket directly is the point: it proves the
// admin interface is actually served on the local socket, not just registered.
func adminRequest(t *testing.T, method, sock, path string) (int, []byte) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
		},
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequest(method, "http://admin"+path, nil)
	if err != nil {
		t.Fatalf("build admin request %s %s: %v", method, path, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("admin %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func adminGet(t *testing.T, sock, path string) (int, []byte) {
	t.Helper()
	return adminRequest(t, http.MethodGet, sock, path)
}

func adminPost(t *testing.T, sock, path string) (int, []byte) {
	t.Helper()
	return adminRequest(t, http.MethodPost, sock, path)
}

// waitForAdmin polls the admin socket until it answers, so the test does not
// race the listener coming up.
func waitForAdmin(t *testing.T, sock string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if code, _ := adminGet(t, sock, "/health"); code == 200 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the admin socket never became ready at %s", sock)
}

// liveGateway is a fully running gateway plus the handles a test needs to talk
// to it and to change its configuration.
type liveGateway struct {
	baseURL   string
	adminSock string
	cfgPath   string
	app       *app.App
	cancel    context.CancelFunc
	done      chan error
}

// writeConfig writes a gateway config pointing at the given upstream, and
// signals the caller that the file changed so the watcher picks it up.
func writeConfig(t *testing.T, path, upstream, adminSock, keystore, db string) {
	t.Helper()
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
  # Prices are set so recorded spend is non-zero. Without them every request
  # costs nothing, and an assertion that the budget view moves could only ever
  # be satisfied by a view that reports garbage. The key is price_input_per_1m:
  # pricing is per 1M tokens, and a wrong key here is silently ignored, so the
  # numbers stay at zero with no error anywhere.
  price_input_per_1m = 3.0
  price_output_per_1m = 15.0
`, adminSock, keystore, db, upstream)
	// Write via a temp file and rename, which is what a config editor or
	// deployment tool does and is atomic, so the watcher never sees a torn file.
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// startLive boots a real gateway via App.Run and waits for BOTH the proxy
// listener and the admin socket to answer.
func startLive(t *testing.T, upstreamURL string) *liveGateway {
	t.Helper()
	dir := t.TempDir()
	adminSock := filepath.Join(dir, "admin.sock")
	cfgPath := filepath.Join(dir, "config.toml")
	keystore := filepath.Join(dir, "keys.json")
	db := filepath.Join(dir, "tallow.db")

	const masterKey = "integration-master-key"
	box, err := secret.NewBox([]byte(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.LoadStore(keystore, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("p1:k1", "sk-fake"); err != nil {
		t.Fatal(err)
	}

	// The config needs a concrete port, so pick a free one and retry on failure.
	var g *liveGateway
	for attempt := 0; attempt < 5 && g == nil; attempt++ {
		port := freePort(t)
		cfg := fmt.Sprintf(`version = 1
[auth]
open = true   # integration harness: no auth exercise, accept any token.

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
  price_input_per_1m = 3.0
  price_output_per_1m = 15.0
`, port, adminSock, keystore, db, upstreamURL)
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
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()

		baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
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
		if !ready {
			cancel()
			<-done
			continue
		}
		g = &liveGateway{
			baseURL:   baseURL,
			adminSock: adminSock,
			cfgPath:   cfgPath,
			app:       a,
			cancel:    cancel,
			done:      done,
		}
	}
	if g == nil {
		t.Fatal("gateway never became ready")
	}
	waitForAdmin(t, g.adminSock)
	t.Cleanup(func() { g.stop(t) })
	return g
}

func (g *liveGateway) stop(t *testing.T) {
	t.Helper()
	g.cancel()
	select {
	case err := <-g.done:
		if err != nil {
			t.Errorf("Run returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Run did not return after context cancel: graceful shutdown hung")
	}
}

// chat sends a real chat request through the live proxy and returns the status
// and body. Keep-alives are disabled so shutdown is never blocked on an idle
// connection.
//
// text is the user message. It must be unique per call: the gateway caches
// responses, and a repeat of an identical prompt is served from cache without
// ever reaching an upstream. That is correct product behaviour, but it makes a
// repeated prompt useless for asking "which upstream served this?".
func (g *liveGateway) chat(t *testing.T, text string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, g.baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"flash","messages":[{"role":"user","content":`+strconv.Quote(text)+`}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// chatN sends n distinct requests, so none of them can be served from cache.
func (g *liveGateway) chatN(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if code, body := g.chat(t, fmt.Sprintf("request-%d", i)); code != 200 {
			t.Fatalf("chat %d: status %d: %s", i, code, body)
		}
	}
}

// TestAdminSocketServesTheControlPlaneOverItsUnixSocket checks the admin
// interface is genuinely reachable the way a frontend or tallowctl reaches it.
// Asserting on App's own view methods would pass even if the socket were never
// served, misconfigured, or bound somewhere else.
func TestAdminSocketServesTheControlPlaneOverItsUnixSocket(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	// A real request through the proxy, so the views have something to show.
	if code, body := g.chat(t, "control-plane-probe"); code != 200 {
		t.Fatalf("chat status %d: %s", code, body)
	}

	for _, path := range []string{"/health", "/metrics", "/providers", "/aliases", "/budgets", "/health-states", "/logs"} {
		code, body := adminGet(t, g.adminSock, path)
		if code != 200 {
			t.Errorf("admin GET %s returned %d: %s", path, code, body)
		}
	}

	// /providers must describe the configured provider and key, read back off
	// the socket as JSON, not asserted through a Go accessor.
	code, body := adminGet(t, g.adminSock, "/providers")
	if code != 200 {
		t.Fatalf("/providers returned %d", code)
	}
	var providers []struct {
		Name string `json:"name"`
		Keys []struct {
			ID     string `json:"id"`
			Ref    string `json:"ref"`
			Health string `json:"health"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &providers); err != nil {
		t.Fatalf("/providers did not return a provider list: %v (%s)", err, body)
	}
	if len(providers) != 1 || providers[0].Name != "p1" {
		t.Fatalf("/providers returned %+v, want exactly one provider named p1", providers)
	}
	if len(providers[0].Keys) != 1 || providers[0].Keys[0].ID != "k1" {
		t.Errorf("/providers returned keys %+v, want exactly one key k1", providers[0].Keys)
	}
	if providers[0].Keys[0].Ref != "p1:k1" {
		t.Errorf("key ref is %q, want %q", providers[0].Keys[0].Ref, "p1:k1")
	}
}

// TestAdminSocketNeverExposesSecretMaterial is the security assertion. The
// admin interface is the control plane; a provider API key appearing anywhere
// in it, or in the raw bytes of any response, would leak the credential to
// every frontend and to anything that can read the socket.
func TestAdminSocketNeverExposesSecretMaterial(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	if code, body := g.chat(t, "secret-scan-probe"); code != 200 {
		t.Fatalf("chat status %d: %s", code, body)
	}

	// The keystore really does hold the secret, so a negative result here is
	// meaningful rather than vacuous.
	const mustNotAppear = "sk-fake"

	for _, path := range []string{
		"/health", "/metrics", "/providers", "/aliases", "/budgets",
		"/health-states", "/logs", "/rollups", "/audit", "/cache", "/config",
	} {
		code, body := adminGet(t, g.adminSock, path)
		if code == 404 {
			continue // an endpoint this build does not have is not a leak
		}
		if strings.Contains(string(body), mustNotAppear) {
			t.Errorf("admin GET %s leaked the provider secret in its response body", path)
		}
	}

	// The keystore file on disk must still be the only place it lives, and the
	// file itself must not be world-readable.
	dir := filepath.Dir(g.adminSock)
	fi, err := os.Stat(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatalf("stat keystore: %v", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("keystore mode is %04o, want group and other bits cleared", mode)
	}

	// And the admin socket itself must not be reachable by other users.
	sfi, err := os.Stat(g.adminSock)
	if err != nil {
		t.Fatalf("stat admin socket: %v", err)
	}
	if mode := sfi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("admin socket mode is %04o, want it owner-only: it exposes the "+
			"whole control plane including reload", mode)
	}
}

// TestAdminReloadAppliesToTheLiveGateway proves /reload is a real control, not
// a stub. The config on disk is changed to a second upstream, the reload is
// issued over the admin socket, and the next real request must be served by
// the NEW upstream. If reload only worked in-process, or the gateway kept
// serving from a stale registry, this would keep hitting the old server.
func TestAdminReloadAppliesToTheLiveGateway(t *testing.T) {
	upSrv1, up1 := setup(t)
	g := startLive(t, upSrv1.URL)

	// Baseline: the first upstream serves the request.
	if code, body := g.chat(t, "before-reload"); code != 200 {
		t.Fatalf("chat status %d: %s", code, body)
	}
	if _, ok := up1.lastBody(); !ok {
		t.Fatal("the original upstream did not receive the request")
	}

	// Bring up a second upstream and repoint the config at it.
	upSrv2, up2 := setup(t)
	writeConfig(t, g.cfgPath, upSrv2.URL, g.adminSock,
		filepath.Join(filepath.Dir(g.adminSock), "keys.json"),
		filepath.Join(filepath.Dir(g.adminSock), "tallow.db"))

	// Ask the running gateway to reload, over the socket. /reload mutates the
	// gateway, so it is POST-only; a GET must be refused rather than acted on.
	code, body := adminGet(t, g.adminSock, "/reload")
	if code != http.StatusMethodNotAllowed {
		t.Errorf("a GET to /reload returned %d, want 405: a mutating control must "+
			"not be reachable with a plain GET", code)
	}
	if code, body := adminPost(t, g.adminSock, "/reload"); code != 200 {
		t.Fatalf("/reload returned %d: %s", code, body)
	}

	// A request issued right after the reload must reach the new upstream.
	// Every prompt is distinct so the response cache cannot answer on the
	// gateway's behalf: a cached reply proves the gateway is alive, not which
	// upstream it would have called.
	var reached bool
	for i := 0; i < 20 && !reached; i++ {
		if code, body := g.chat(t, fmt.Sprintf("after-reload-%d", i)); code != 200 {
			t.Fatalf("chat after reload: status %d: %s", code, body)
		}
		if _, ok := up2.lastBody(); ok {
			reached = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reached {
		t.Error("after a reload the gateway still served the OLD upstream: the " +
			"config on disk changed and /reload returned 200, but traffic did not move")
	}

	// And the admin view must report the new upstream, proving the reload
	// reached the registry the views read from.
	code, body = adminGet(t, g.adminSock, "/providers")
	if code != 200 {
		t.Fatalf("/providers after reload returned %d", code)
	}
	if !strings.Contains(string(body), upSrv2.URL) {
		t.Errorf("/providers does not report the new base URL after a reload: %s", body)
	}
}

// TestAdminReloadReportsARejection proves a bad reload fails loudly instead of
// leaving the gateway in a half-swapped state. An operator who typos a config
// must get an error and must keep the previously working configuration.
func TestAdminReloadReportsARejection(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	if code, body := g.chat(t, "before-bad-reload"); code != 200 {
		t.Fatalf("chat status %d: %s", code, body)
	}
	// Write a syntactically invalid config over the live one.
	if err := os.WriteFile(g.cfgPath, []byte("this is not = = valid toml [[["), 0o600); err != nil {
		t.Fatal(err)
	}

	code, body := adminPost(t, g.adminSock, "/reload")
	if code == 200 {
		t.Errorf("/reload accepted an invalid config with status 200: %s", body)
	}

	// The gateway must still be serving. A failed reload that took down the
	// live proxy would turn a typo into a full outage. The prompt is distinct
	// so this cannot be answered from the response cache.
	if code, body := g.chat(t, "after-bad-reload"); code != 200 {
		t.Errorf("after a rejected reload the gateway stopped serving: status %d: %s", code, body)
	}
}

// TestAdminUnknownRouteIs404NotASilentSuccess confirms the control plane does
// not answer 200 for endpoints it does not implement, which would let a
// frontend believe a control it never performed succeeded.
func TestAdminUnknownRouteIs404NotASilentSuccess(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	code, _ := adminGet(t, g.adminSock, "/definitely-not-a-route")
	if code != 404 {
		t.Errorf("an unknown admin route returned %d, want 404", code)
	}
}

// TestBudgetsViewOverTheSocketReflectsLiveTraffic ties the admin view to real
// usage. A budget endpoint that always reported a zero spend and a full window
// would look fine in isolation but would be useless to an operator watching
// cost, so this drives real traffic and asserts both that the recorded spend
// moves and that every concurrency slot taken by a request comes back.
func TestBudgetsViewOverTheSocketReflectsLiveTraffic(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	type budgetStatus struct {
		Provider     string `json:"provider"`
		Key          string `json:"key"`
		RPMRemaining int    `json:"rpm_remaining"`
		Inflight     int    `json:"inflight"`
		CostCents    int64  `json:"cost_cents"`
	}
	read := func() []budgetStatus {
		t.Helper()
		code, body := adminGet(t, g.adminSock, "/budgets")
		if code != 200 {
			t.Fatalf("/budgets returned %d: %s", code, body)
		}
		var out []budgetStatus
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("/budgets did not return a list: %v (%s)", err, body)
		}
		return out
	}
	totals := func(in []budgetStatus) (rpm int, cost int64) {
		for _, b := range in {
			rpm += b.RPMRemaining
			cost += b.CostCents
		}
		return rpm, cost
	}

	before := read()
	if len(before) == 0 {
		t.Fatal("/budgets reported no tracked keys at all: an operator cannot " +
			"watch a budget that is not listed")
	}
	rpmBefore, costBefore := totals(before)

	// The configured key RPM is 100, so a fresh window must report it in full.
	// This is the positive control: without it, "the numbers did not move"
	// would be unfalsifiable.
	if rpmBefore != 100 {
		t.Errorf("/budgets reports rpm_remaining=%d for a fresh window, want the "+
			"configured 100", rpmBefore)
	}

	// Drive real traffic, then re-read.
	g.chatN(t, 3)

	after := read()
	rpmAfter, costAfter := totals(after)

	if rpmAfter >= rpmBefore {
		t.Errorf("/budgets reports rpm_remaining=%d after 3 real requests, which "+
			"is not less than the %d of a fresh window: the admin view is not "+
			"tracking live usage", rpmAfter, rpmBefore)
	}
	if rpmBefore-rpmAfter != 3 {
		t.Errorf("3 requests moved rpm_remaining by %d, want exactly 3: the view "+
			"is not accounting one slot per request", rpmBefore-rpmAfter)
	}
	if costAfter <= costBefore {
		t.Errorf("/budgets reports cost_cents=%d after 3 real billable requests, "+
			"which is not more than the %d before them: the admin view is not "+
			"tracking live spend", costAfter, costBefore)
	}
	// Every slot taken by a request must come back, or the key is capped
	// permanently. A completed request must not leave anything in flight.
	for _, b := range after {
		if b.Inflight != 0 {
			t.Errorf("/budgets reports inflight=%d for %s/%s after all requests "+
				"completed: a leaked slot caps this key permanently",
				b.Inflight, b.Provider, b.Key)
		}
	}
}
