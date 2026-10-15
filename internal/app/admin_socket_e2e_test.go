package app_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

func TestAdminReloadRejectsNonPostMethodsOverTheSocket(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	for _, method := range []string{http.MethodGet, http.MethodPut,
		http.MethodPatch, http.MethodDelete} {
		code, body := adminRequest(t, method, g.adminSock, "/reload")
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%s /reload returned %d, want 405: a non-POST must never "+
				"trigger a reload, which re-resolves provider credentials from "+
				"the keystore on demand (%s)", method, code, body)
		}
	}

	if code, body := adminPost(t, g.adminSock, "/reload"); code != http.StatusOK {
		t.Fatalf("POST /reload = %d, want 200: the method check must not break "+
			"the endpoint (%s)", code, body)
	}
}

func seedRequestRows(t *testing.T, cfgPath string, n int) {
	t.Helper()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load the live config: %v", err)
	}
	st, err := store.Open(store.StoreConfig{Path: cfg.Store.Path})
	if err != nil {
		t.Fatalf("open the gateway's own database to seed rows: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()

	for i := 0; i < n; i++ {
		if err := st.RecordRequest(model.RequestMeta{
			ID:        "seed-" + strconv.Itoa(i),
			StartedAt: now.Add(-time.Duration(i) * time.Second),
			Provider:  "p1",
			Key:       "k1",
			Model:     "flash",
			Status:    "200",
			DurMillis: 1,
		}); err != nil {
			t.Fatalf("seed request row %d: %v", i, err)
		}
		if i%100 == 99 {
			st.Flush()
		}
	}
	st.Flush()

	got, err := st.RecentRequests(n)
	if err != nil {
		t.Fatalf("read back the seeded rows: %v", err)
	}
	if len(got) < n {
		t.Fatalf("seeded %d rows but only %d are readable: the limit assertions "+
			"in this test would be measuring a short read", n, len(got))
	}
}

func TestAdminLimitParameterIsClampedOverTheSocket(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	if code, body := g.chat(t, "limit-clamp-real"); code != http.StatusOK {
		t.Fatalf("seeding request failed: %d %s", code, body)
	}

	seedRequestRows(t, g.cfgPath, 1200)

	code, body := adminGet(t, g.adminSock, "/logs")
	if code != http.StatusOK {
		t.Fatalf("GET /logs = %d: %s", code, body)
	}
	if def := countRows(t, body); def != 100 {
		t.Errorf("GET /logs with no limit returned %d rows, want the documented "+
			"default of 100", def)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/logs?limit=0", 100},
		{"/logs?limit=-4", 100},
		{"/logs?limit=abc", 100},
		{"/logs?limit=+5", 100},
		{"/logs?limit=5.9", 100},
		{"/logs?limit=", 100},
		{"/logs?limit=99999999999999999999", 100},
	} {
		code, body := adminGet(t, g.adminSock, tc.path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", tc.path, code, body)
			continue
		}
		if n := countRows(t, body); n != tc.want {
			t.Errorf("GET %s returned %d rows, want %d", tc.path, n, tc.want)
		}
	}

	for _, huge := range []string{"100000000", "2147483647", "9999999999999999999"} {
		code, body := adminGet(t, g.adminSock, "/logs?limit="+huge)
		if code != http.StatusOK {
			t.Errorf("GET /logs?limit=%s = %d, want 200: clamping must not turn "+
				"a large limit into an error (%s)", huge, code, body)
			continue
		}
		n := countRows(t, body)
		if n > 1000 {
			t.Errorf("GET /logs?limit=%s returned %d rows: the caller's limit "+
				"reached SQL unchecked, so any process that can open the admin "+
				"socket can force the gateway to build and serialise an "+
				"arbitrarily large response", huge, n)
		}
	}

	code, body = adminGet(t, g.adminSock, "/logs?limit=1")
	if code != http.StatusOK {
		t.Fatalf("GET /logs?limit=1 = %d: %s", code, body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("GET /logs?limit=1 returned %d rows, want 1: a small valid "+
			"limit must be honoured exactly, not clamped to the maximum", n)
	}

	for _, path := range []string{"/rollups", "/audit"} {
		for _, limit := range []string{"1001", "2000", "100000000"} {
			code, body := adminGet(t, g.adminSock, path+"?limit="+limit)
			if code != http.StatusOK {
				t.Errorf("GET %s?limit=%s = %d, want 200: %s", path, limit, code, body)
				continue
			}
			if n := countRows(t, body); n > 1000 {
				t.Errorf("GET %s?limit=%s returned %d rows, want at most the "+
					"1000-row cap: the limit is unbounded here, or its clamp "+
					"fires only for values far above the cap", path, limit, n)
			}
		}
	}
}

func TestAdminLimitClampIsTransparentBelowTheCap(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	seedRequestRows(t, g.cfgPath, 1200)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/logs?limit=1", 1},
		{"/logs?limit=3", 3},
		{"/logs?limit=37", 37},
		{"/logs?limit=500", 500},
		{"/logs?limit=999", 999},
		{"/logs?limit=1000", 1000},
		{"/logs?limit=1001", 1000},
		{"/logs?limit=2000", 1000},
		{"/logs?limit=5000", 1000},
	} {
		code, body := adminGet(t, g.adminSock, tc.path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", tc.path, code, body)
			continue
		}
		if n := countRows(t, body); n != tc.want {
			t.Errorf("GET %s returned %d rows, want %d", tc.path, n, tc.want)
		}
	}
}

func countRows(t *testing.T, body []byte) int {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("admin response is not a JSON array of objects: %v (%s)", err, body)
	}
	return len(rows)
}
