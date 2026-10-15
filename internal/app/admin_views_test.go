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

func withUpstream(t *testing.T) *app.App {
	t.Helper()
	srv, _ := setup(t)
	return viewApp(t, srv.URL, "")
}

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

	if k.MaxInflight != 4 {
		t.Errorf("MaxInflight = %d, want 4", k.MaxInflight)
	}
	if k.CostLimit != 500 {
		t.Errorf("CostLimit = %d, want 500", k.CostLimit)
	}

	for _, pv := range views {
		if pv.Name == "p2" && len(pv.Keys) != 1 {
			t.Errorf("p2 has %d keys, want 1", len(pv.Keys))
		}
	}
}

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

func TestBudgetsViewReportsLivePerKeyBudgets(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")

	for i := 0; i < 5; i++ {
		body := strings.NewReader(`{"model":"flash-v4","input":"hi"}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

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

func TestRecentRequestsReflectsTraffic(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")

	gw := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(gw.Close)

	body := strings.NewReader(`{"model":"flash-v4","input":"hi"}`)
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/responses", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

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

	hits, miss, evict, size := a.CacheStats()
	if hits < 0 || miss < 0 || evict < 0 || size < 0 {
		t.Errorf("CacheStats returned a negative value: %d %d %d %d", hits, miss, evict, size)
	}

	_ = a.Metrics()
}

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

	for _, pv := range a.ProvidersView() {
		if pv.Name == "p1" && pv.RPM != 77 {
			t.Errorf("p1 RPM = %d after reload, want 77", pv.RPM)
		}
	}
}

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

func TestViewsAreSafeUnderConcurrentReload(t *testing.T) {
	srv, _ := setup(t)
	a := viewApp(t, srv.URL, "")

	var wg sync.WaitGroup
	stop := make(chan struct{})

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

	probe := httptest.NewRecorder()
	after.ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code == 0 {
		t.Error("the reloaded handler served no status code")
	}
}
