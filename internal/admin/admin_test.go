package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

// testSecret is a plaintext value the fake Backend "holds" internally. No
// admin endpoint may ever surface it in a response body.
const testSecret = "sk-test-secret-do-not-leak-42"

const testVersion = "v9.9.9-test"

const testConfigPath = "/etc/tallow/config.toml"

// fakeBackend implements Backend with canned data and records what the admin
// server asks for, so limit handling and reload invocation are observable.
type fakeBackend struct {
	mu        sync.Mutex
	snap      observ.Snapshot
	providers []ProviderView
	aliases   []AliasView
	budgets   []routing.BudgetStatus
	health    map[string]string
	reqs      []model.RequestMeta
	rollups   []store.Rollup
	audit     []store.AuditEntry
	hit       int64
	miss      int64
	evict     int64
	size      int64
	reloadErr error

	reloadCalls   int
	lastRequestsN int
	lastRollupsN  int
	lastAuditN    int
}

func (f *fakeBackend) Metrics() observ.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeBackend) ProvidersView() []ProviderView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.providers
}

func (f *fakeBackend) AliasesView() []AliasView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aliases
}

func (f *fakeBackend) BudgetsView() []routing.BudgetStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budgets
}

func (f *fakeBackend) HealthView() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health
}

func (f *fakeBackend) RecentRequests(n int) []model.RequestMeta {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRequestsN = n
	return f.reqs
}

func (f *fakeBackend) RecentRollups(n int) []store.Rollup {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRollupsN = n
	return f.rollups
}

func (f *fakeBackend) RecentAudit(n int) []store.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuditN = n
	return f.audit
}

func (f *fakeBackend) CacheStats() (hit, miss, evict, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hit, f.miss, f.evict, f.size
}

func (f *fakeBackend) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadCalls++
	return f.reloadErr
}

func (f *fakeBackend) Version() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return testVersion
}

func (f *fakeBackend) ConfigPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return testConfigPath
}

func (f *fakeBackend) reloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloadCalls
}

func (f *fakeBackend) requestsLimit() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequestsN
}

func (f *fakeBackend) rollupsLimit() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRollupsN
}

func (f *fakeBackend) auditLimit() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuditN
}

// populatedFake returns a fakeBackend whose fixed reference time is
// time.Unix(1700000000, 0).UTC(). The plaintext testSecret is deliberately
// kept out of every view.
func populatedFake() *fakeBackend {
	return &fakeBackend{
		snap: observ.Snapshot{
			Enabled:          true,
			UptimeSeconds:    1234,
			Total:            42,
			Errors:           2,
			Cached:           7,
			PromptTokens:     900,
			CompletionTokens: 300,
			CostMicros:       15,
			LatencyP50Ms:     120,
			LatencyP95Ms:     890,
			ByProvider: map[string]observ.ProviderStat{
				"prov-a": {Requests: 30, Errors: 1, CostMicros: 10},
			},
		},
		providers: []ProviderView{{
			Name:    "prov-a",
			BaseURL: "https://api.prov-a.test/v1",
			RPM:     60,
			Health:  "ok",
			Keys: []KeyView{{
				ID:           "key-1",
				Ref:          "keystore:key-1",
				RPM:          60,
				Health:       "ok",
				RPMRemaining: 59,
				Inflight:     1,
				MaxInflight:  8,
				CostCents:    100,
				CostLimit:    5000,
			}},
		}},
		aliases: []AliasView{{
			Name:    "main",
			Targets: []TargetView{{Provider: "prov-a", Model: "gpt-x"}},
		}},
		budgets: []routing.BudgetStatus{{
			Provider:     "prov-a",
			Key:          "key-1",
			RPMRemaining: 59,
			Inflight:     1,
			MaxInflight:  8,
			CostCents:    100,
			CostLimit:    5000,
		}},
		health: map[string]string{"prov-a/key-1": "ok"},
		reqs: []model.RequestMeta{{
			ID:               "req-1",
			StartedAt:        time.Unix(1700000000, 0).UTC(),
			DurMillis:        250,
			Provider:         "prov-a",
			Key:              "key-1",
			Model:            "main",
			UpstreamModel:    "gpt-x",
			Stream:           true,
			Cached:           false,
			Status:           "ok",
			RouteReason:      "alias",
			PromptTokens:     10,
			CompletionTokens: 5,
			CostMicros:       3,
			Err:              "",
		}},
		rollups: []store.Rollup{{
			Bucket:      "2026-09-23T00",
			BucketStart: time.Unix(1700000000, 0).UTC(),
			Provider:    "prov-a",
			Key:         "key-1",
			Requests:    9,
			Errors:      1,
			Prompt:      900,
			Completion:  300,
			CostMicros:  15,
		}},
		audit: []store.AuditEntry{{
			TS:     time.Unix(1700000000, 0).UTC(),
			Action: "config.reload",
			Entity: "gateway",
			Detail: "ok",
		}},
		hit: 10, miss: 4, evict: 1, size: 7,
	}
}

