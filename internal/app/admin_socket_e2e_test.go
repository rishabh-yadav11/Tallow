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

// The suite in admin_socket_acceptance_test.go already covers the control
// plane over a real socket: the route table, the 404 for unknown routes, the
// socket's file mode, /reload applying to the live gateway, a rejected reload,
// and that the gateway survives a rejected reload. Repeating those here would
// be duplicated cost with no new observation, so this file holds only what is
// not already checked anywhere.
//
// Two things are new here, and both are properties of the live gateway rather
// than of the handler:
//
//  1. The method restriction on /reload, observed from outside. A GET that
//     triggered a reload would let anything able to open the socket force a
//     re-resolution of every provider credential on demand, at a moment of its
//     choosing. The handler suite asserts the code path, but nothing asserted
//     the socket-level behaviour, which is what anything able to open the
//     socket would actually use.
//
//  2. The ?limit= bound, which found a live defect. limitParam passed the
//     caller's number straight to a SQL LIMIT, so ?limit=100000000 asked the
//     gateway to materialise and serialise a hundred million rows. The admin
//     API is local-only, but that boundary is weaker than it appears: a
//     compromised low-privilege process on the same machine can reach the
//     socket, and it could turn a read endpoint into an unbounded allocation in
//     the gateway's own address space. limitParam now clamps to maxLimitParam.
//
// A note on why the first draft of this file proved nothing, since the failure
// is instructive and easy to repeat. It drove real traffic, then asked for
// ?limit=1e8 and asserted the response parsed. That assertion is unsatisfiable
// as a detection: the test database held three requests, so every limit
// returned three rows, clamped or not, and the suite passed against a
// deliberately broken limitParam. Three mutations, including one that deleted
// the clamp outright, all survived. Asserting only that a response is
// well-formed is not evidence of a bound. The test below seeds more rows than
// the cap, so the ceiling is observable from the socket, which is where a
// caller reaches it.

// TestAdminReloadRejectsNonPostMethodsOverTheSocket checks the method
// restriction where it matters, over the real socket, and confirms POST is
// still accepted so the check is not satisfied by a route that refuses
// everything.
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

	// POST must still work, and must report success.
	if code, body := adminPost(t, g.adminSock, "/reload"); code != http.StatusOK {
		t.Fatalf("POST /reload = %d, want 200: the method check must not break "+
			"the endpoint (%s)", code, body)
	}
}

// seedRequestRows writes n rows straight into the running gateway's own
// database, through the store's public API, so the log endpoints have more rows
// than the cap allows. Writing through the store rather than by hand-rolled SQL
// keeps the rows shaped exactly as the gateway would have written them, so
// /logs is reading real data through the real query path.
//
// The gateway is already running when this is called, so this needs its own
// Store handle. SQLite in WAL mode allows a second writer, and the gateway's
// request log is append-only, so there is no contention to lose to. The write
// must go to the same file the gateway opened, which is found by reading the
// live config rather than assumed.
func seedRequestRows(t *testing.T, cfgPath string, n int) {
	t.Helper()
	// Read the live config to find the database the gateway actually opened,
	// rather than assuming a path.
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
	// RecordRequest hands the row to an async writer, so the loop flushes
	// periodically. Writing all rows before flushing overflows the bounded
	// write queue, and worse, a row that is still queued is invisible to
	// /logs, which would silently make the row-count assertions read low.
	// Every row is therefore confirmed visible before the test reads the
	// endpoint, so a seeding failure cannot masquerade as a limit bug.
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

	// Confirm the rows are actually readable now, so a seeding problem is
	// reported as such rather than as a limit failure later.
	got, err := st.RecentRequests(n)
	if err != nil {
		t.Fatalf("read back the seeded rows: %v", err)
	}
	if len(got) < n {
		t.Fatalf("seeded %d rows but only %d are readable: the limit assertions "+
			"in this test would be measuring a short read", n, len(got))
	}
}

