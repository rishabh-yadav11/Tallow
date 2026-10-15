package app_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

const rpmCapCfg = `version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 64
queue_timeout = "5s"
sticky_ttl = "5s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = false

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

func startGatewayWithProviderRPM(t *testing.T, upstreamURL string, providerRPM int) (*app.App, string, string) {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.NewBox([]byte("integration-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	ksPath := filepath.Join(dir, "keys.json")
	ks, err := secret.LoadStore(ksPath, box)
	if err != nil {
		t.Fatal(err)
	}

	for ref, val := range map[string]string{
		"p1:k1": "sk-fake-key-one",
		"p1:k2": "sk-fake-key-two",
	} {
		if err := ks.Set(ref, val); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fmt.Sprintf(rpmCapCfg,
		filepath.Join(dir, "admin.sock"),
		ksPath,
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
	return a, srv.URL, ksPath
}

func postWithSession(t *testing.T, url, session, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type credentialUpstream struct {
	*fakeUpstream
	credsMu sync.Mutex
	creds   []string
}

func (c *credentialUpstream) handler(w http.ResponseWriter, r *http.Request) {
	c.credsMu.Lock()
	c.creds = append(c.creds, r.Header.Get("Authorization"))
	c.credsMu.Unlock()
	c.fakeUpstream.handler(w, r)
}

func (c *credentialUpstream) seenCredentials() []string {
	c.credsMu.Lock()
	defer c.credsMu.Unlock()
	return append([]string(nil), c.creds...)
}

func setupCredentialUpstream(t *testing.T) (*httptest.Server, *credentialUpstream) {
	t.Helper()
	up := &credentialUpstream{fakeUpstream: &fakeUpstream{}}
	up.srv = httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(up.srv.Close)
	return up.srv, up
}

func distinct(creds []string) map[string]int {
	m := make(map[string]int, len(creds))
	for _, c := range creds {
		c = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c), "Bearer "))
		if c == "" {
			continue
		}
		m[c]++
	}
	return m
}

func TestProviderRPMCapIsSharedAcrossKeys(t *testing.T) {
	upSrv, up := setupCredentialUpstream(t)
	const cap = 4
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	served, limited := 0, 0
	for i := 0; i < cap*3; i++ {
		body := fmt.Sprintf(`{"model":"flash","input":"req-%d"}`, i)
		resp, got := post(t, gw+"/v1/responses", body)
		switch resp.StatusCode {
		case http.StatusOK:
			if !strings.Contains(got, "pong") {
				t.Fatalf("request %d: unexpected body %s", i, got)
			}
			served++
		case http.StatusTooManyRequests:
			limited++

			if !strings.Contains(got, "rate_limit_error") {
				t.Errorf("429 body is not typed as a rate limit: %s", got)
			}

			if !strings.Contains(got, "rpm") {
				t.Errorf("429 body does not name the provider RPM cap: %s. A "+
					"saturated admission queue produces a 429 with a different "+
					"message, so this should never be the limiter under test",
					got)
			}

			ra := resp.Header.Get("Retry-After")
			if ra == "" {
				t.Errorf("429 has no Retry-After header, so the client cannot know " +
					"when to come back")
			} else {
				secs, err := strconv.Atoi(ra)
				switch {
				case err != nil:
					t.Errorf("Retry-After %q is not an integer number of seconds: %v", ra, err)
				case secs < 1:

					t.Errorf("Retry-After is %d, want at least 1 second", secs)
				case secs > 60:

					t.Errorf("Retry-After is %d seconds, longer than the 60s provider "+
						"RPM window it is derived from", secs)
				}

				if err == nil && secs < 59 {
					t.Errorf("Retry-After is %d seconds, under-reporting the 60s "+
						"provider RPM window: the client would return before the "+
						"window rolls and be limited again", secs)
				}
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

	if int(up.hits.Load()) != served {
		t.Errorf("the upstream saw %d requests but %d were counted as served: "+
			"the accounting and the actual traffic disagree", up.hits.Load(), served)
	}
}

func TestProviderRPMCapHoldsAcrossTwoKeysExplicitly(t *testing.T) {
	upSrv, up := setupCredentialUpstream(t)
	const cap = 4
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	served := 0
	for i := 0; i < cap+2; i++ {
		body := fmt.Sprintf(`{"model":"flash","input":"split-%d"}`, i)
		resp := postWithSession(t, gw+"/v1/responses", fmt.Sprintf("split-session-%d", i), body)
		if resp.StatusCode == http.StatusOK {
			served++
		}
	}
	if served > cap {
		t.Errorf("served %d requests with a shared provider cap of %d", served, cap)
	}
	if served == 0 {
		t.Fatal("no request was served: a cap assertion would pass vacuously")
	}

	creds := distinct(up.seenCredentials())
	if len(creds) < 2 {
		t.Errorf("the provider cap was spent using %d distinct credential(s) "+
			"(%v); the aggregate cap was never actually shared across the "+
			"provider's two keys, so this test does not establish sharing",
			len(creds), creds)
	}
}

func TestProviderRPMCapChargesEachRequestOnce(t *testing.T) {
	upSrv, up := setupCredentialUpstream(t)
	const cap = 3
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	served, limited := 0, 0
	for i := 0; i < cap*4; i++ {
		body := fmt.Sprintf(`{"model":"flash","input":"acct-%d"}`, i)

		resp := postWithSession(t, gw+"/v1/responses", fmt.Sprintf("acct-session-%d", i), body)
		switch resp.StatusCode {
		case http.StatusOK:
			served++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("request %d: unexpected status %d", i, resp.StatusCode)
		}
	}

	if got := int(up.hits.Load()); got > cap {
		t.Errorf("the upstream received %d requests under a provider cap of %d: "+
			"the cap is not bounding real provider traffic", got, cap)
	}
	if got := int(up.hits.Load()); got != served {
		t.Errorf("the upstream saw %d requests but the gateway counted %d served: "+
			"the RPM accounting and the actual traffic disagree", got, served)
	}
	if limited == 0 {
		t.Error("no request was rate limited: the test never reached the cap, so " +
			"it proves nothing about enforcement")
	}
}

func TestRetryAfterShrinksAsTheWindowAges(t *testing.T) {
	upSrv, _ := setup(t)
	const cap = 1
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	send := func(session string) *http.Response {
		t.Helper()
		body := fmt.Sprintf(`{"model":"flash","input":"%s"}`, session)
		return postWithSession(t, gw+"/v1/responses", session, body)
	}

	if resp := send("seed"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", resp.StatusCode)
	}

	time.Sleep(3500 * time.Millisecond)

	resp := send("after-ageing")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after ageing: status %d, want 429", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("no Retry-After on the aged 429")
	}
	secs, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After %q is not an integer: %v", ra, err)
	}

	if secs != 57 {
		t.Errorf("Retry-After is %d seconds after ageing a 60s window by 3.5s, "+
			"want exactly 57: the wait is at least 56.49s, so 56 means the value "+
			"was truncated and the client returns before the window rolls, and any "+
			"other value means the gateway is misreporting its own window",
			secs)
	}
}

func TestBudgetViewRemainsCoherentWhenACapIsHit(t *testing.T) {
	upSrv, _ := setup(t)
	const cap = 2
	a, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	for i := 0; i < cap+2; i++ {
		body := fmt.Sprintf(`{"model":"flash","input":"edge-%d"}`, i)
		post(t, gw+"/v1/responses", body)
	}

	views := a.BudgetsView()
	if len(views) == 0 {
		t.Fatal("BudgetsView returned nothing after the cap was hit")
	}

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

	for i := 1; i < len(first); i++ {
		prev, cur := first[i-1], first[i]
		if prev.Provider > cur.Provider ||
			(prev.Provider == cur.Provider && prev.Key > cur.Key) {
			t.Errorf("BudgetsView is not sorted by provider then key: %s/%s "+
				"comes before %s/%s", prev.Provider, prev.Key, cur.Provider, cur.Key)
		}
	}
}