// unixClient returns an http.Client that dials the admin server through its
// Unix socket, exercising the real Serve path.
func unixClient(t *testing.T, sockPath string) *http.Client {
	t.Helper()
	c := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 2 * time.Second}
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// waitReady polls /health until the server accepts connections.
func waitReady(t *testing.T, c *http.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := c.Get("http://admin/health")
		if err == nil {
			resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admin server never became ready on unix socket: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startAdmin serves a Server over a real Unix socket in a temp dir and
// cancels it at cleanup, verifying Serve unwinds with http.ErrServerClosed.
func startAdmin(t *testing.T, b Backend) (*Server, *http.Client) {
	t.Helper()
	s := New(b, filepath.Join(t.TempDir(), "admin.sock"))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx) }()
	c := unixClient(t, s.path)
	waitReady(t, c)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve returned %v; want http.ErrServerClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of ctx cancel")
		}
	})
	return s, c
}

func doJSON(t *testing.T, c *http.Client, method, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://admin"+path, nil)
	if err != nil {
		t.Fatalf("building request %s %s: %v", method, path, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body of %s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp, body
}

func decodeBody(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decoding response body: %v; body=%s", err, body)
	}
}

func wantStatus(t *testing.T, resp *http.Response, status int) {
	t.Helper()
	if resp.StatusCode != status {
		t.Errorf("status = %d; want %d", resp.StatusCode, status)
	}
}

func wantJSONContentType(t *testing.T, resp *http.Response) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
}

func decodeErrBody(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]string
	decodeBody(t, body, &m)
	return m["error"]
}