// TestAdminLimitParameterIsClampedOverTheSocket drives the bound at the layer a
// caller actually reaches, with enough rows behind it for the ceiling to be
// visible. Without the seed, every assertion here would pass against an
// unclamped limitParam.
func TestAdminLimitParameterIsClampedOverTheSocket(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)

	// One real request through the proxy, so the log contains a row the
	// gateway genuinely wrote, not only seeded ones.
	if code, body := g.chat(t, "limit-clamp-real"); code != http.StatusOK {
		t.Fatalf("seeding request failed: %d %s", code, body)
	}
	// Then more rows than the cap, so a clamp is distinguishable from
	// "returned everything".
	seedRequestRows(t, g.cfgPath, 1200)

	// The default, which is well under the cap, must be unchanged. Exactly 100
	// rows is itself the evidence the seed landed: the test run contributes a
	// couple of rows and the seed contributes 1200, so 100 is only reachable
	// because there is far more data than the default asks for. seedRequestRows
	// also confirms the rows are readable, so this cannot pass on a short read.
	code, body := adminGet(t, g.adminSock, "/logs")
	if code != http.StatusOK {
		t.Fatalf("GET /logs = %d: %s", code, body)
	}
	if def := countRows(t, body); def != 100 {
		t.Errorf("GET /logs with no limit returned %d rows, want the documented "+
			"default of 100", def)
	}

	// The fallback values, which must all land on that same default rather than
	// on whatever the database happens to do with a nonsense limit. They are
	// asserted here, in the test that owns the seeding, because that is the
	// only place the values can be told apart: with a handful of rows in the
	// table, a mutant that passes a negative limit straight through is
	// indistinguishable from the correct code, and a mutation run confirmed
	// exactly that, the mutant surviving because the store's own <= 0 clamp
	// happened to agree with it. The store's clamp does not always agree, so
	// that mutant is not strictly equivalent: RecentRollups defaults 0 to 200
	// rather than 100, which would change what /rollups returns. That is not
	// observable from a table holding no rollups, so the parsing rule is pinned
	// directly against the one function that owns it.
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
		{"/logs?limit=99999999999999999999", 100}, // overflows int64, so unparseable
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

	// The regression itself. With 1201 rows available, an honoured 1e8 returns
	// all of them and a clamped one returns at most the cap. This is the
	// assertion that fails when the clamp is removed.
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

	// A small valid limit must be honoured exactly, not rounded up to the cap.
	// A clamp that broke parsing would pass the test above by refusing
	// everything, so the small case is what proves the clamp is a bound and not
	// a breakage.
	code, body = adminGet(t, g.adminSock, "/logs?limit=1")
	if code != http.StatusOK {
		t.Fatalf("GET /logs?limit=1 = %d: %s", code, body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("GET /logs?limit=1 returned %d rows, want 1: a small valid "+
			"limit must be honoured exactly, not clamped to the maximum", n)
	}

	// The other two endpoints each call limitParam separately, so a bound
	// applied at only one call site would pass a /logs-only test. The values
	// include one just past the cap, because a clamp that only fired for very
	// large inputs would otherwise pass a check that only asks for 1e8.
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

// TestAdminLimitClampIsTransparentBelowTheCap checks that the clamp changed
// nothing for ordinary consumers. Every value here is below the cap, so each
// must produce exactly what it produced before the fix.
func TestAdminLimitClampIsTransparentBelowTheCap(t *testing.T) {
	upSrv, _ := setup(t)
	g := startLive(t, upSrv.URL)
	// Enough rows that a value between the cap and the row count is
	// distinguishable. With only 40 rows, a mutant that clamps every parsed
	// limit *up* to maxLimitParam would be behaviourally identical here, and
	// that equivalence was confirmed by mutation: such a mutant survived. The
	// fix is more rows than the cap, so the exact value is pinned.
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
		{"/logs?limit=1000", 1000}, // exactly the cap
		{"/logs?limit=1001", 1000}, // one past the cap: must clamp down
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

// countRows decodes a JSON array of objects, failing the test if the body is
// not a well-formed array.
func countRows(t *testing.T, body []byte) int {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("admin response is not a JSON array of objects: %v (%s)", err, body)
	}
	return len(rows)
}
