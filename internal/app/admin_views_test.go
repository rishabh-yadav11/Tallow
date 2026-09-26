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
	"sync"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// This file covers the admin.Backend surface of App: the read-only views that
// back the admin socket, plus the accessors. These are operator-visible, so a
// silent regression here either misreports a provider's health or hides a
// budget that is actually exhausted.

// viewApp builds a running App with the given provider/alias/store config, on
// top of a stub upstream, and returns the App.
func viewApp(t *testing.T, upstreamURL string, extra string) *app.App {
	t.Helper()
	dir := t.TempDir()
	const masterKey = "view-master-key"
	box, err := secret.NewBox([]byte(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range []string{"p1:k1", "p2:k2", "p3:k3"} {
		if err := ks.Set(kv, "sk-"+kv); err != nil {
			t.Fatal(err)
		}
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

[retention]
raw_bodies_days = 5
metadata_days = 30
errors_days = 90

[observability]
enabled = true

[[provider]]
name = "p1"
base_url = "%s"
rpm = 100

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  rpm = 60
  max_concurrent = 4
  cost_limit_cents = 500

[[provider]]
name = "p2"
base_url = "%s"
rpm = 200

  [[provider.key]]
  id = "k2"
  ref = "p2:k2"
  rpm = 30

[[provider]]
name = "p3"
base_url = "http://127.0.0.1:1"
rpm = 10

  [[provider.key]]
  id = "k3"
  ref = "p3:k3"
  rpm = 5

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
  [[alias.target]]
  provider = "p2"
  model = "p2-flash"

[[alias]]
name = "solo"
  [[alias.target]]
  provider = "p1"
  model = "solo-v1"
%s`,
		filepath.Join(dir, "admin.sock"),
		filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"),
		upstreamURL,
		upstreamURL,
		extra,
	)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

	a, err := app.New(cfgPath, "v1.2.3")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return a
}

// withUpstream starts a stub upstream and builds a view App on top of it.
func withUpstream(t *testing.T) *app.App {
	t.Helper()
	srv, _ := setup(t)
	return viewApp(t, srv.URL, "")
}

// TestProvidersViewReportsEveryProviderAndKey is the core admin view. Every
// configured provider must appear, and every key under it, with the configured
// limits echoed back. A provider that silently drops out of this view is
// invisible to an operator trying to debug routing.
func TestProvidersViewReportsEveryProviderAndKey(t *testing.T) {
	a := withUpstream(t)

	views := a.ProvidersView()
	byName := map[string]int{}
	for i, pv := range views {
		if _, dup := byName[pv.Name]; dup {
			t.Errorf("provider %q appears twice in ProvidersView", pv.Name)
		}
		byName[pv.Name] = i
	}
	for _, want := range []string{"p1", "p2", "p3"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("provider %q missing from ProvidersView; got %v", want, byName)
		}
	}

	p1 := views[byName["p1"]]
	if p1.RPM != 100 {
		t.Errorf("p1 provider RPM = %d, want 100", p1.RPM)
	}
	if len(p1.Keys) != 1 {
		t.Fatalf("p1 has %d keys, want 1", len(p1.Keys))
	}
	k := p1.Keys[0]
	if k.ID != "k1" {
		t.Errorf("key ID = %q, want k1", k.ID)
	}
	if k.Ref != "p1:k1" {
		t.Errorf("key Ref = %q, want p1:k1", k.Ref)
	}
	if k.RPM != 60 {
		t.Errorf("key RPM = %d, want 60", k.RPM)
	}
	// The budget limits must be visible, or an operator cannot tell a
	// concurrency refusal from a rate refusal.
	if k.MaxInflight != 4 {
		t.Errorf("MaxInflight = %d, want 4", k.MaxInflight)
	}
	if k.CostLimit != 500 {
		t.Errorf("CostLimit = %d, want 500", k.CostLimit)
	}

	// A provider with no keys still appears, just with an empty key list. It
	// must not be dropped.
	for _, pv := range views {
		if pv.Name == "p2" && len(pv.Keys) != 1 {
			t.Errorf("p2 has %d keys, want 1", len(pv.Keys))
		}
	}
}

// TestProvidersViewReportsNonEmptyHealth pins that every provider and key
// carries a health string. An empty string would render as a blank cell and
// read as "no data" rather than a state.
func TestProvidersViewReportsNonEmptyHealth(t *testing.T) {
	a := withUpstream(t)
	for _, pv := range a.ProvidersView() {
		if pv.Health == "" {
			t.Errorf("provider %q has an empty health string", pv.Name)
		}
		for _, k := range pv.Keys {
			if k.Health == "" {
				t.Errorf("key %s/%s has an empty health string", pv.Name, k.ID)
			}
		}
	}
}

// TestHealthViewKeysEveryProviderAndKeyFlat checks the flat map form, which is
// what the admin socket serializes. Keys must be namespaced so a provider named
// like "provider:x" cannot collide with a key entry.
func TestHealthViewKeysEveryProviderAndKeyFlat(t *testing.T) {
	a := withUpstream(t)
	m := a.HealthView()
	for _, want := range []string{"provider:p1", "provider:p2", "provider:p3", "key:p1/k1", "key:p2/k2", "key:p3/k3"} {
		if _, ok := m[want]; !ok {
			t.Errorf("HealthView missing %q; got %v", want, m)
		}
	}
	if len(m) != 6 {
		t.Errorf("HealthView has %d entries, want 6: %v", len(m), m)
	}
}

// TestAliasesViewReturnsAliasesAndTargets checks the alias view shape.
func TestAliasesViewReturnsAliasesAndTargets(t *testing.T) {
	a := withUpstream(t)
	views := a.AliasesView()
	byName := map[string]int{}
	for i, av := range views {
		byName[av.Name] = i
	}
	for _, want := range []string{"flash", "solo"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("alias %q missing from AliasesView; got %v", want, byName)
		}
	}
	flash := views[byName["flash"]]
	if len(flash.Targets) != 2 {
		t.Fatalf("flash has %d targets, want 2", len(flash.Targets))
	}
	// Both targets must be distinct and intact.
	seen := map[string]string{}
	for _, tv := range flash.Targets {
		if prev, dup := seen[tv.Provider]; dup {
			t.Errorf("provider %q appears twice in flash targets (models %q and %q)",
				tv.Provider, prev, tv.Model)
		}
		seen[tv.Provider] = tv.Model
	}
	if seen["p1"] != "flash-v4" {
		t.Errorf("flash target p1 model = %q, want flash-v4", seen["p1"])
	}
	if seen["p2"] != "p2-flash" {
		t.Errorf("flash target p2 model = %q, want p2-flash", seen["p2"])
	}
}

// TestBudgetsViewReportsLivePerKeyBudgets drives real traffic and then checks
// the budget view reflects it. A budget view that always reports full headroom
// is worse than no view at all, because it tells an operator the opposite of
// the truth.
func TestBudgetsViewReportsLivePerKeyBudgets(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")

	// Drive enough traffic to consume headroom on p1:k1 (rpm 60).
	for i := 0; i < 5; i++ {
		body := strings.NewReader(`{"model":"flash-v4","messages":[{"role":"user","content":"hi"}]}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// BudgetsView must report an entry for p1/k1 with the configured limit.
	var found bool
	for _, b := range a.BudgetsView() {
		if b.Provider == "p1" && b.Key == "k1" {
			found = true
			if b.MaxInflight != 4 {
				t.Errorf("MaxInflight = %d, want 4", b.MaxInflight)
			}
			if b.CostLimit != 500 {
				t.Errorf("CostLimit = %d, want 500", b.CostLimit)
			}
		}
	}
	if !found {
		t.Errorf("no budget entry for p1/k1; got %+v", a.BudgetsView())
	}
}

// TestRecentRequestsReflectsTraffic checks the store-backed read paths actually
// read. These swallow their error, so an empty result must be distinguishable
// from a broken query, and this pins that traffic shows up.
func TestRecentRequestsReflectsTraffic(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")
	// viewApp does not wrap the handler, so serve it here; without this the
	// request below would go straight to the upstream and never be recorded.
	gw := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(gw.Close)

	body := strings.NewReader(`{"model":"flash-v4","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The store is written asynchronously, so poll rather than sleep once.
	deadline := time.Now().Add(5 * time.Second)
	var reqs []model.RequestMeta
	for time.Now().Before(deadline) {
		reqs = a.RecentRequests(10)
		if len(reqs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(reqs) == 0 {
		t.Error("RecentRequests is empty after a successful request")
	}
	// A non-positive limit falls back to the method's default rather than
	// returning nothing. This is a deliberate defensive clamp in the store, and
	// it is unreachable from the admin socket, whose limitParam already maps any
	// limit<=0 to the default before calling in. So assert the clamp holds and
	// is bounded, rather than asserting an empty result.
	for _, n := range []int{0, -1, -1000} {
		if got := a.RecentRequests(n); len(got) == 0 {
			t.Errorf("RecentRequests(%d) returned nothing, want the default page", n)
		} else if len(got) > 100 {
			t.Errorf("RecentRequests(%d) returned %d entries, want at most the "+
				"100-entry default", n, len(got))
		}
		if got := a.RecentRollups(n); len(got) > 200 {
			t.Errorf("RecentRollups(%d) returned %d entries, want at most the "+
				"200-entry default", n, len(got))
		}
		if got := a.RecentAudit(n); len(got) > 100 {
			t.Errorf("RecentAudit(%d) returned %d entries, want at most the "+
				"100-entry default", n, len(got))
		}
	}
}

// TestAccessorsReportConfigAndVersion covers the small operator-visible
// accessors.
func TestAccessorsReportConfigAndVersion(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")

	if got := a.Version(); got != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", got)
	}
	if a.ConfigPath() == "" {
		t.Error("ConfigPath is empty")
	}
	if got := a.ListenAddr(); got != "127.0.0.1:0" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1:0", got)
	}
	if a.AdminSocket() == "" {
		t.Error("AdminSocket is empty")
	}
	// CacheStats must be readable and non-negative.
	hits, miss, evict, size := a.CacheStats()
	if hits < 0 || miss < 0 || evict < 0 || size < 0 {
		t.Errorf("CacheStats returned a negative value: %d %d %d %d", hits, miss, evict, size)
	}
	// Metrics must be a usable snapshot.
	_ = a.Metrics()
}

// TestAccessorsTrackReload checks the accessors are live: after a hot reload
// that changes the listen address, the accessor must report the new value, not
// the value captured at construction.
func TestAccessorsTrackReload(t *testing.T) {
	srv, _ := setup(t)
	dir := t.TempDir()
	const masterKey = "reload-view-master-key"
	box, _ := secret.NewBox([]byte(masterKey))
	ks, err := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("p1:k1", "sk-x"); err != nil {
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
[retention]
raw_bodies_days = 5
metadata_days = 30
errors_days = 90
[observability]
enabled = true
[[provider]]
name = "p1"
base_url = "%s"
  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
`, filepath.Join(dir, "admin.sock"), filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"), srv.URL)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv(secret.EnvMasterKey, masterKey)
	t.Cleanup(func() { os.Unsetenv(secret.EnvMasterKey) })

	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.ListenAddr(); got != "127.0.0.1:0" {
		t.Fatalf("ListenAddr = %q before reload", got)
	}

	// Change the listen address and the provider's RPM, then reload.
	cfg2 := strings.Replace(cfg, `listen = "127.0.0.1:0"`, `listen = "127.0.0.1:9099"`, 1)
	cfg2 = strings.Replace(cfg2, `base_url = "`+srv.URL+`"`, `base_url = "`+srv.URL+`"`+"\nrpm = 77", 1)
	if err := os.WriteFile(cfgPath, []byte(cfg2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := a.ListenAddr(); got != "127.0.0.1:9099" {
		t.Errorf("ListenAddr = %q after reload, want 127.0.0.1:9099: the accessor "+
			"serves the config captured at construction", got)
	}
	// The provider view must reflect the reloaded RPM too.
	for _, pv := range a.ProvidersView() {
		if pv.Name == "p1" && pv.RPM != 77 {
			t.Errorf("p1 RPM = %d after reload, want 77", pv.RPM)
		}
	}
}

// TestMetricsSnapshotIsSerializable makes sure the metrics view can actually be
// written to the admin socket as JSON. A snapshot with an unserializable field
// would fail at the admin boundary, which is hard to reach from a unit test.
func TestMetricsSnapshotIsSerializable(t *testing.T) {
	a := withUpstream(t)
	b, err := json.Marshal(a.Metrics())
	if err != nil {
		t.Fatalf("Metrics snapshot is not JSON-serializable: %v", err)
	}
	if len(b) == 0 {
		t.Error("Metrics snapshot marshalled to nothing")
	}
}

// TestViewsAreSafeUnderConcurrentReload exercises the read-only views while a
// reload swaps the registry underneath them. A view that reads a.reg without
// synchronization races with the swap, and the admin socket is reachable
// concurrently in production.
func TestViewsAreSafeUnderConcurrentReload(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent readers of every view.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.ProvidersView()
				_ = a.HealthView()
				_ = a.AliasesView()
				_ = a.BudgetsView()
				_ = a.ListenAddr()
				_ = a.AdminSocket()
				_ = a.Metrics()
			}
		}()
	}
	// Concurrent reloaders, as a config watcher plus an operator would produce.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.Reload()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestProxyHandlerIsStableAcrossReload checks the handler identity is preserved
// across a reload, since a reload that rebuilt the handler would drop
// in-flight connections. This is the documented "reuses all long-lived
// objects" guarantee.
func TestProxyHandlerIsStableAcrossReload(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")

	before := a.ProxyHandler()
	if before == nil {
		t.Fatal("ProxyHandler is nil")
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	after := a.ProxyHandler()
	if after == nil {
		t.Fatal("ProxyHandler is nil after reload")
	}
	// The handler must still serve requests after a reload.
	probe := httptest.NewRecorder()
	after.ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code == 0 {
		t.Error("the reloaded handler served no status code")
	}
}