// TestAdminEndpoints hits every GET route over a real Unix socket and asserts
// the decoded JSON shape, not raw strings.
func TestAdminEndpoints(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	tests := []struct {
		name   string
		method string
		path   string
		want   func(t *testing.T, resp *http.Response, body []byte)
	}{
		{
			name: "health", method: http.MethodGet, path: "/health",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				wantJSONContentType(t, resp)
				var m map[string]string
				decodeBody(t, body, &m)
				if m["status"] != "ok" {
					t.Errorf("status = %q; want ok", m["status"])
				}
				if m["version"] != testVersion {
					t.Errorf("version = %q; want %q", m["version"], testVersion)
				}
			},
		},
		{
			name: "metrics", method: http.MethodGet, path: "/metrics",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var snap observ.Snapshot
				decodeBody(t, body, &snap)
				if !snap.Enabled {
					t.Error("enabled = false; want true")
				}
				if snap.Total != 42 || snap.Errors != 2 || snap.Cached != 7 {
					t.Errorf("totals = (%d, %d, %d); want (42, 2, 7)", snap.Total, snap.Errors, snap.Cached)
				}
				if snap.LatencyP50Ms != 120 || snap.LatencyP95Ms != 890 {
					t.Errorf("latency = (%d, %d); want (120, 890)", snap.LatencyP50Ms, snap.LatencyP95Ms)
				}
				st, ok := snap.ByProvider["prov-a"]
				if !ok {
					t.Fatalf("by_provider missing prov-a; got %v", snap.ByProvider)
				}
				if st.Requests != 30 || st.Errors != 1 || st.CostMicros != 10 {
					t.Errorf("prov-a stats = %+v; want requests 30, errors 1, cost 10", st)
				}
			},
		},
		{
			name: "providers", method: http.MethodGet, path: "/providers",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var provs []ProviderView
				decodeBody(t, body, &provs)
				if len(provs) != 1 {
					t.Fatalf("got %d providers; want 1", len(provs))
				}
				p := provs[0]
				if p.Name != "prov-a" || p.BaseURL != "https://api.prov-a.test/v1" || p.RPM != 60 || p.Health != "ok" {
					t.Errorf("provider = %+v", p)
				}
				if len(p.Keys) != 1 {
					t.Fatalf("got %d keys; want 1", len(p.Keys))
				}
				k := p.Keys[0]
				if k.ID != "key-1" || k.Ref != "keystore:key-1" || k.RPMRemaining != 59 || k.MaxInflight != 8 {
					t.Errorf("key = %+v", k)
				}
			},
		},
		{
			name: "aliases", method: http.MethodGet, path: "/aliases",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var aliases []AliasView
				decodeBody(t, body, &aliases)
				if len(aliases) != 1 {
					t.Fatalf("got %d aliases; want 1", len(aliases))
				}
				a := aliases[0]
				if a.Name != "main" {
					t.Errorf("alias name = %q; want main", a.Name)
				}
				if len(a.Targets) != 1 || a.Targets[0].Provider != "prov-a" || a.Targets[0].Model != "gpt-x" {
					t.Errorf("targets = %+v", a.Targets)
				}
			},
		},
		{
			name: "budgets", method: http.MethodGet, path: "/budgets",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var budgets []routing.BudgetStatus
				decodeBody(t, body, &budgets)
				if len(budgets) != 1 {
					t.Fatalf("got %d budgets; want 1", len(budgets))
				}
				b := budgets[0]
				if b.Provider != "prov-a" || b.Key != "key-1" || b.RPMRemaining != 59 || b.CostLimit != 5000 {
					t.Errorf("budget = %+v", b)
				}
			},
		},
		{
			name: "health-states", method: http.MethodGet, path: "/health-states",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var m map[string]string
				decodeBody(t, body, &m)
				if m["prov-a/key-1"] != "ok" {
					t.Errorf("health state = %v; want prov-a/key-1=ok", m)
				}
			},
		},
		{
			name: "logs with limit", method: http.MethodGet, path: "/logs?limit=3",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var logs []model.RequestMeta
				decodeBody(t, body, &logs)
				if len(logs) != 1 {
					t.Fatalf("got %d logs; want 1", len(logs))
				}
				l := logs[0]
				if l.ID != "req-1" || l.Provider != "prov-a" || l.Status != "ok" || l.CostMicros != 3 {
					t.Errorf("log = %+v", l)
				}
				if !l.StartedAt.Equal(time.Unix(1700000000, 0).UTC()) {
					t.Errorf("started_at = %v", l.StartedAt)
				}
				if n := f.requestsLimit(); n != 3 {
					t.Errorf("backend asked for %d logs; want 3", n)
				}
			},
		},
		{
			name: "rollups with limit", method: http.MethodGet, path: "/rollups?limit=7",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var rollups []store.Rollup
				decodeBody(t, body, &rollups)
				if len(rollups) != 1 {
					t.Fatalf("got %d rollups; want 1", len(rollups))
				}
				r := rollups[0]
				if r.Bucket != "2026-09-23T00" || r.Requests != 9 || r.CostMicros != 15 {
					t.Errorf("rollup = %+v", r)
				}
				if n := f.rollupsLimit(); n != 7 {
					t.Errorf("backend asked for %d rollups; want 7", n)
				}
			},
		},
		{
			name: "audit with limit", method: http.MethodGet, path: "/audit?limit=5",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var audit []store.AuditEntry
				decodeBody(t, body, &audit)
				if len(audit) != 1 {
					t.Fatalf("got %d audit entries; want 1", len(audit))
				}
				e := audit[0]
				if e.Action != "config.reload" || e.Entity != "gateway" || e.Detail != "ok" {
					t.Errorf("audit entry = %+v", e)
				}
				if n := f.auditLimit(); n != 5 {
					t.Errorf("backend asked for %d audit entries; want 5", n)
				}
			},
		},
		{
			name: "cache", method: http.MethodGet, path: "/cache",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var m struct {
					Hit   int64 `json:"hit"`
					Miss  int64 `json:"miss"`
					Evict int64 `json:"evict"`
					Size  int64 `json:"size"`
				}
				decodeBody(t, body, &m)
				if m.Hit != 10 || m.Miss != 4 || m.Evict != 1 || m.Size != 7 {
					t.Errorf("cache = %+v; want hit 10, miss 4, evict 1, size 7", m)
				}
			},
		},
		{
			name: "config", method: http.MethodGet, path: "/config",
			want: func(t *testing.T, resp *http.Response, body []byte) {
				wantStatus(t, resp, http.StatusOK)
				var m struct {
					Path    string `json:"path"`
					Version string `json:"version"`
				}
				decodeBody(t, body, &m)
				if m.Path != testConfigPath {
					t.Errorf("path = %q; want %q", m.Path, testConfigPath)
				}
				if m.Version != testVersion {
					t.Errorf("version = %q; want %q", m.Version, testVersion)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := doJSON(t, c, tt.method, tt.path)
			tt.want(t, resp, body)
		})
	}
}

