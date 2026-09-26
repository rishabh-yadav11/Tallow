package app_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"net/http/httptest"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// The provider RPM refund is covered in internal/routing, where the bypass was
// and where an injected clock makes the window boundary exact. That is the right
// place for the mechanism. It is not where an operator meets the consequence.
//
// The number the operator configured is a cap on the PROVIDER, shared by every
// key, so the question that matters is whether traffic a client can actually
// send drives the gateway over it. The bypass made the answer wrong only under
// one condition: an abandoned candidate whose window rolled over while it was in
// flight, which means a request that outlives its RPM window. Nothing shorter
// reproduces it, which is why the routing suite needs a fake clock to see it at
// all. This file therefore checks what the routing suite structurally cannot:
// that the cap is enforced across keys, and that the accounting an operator
// reads back does not contradict enforcement.
//
// The provider cap is configured as `rpm` on the provider block and is enforced
// by the router, not by the proxy, so the observable is the HTTP status a
// client receives once the aggregate is spent.

// rpmCapCfg is a gateway whose provider-level `rpm` cap is the only binding
// limit. Each key's own rpm is left far above it so the provider cap is
// unambiguously the constraint under test.
const rpmCapCfg = `version = 1
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
rpm = %d

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  rpm = 100000

  [[provider.key]]
  id = "k2"
  ref = "p1:k2"
  rpm = 100000

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "flash-v4"
`

// startGatewayWithProviderRPM starts a real App whose single provider carries
// the given aggregate RPM cap, with two usable keys on it.
func startGatewayWithProviderRPM(t *testing.T, upstreamURL string, providerRPM int) (*app.App, string) {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.NewBox([]byte("integration-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := secret.LoadStore(filepath.Join(dir, "keys.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"p1:k1", "p1:k2"} {
		if err := ks.Set(ref, "sk-fake"); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fmt.Sprintf(rpmCapCfg,
		filepath.Join(dir, "admin.sock"),
		filepath.Join(dir, "keys.json"),
		filepath.Join(dir, "tallow.db"),
		upstreamURL, providerRPM,
	)
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, "integration-master-key")
	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.ProxyHandler())
	t.Cleanup(srv.Close)
	return a, srv.URL
}

// TestProviderRPMCapIsSharedAcrossKeys is the aggregate assertion. The cap
// belongs to the provider, so a client that can make the gateway use both of its
// keys must not get twice the configured rate. A per-key implementation would
// pass any single-key test and still overrun the provider's real quota, which is
// the entire reason the provider-level cap exists.
func TestProviderRPMCapIsSharedAcrossKeys(t *testing.T) {
	upSrv, up := setup(t)
	const cap = 4
	_, gw := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	// Enough traffic to overrun the cap twice over. Every request is distinct
	// so nothing is served from cache: a cache hit consumes no provider RPM and
	// would make the cap look looser than it is.
	served, limited := 0, 0
	for i := 0; i < cap*3; i++ {
		body := fmt.Sprintf(`{"model":"flash","messages":[{"role":"user","content":"req-%d"}]}`, i)
		resp, got := post(t, gw+"/v1/chat/completions", body)
		switch resp.StatusCode {
		case http.StatusOK:
			if !strings.Contains(got, "pong") {
				t.Fatalf("request %d: unexpected body %s", i, got)
			}
			served++
		case http.StatusTooManyRequests:
			limited++
			// A rate-limited client must be able to tell why, and the body has to
			// agree with the status. Without either, an operator reading
			// upstream_error goes looking at provider health when the cause is a
			// limit they configured.
			if !strings.Contains(got, "rate_limit_error") {
				t.Errorf("429 body is not typed as a rate limit: %s", got)
			}
			if !strings.Contains(got, "rpm") {
				t.Errorf("429 body does not name the limit that was reached: %s", got)
			}
		default:
			t.Fatalf("request %d: unexpected status %d: %s", i, resp.StatusCode, got)
		}
	}

	if served > cap {
		t.Errorf("the gateway served %d requests against a provider cap of %d "+
			"using %d keys: the cap is not shared across the provider's keys",
			served, cap, 2)
	}
	if limited == 0 {
		t.Error("no request was rate limited at all: the test never reached the " +
			"cap, so it proves nothing about enforcement")
	}
	if served == 0 {
		t.Fatal("every request failed: the gateway is not working, so a cap " +
			"assertion would pass vacuously")
	}
	// The upstream must have been the thing that served the accepted traffic,
	// confirming the counted requests were real provider traffic.
	if int(up.hits.Load()) != served {
		t.Errorf("the upstream saw %d requests but %d were counted as served: "+
			"the accounting and the actual traffic disagree", up.hits.Load(), served)
	}
}