// TestMetricsDisabled verifies the enabled flag carried on observ.Snapshot is
// passed through faithfully when metrics are off.
func TestMetricsDisabled(t *testing.T) {
	f := populatedFake()
	f.snap.Enabled = false
	f.snap.Total = 0
	_, c := startAdmin(t, f)

	resp, body := doJSON(t, c, http.MethodGet, "/metrics")
	wantStatus(t, resp, http.StatusOK)
	var snap observ.Snapshot
	decodeBody(t, body, &snap)
	if snap.Enabled {
		t.Error("enabled = true; want false")
	}
	if snap.Total != 0 {
		t.Errorf("total = %d; want 0", snap.Total)
	}
}

// TestLimitParam pins the limit fallback rules: missing, non-numeric, zero,
// and negative limits all fall back to the per-route default; only positive
// integers pass through.
func TestLimitParam(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	tests := []struct {
		name  string
		path  string
		limit func() int
		want  int
	}{
		{"logs default", "/logs", f.requestsLimit, 100},
		{"logs explicit", "/logs?limit=3", f.requestsLimit, 3},
		{"logs non-numeric", "/logs?limit=abc", f.requestsLimit, 100},
		{"logs zero", "/logs?limit=0", f.requestsLimit, 100},
		{"logs negative", "/logs?limit=-4", f.requestsLimit, 100},
		{"rollups default", "/rollups", f.rollupsLimit, 200},
		{"rollups explicit", "/rollups?limit=7", f.rollupsLimit, 7},
		{"rollups non-numeric", "/rollups?limit=zz", f.rollupsLimit, 200},
		{"audit default", "/audit", f.auditLimit, 100},
		{"audit explicit", "/audit?limit=5", f.auditLimit, 5},
		{"audit zero", "/audit?limit=0", f.auditLimit, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _ := doJSON(t, c, http.MethodGet, tt.path)
			wantStatus(t, resp, http.StatusOK)
			if got := tt.limit(); got != tt.want {
				t.Errorf("backend received limit %d; want %d", got, tt.want)
			}
		})
	}
}

// TestReload verifies the method guard, the exactly-once invocation per
// request, and the success body.
func TestReload(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	resp, body := doJSON(t, c, http.MethodGet, "/reload")
	wantStatus(t, resp, http.StatusMethodNotAllowed)
	if msg := decodeErrBody(t, body); msg != "POST required" {
		t.Errorf("error = %q; want POST required", msg)
	}
	if n := f.reloadCount(); n != 0 {
		t.Errorf("reload called %d times on GET; want 0", n)
	}

	resp, body = doJSON(t, c, http.MethodPost, "/reload")
	wantStatus(t, resp, http.StatusOK)
	var ok struct {
		OK bool `json:"ok"`
	}
	decodeBody(t, body, &ok)
	if !ok.OK {
		t.Error("ok = false; want true")
	}
	if n := f.reloadCount(); n != 1 {
		t.Errorf("reload called %d times after one POST; want 1", n)
	}

	_, _ = doJSON(t, c, http.MethodPost, "/reload")
	if n := f.reloadCount(); n != 2 {
		t.Errorf("reload called %d times after two POSTs; want 2", n)
	}
}

// TestReloadError verifies the backend-error path returns 500 with a JSON
// error body carrying the backend message.
func TestReloadError(t *testing.T) {
	f := populatedFake()
	f.reloadErr = errors.New("boom")
	_, c := startAdmin(t, f)

	resp, body := doJSON(t, c, http.MethodPost, "/reload")
	wantStatus(t, resp, http.StatusInternalServerError)
	if msg := decodeErrBody(t, body); msg != "boom" {
		t.Errorf("error = %q; want boom", msg)
	}
	if n := f.reloadCount(); n != 1 {
		t.Errorf("reload called %d times; want 1", n)
	}
}

// TestUnknownRoute pins the mux default: 404 with the net/http plain-text
// body, not a JSON error envelope.
func TestUnknownRoute(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	resp, _ := doJSON(t, c, http.MethodGet, "/no-such-endpoint")
	wantStatus(t, resp, http.StatusNotFound)
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want non-JSON net/http default", ct)
	}
}

// TestSecretNotExposed sweeps every GET endpoint and asserts the fake's
// plaintext secret appears nowhere in any response body. It additionally pins
// the exact JSON key sets of provider and key objects, so a future field that
// could carry a secret would surface as an unexpected key.
func TestSecretNotExposed(t *testing.T) {
	f := populatedFake()
	_, c := startAdmin(t, f)

	paths := []string{
		"/health", "/metrics", "/providers", "/aliases", "/budgets",
		"/health-states", "/logs", "/rollups", "/audit", "/cache", "/config",
	}
	for _, path := range paths {
		_, body := doJSON(t, c, http.MethodGet, path)
		if strings.Contains(string(body), testSecret) {
			t.Errorf("%s: response body leaks the secret", path)
		}
	}

	_, body := doJSON(t, c, http.MethodGet, "/providers")
	var provs []ProviderView
	decodeBody(t, body, &provs)
	if len(provs) != 1 || len(provs[0].Keys) != 1 {
		t.Fatalf("unexpected providers payload: %s", body)
	}
	if ref := provs[0].Keys[0].Ref; ref != "keystore:key-1" {
		t.Errorf("ref = %q; want keystore:key-1 (the ref is exposed, the secret is not)", ref)
	}

	var raw []map[string]any
	decodeBody(t, body, &raw)
	if len(raw) != 1 {
		t.Fatalf("got %d provider objects; want 1", len(raw))
	}
	if got := len(raw[0]); got != 5 {
		t.Errorf("provider object has %d keys; want exactly 5 (name, base_url, rpm, health, keys)", got)
	}
	keys, ok := raw[0]["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("keys payload unexpected: %s", body)
	}
	keyObj, ok := keys[0].(map[string]any)
	if !ok {
		t.Fatalf("key object unexpected: %s", body)
	}
	if got := len(keyObj); got != 9 {
		t.Errorf("key object has %d keys; want exactly 9 (id, ref, rpm, health, rpm_remaining, inflight, max_inflight, cost_cents, cost_limit_cents)", got)
	}
}

// TestServeClose covers the full lifecycle: a socket path in a not-yet-existing
// nested directory is created, the server answers, ctx cancel unwinds Serve
// with http.ErrServerClosed, and Close is safe afterwards.
func TestServeClose(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "run", "admin.sock")
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket should not exist before Serve: %v", err)
	}

	s := New(populatedFake(), sockPath)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx) }()

	c := unixClient(t, sockPath)
	waitReady(t, c)

	if fi, err := os.Stat(sockPath); err != nil {
		t.Errorf("socket missing after Serve: %v", err)
	} else if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket path is not a socket: %v", fi.Mode())
	}

	resp, body := doJSON(t, c, http.MethodGet, "/providers")
	wantStatus(t, resp, http.StatusOK)
	var provs []ProviderView
	decodeBody(t, body, &provs)
	if len(provs) != 1 {
		t.Fatalf("got %d providers; want 1", len(provs))
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v; want http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of ctx cancel")
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close after shutdown = %v; want nil", err)
	}

	if resp, err := c.Get("http://admin/health"); err == nil {
		resp.Body.Close()
		t.Error("request succeeded after shutdown; want dial error")
	}
}