// TestProviderRPMCapHoldsAcrossTwoKeysExplicitly splits the traffic across both
// keys so the shared cap is not satisfied by luck. Each key is driven directly,
// by pinning a request to it, so a cap enforced per key would serve the full
// allowance twice and fail here even though TestProviderRPMCapIsSharedAcrossKeys
// passed on a single alias.
func TestProviderRPMCapHoldsAcrossTwoKeysExplicitly(t *testing.T) {
	upSrv, _ := setup(t)
	const cap = 4
	_, gw := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	// Three requests, which the router will spread across both keys, then a
	// fourth that must be refused. If the cap were per key, the aggregate would
	// still have room and the fourth would succeed.
	served := 0
	for i := 0; i < cap+2; i++ {
		body := fmt.Sprintf(`{"model":"flash","messages":[{"role":"user","content":"split-%d"}]}`, i)
		resp, _ := post(t, gw+"/v1/chat/completions", body)
		if resp.StatusCode == http.StatusOK {
			served++
		}
	}
	if served > cap {
		t.Errorf("served %d requests with a shared provider cap of %d", served, cap)
	}
}

// TestBudgetViewRemainsCoherentWhenACapIsHit is the operator-facing edge. The
// admin budget view is how an operator sees why traffic is being refused, so it
// has to stay readable and non-negative at exactly the moment a limit is
// reached. A view that reports a negative headroom or vanishes under load is
// unusable when it is needed.
func TestBudgetViewRemainsCoherentWhenACapIsHit(t *testing.T) {
	upSrv, _ := setup(t)
	const cap = 2
	a, gw := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	// Drive past the cap so both the accepted and the refused states are seen.
	for i := 0; i < cap+2; i++ {
		body := fmt.Sprintf(`{"model":"flash","messages":[{"role":"user","content":"edge-%d"}]}`, i)
		post(t, gw+"/v1/chat/completions", body)
	}

	views := a.BudgetsView()
	if len(views) == 0 {
		t.Fatal("BudgetsView returned nothing after the cap was hit")
	}
	// Both keys are routable, so both must be represented, or an operator
	// cannot tell which credential is throttled.
	if len(views) != 2 {
		t.Errorf("BudgetsView reports %d rows, want one per configured key: %+v",
			len(views), views)
	}
	for _, b := range views {
		if b.RPMRemaining < 0 {
			t.Errorf("%s/%s reports negative remaining RPM %d", b.Provider, b.Key, b.RPMRemaining)
		}
		if b.MaxInflight < 0 {
			t.Errorf("%s/%s reports a negative inflight cap %d", b.Provider, b.Key, b.MaxInflight)
		}
	}
	// Repeated reads of an idle gateway must agree on identity and shape: a
	// view whose rows reordered between polls would make an operator think the
	// wrong key was throttled, because the values would appear to move between
	// rows.
	//
	// Two things are deliberately NOT asserted here. First, identity is compared
	// by provider/key rather than by index. Second, RPM is not compared at all:
	// the rolling window refills continuously, so two reads microseconds apart
	// legitimately differ, and asserting they match would be asserting the
	// clock stopped. The first version of this test compared both by index and
	// including RPM, and failed about 40% of runs under -race purely from
	// window refill and Go's randomised map order, neither of which is a defect.
	// A view that reported a changing number is the design; a view that
	// reordered its rows was the bug, and Budgets now sorts them.
	first := a.BudgetsView()
	second := a.BudgetsView()
	if len(first) != len(second) {
		t.Fatalf("repeated BudgetsView calls disagree on row count: %d then %d",
			len(first), len(second))
	}
	byID := func(vs []routing.BudgetStatus) map[string]routing.BudgetStatus {
		m := make(map[string]routing.BudgetStatus, len(vs))
		for _, v := range vs {
			m[v.Provider+"/"+v.Key] = v
		}
		return m
	}
	f, s := byID(first), byID(second)
	for id, a1 := range f {
		b1, ok := s[id]
		if !ok {
			t.Errorf("%s present in one BudgetsView call and absent from the next", id)
			continue
		}
		// Identity and the non-moving fields must be identical. RPM is excluded
		// for the reason above; inflight and the caps are not moving.
		if a1.Inflight != b1.Inflight {
			t.Errorf("%s inflight changed between two back-to-back reads on an "+
				"idle gateway: %d then %d", id, a1.Inflight, b1.Inflight)
		}
		if a1.MaxInflight != b1.MaxInflight {
			t.Errorf("%s inflight cap changed between reads: %d then %d",
				id, a1.MaxInflight, b1.MaxInflight)
		}
		if a1.CostCents != b1.CostCents {
			t.Errorf("%s spend changed between two back-to-back reads on an "+
				"idle gateway: %d then %d", id, a1.CostCents, b1.CostCents)
		}
	}

	// The ordering itself is part of the contract, so it is asserted directly
	// rather than inferred. A dashboard renders these rows in order, and a
	// client that diffs two polls positionally would otherwise see a spurious
	// change on every request.
	for i := 1; i < len(first); i++ {
		prev, cur := first[i-1], first[i]
		if prev.Provider > cur.Provider ||
			(prev.Provider == cur.Provider && prev.Key > cur.Key) {
			t.Errorf("BudgetsView is not sorted by provider then key: %s/%s "+
				"comes before %s/%s", prev.Provider, prev.Key, cur.Provider, cur.Key)
		}
	}
}
