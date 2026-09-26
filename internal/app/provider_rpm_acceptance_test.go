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

// startGatewayWithProviderRPM starts a real App whose single provider carries
// the given aggregate RPM cap, with two usable keys on it.
//
// The queue is sized so the CONCURRENCY limiter is never what refuses a
// request. These tests are sequential, so the queue is never under pressure, and
// the 429 they observe is unambiguously the provider RPM cap. That distinction
// matters: a saturated queue also answers 429 rate_limit_error, so a test that
// accidentally tripped the queue would see the same status and could not tell
// the two limits apart. The body check below is what confirms the cause.
//
// The two keys get DISTINCT secrets. Storing the same value under both refs would
// mean the suite could not tell whether the router actually spread the traffic
// or used one credential twice, which is precisely the property the shared-cap
// test exists to check. Returning the keystore path lets a test read the secrets
// back for comparison against what the upstream was actually sent.
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
	// Distinct per key, so a credential observed upstream identifies which key
	// served it.
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

// postWithSession posts with an explicit X-Session-Id.
//
// It exists because the gateway's routing is sticky: absent a session header, the
// sticky key is derived from the client bearer token and the alias, and every
// request in these tests shares both, so ALL of them pin to whichever key was
// picked first. Sending one request per distinct session is what actually makes
// the key pool round-robin across keys, which is the property the shared-cap
// test is about. A test that wanted two keys but sent one session would see the
// cap enforced per key in appearance while never having touched the second one.
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

// credentialUpstream is a fakeUpstream that also records the credential each
// request arrived with. The shared-cap claim is about provider traffic being
// split across the provider's keys, and the only place that can be observed from
// outside the gateway is what the upstream was actually sent.
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

// distinct collapses recorded Authorization headers into a count per credential,
// with the "Bearer " prefix and surrounding whitespace stripped and empty values
// dropped. A test asks len() of the result to learn how many different
// credentials actually reached the upstream, which is how "the traffic really
// was split across the provider's keys" is checked rather than assumed.
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

// TestProviderRPMCapIsSharedAcrossKeys is the aggregate assertion. The cap
// belongs to the provider, so a client that can make the gateway use both of its
// keys must not get twice the configured rate. A per-key implementation would
// pass any single-key test and still overrun the provider's real quota, which is
// the entire reason the provider-level cap exists.
func TestProviderRPMCapIsSharedAcrossKeys(t *testing.T) {
	upSrv, up := setupCredentialUpstream(t)
	const cap = 4
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

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
			// The limit must be identified as the provider RPM cap, not merely
			// as "some rate limit". A saturated admission queue answers 429 with
			// the same type but the message "over capacity", which contains
			// neither "rpm" nor "provider", so this check tells the two limits
			// apart rather than passing on either.
			if !strings.Contains(got, "rpm") {
				t.Errorf("429 body does not name the provider RPM cap: %s. A "+
					"saturated admission queue produces a 429 with a different "+
					"message, so this should never be the limiter under test",
					got)
			}
			// Retry-After must be present and usable. A 429 without it leaves the
			// caller to invent a backoff, and a guess that is too short walks
			// straight back into the same limit, which is how a rate limit turns
			// into self-inflicted load on the gateway.
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
					// 0 is not a valid Retry-After and means "retry immediately",
					// which is exactly wrong for a rate limit.
					t.Errorf("Retry-After is %d, want at least 1 second", secs)
				case secs > 60:
					// The provider RPM window is a minute, so anything longer
					// means the gateway is misreporting its own window.
					t.Errorf("Retry-After is %d seconds, longer than the 60s provider "+
						"RPM window it is derived from", secs)
				}
				// The header must never UNDER-report the wait. The gateway's own
				// limit is a minute and this test has just spent it, so the value
				// must be at least 59. An upper bound alone does not catch
				// truncation: rounding down would invite the client back before
				// the window rolls, producing an immediate second 429 and turning
				// a rate limit into self-inflicted retry load.
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
	upSrv, up := setupCredentialUpstream(t)
	const cap = 4
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	// One request per session, so the round-robin key pool actually alternates
	// between the provider's two keys instead of sticky routing pinning every
	// request to the first one. If the cap were per key, the aggregate would
	// still have room after the first key's allowance and this would pass
	// `cap+2` requests.
	served := 0
	for i := 0; i < cap+2; i++ {
		body := fmt.Sprintf(`{"model":"flash","messages":[{"role":"user","content":"split-%d"}]}`, i)
		resp := postWithSession(t, gw+"/v1/chat/completions", fmt.Sprintf("split-session-%d", i), body)
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

	// The aggregate assertion above only bites if BOTH keys were actually used
	// to spend the cap. If the router had served all of it through one key, then
	// a per-key cap would also have stopped at `cap` here and the test would pass
	// for the wrong reason. This is the check that distinguishes "shared cap,
	// traffic split across keys" from "one key happened to be enough".
	creds := distinct(up.seenCredentials())
	if len(creds) < 2 {
		t.Errorf("the provider cap was spent using %d distinct credential(s) "+
			"(%v); the aggregate cap was never actually shared across the "+
			"provider's two keys, so this test does not establish sharing",
			len(creds), creds)
	}
}

// TestProviderRPMCapChargesEachRequestOnce checks the accounting, not just the
// cap. The two keys hold distinct secrets, so the upstream can attribute every
// accepted request to a key, and the number of upstream hits must equal the
// number the gateway counted as served. A charge that is refunded on success
// would let the gateway serve more than `cap` while looking correct here.
func TestProviderRPMCapChargesEachRequestOnce(t *testing.T) {
	upSrv, up := setupCredentialUpstream(t)
	const cap = 3
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	served, limited := 0, 0
	for i := 0; i < cap*4; i++ {
		body := fmt.Sprintf(`{"model":"flash","messages":[{"role":"user","content":"acct-%d"}]}`, i)
		// Distinct sessions, so both keys are charged and the traffic is really
		// split rather than pinned to one by sticky routing.
		resp := postWithSession(t, gw+"/v1/chat/completions", fmt.Sprintf("acct-session-%d", i), body)
		switch resp.StatusCode {
		case http.StatusOK:
			served++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("request %d: unexpected status %d", i, resp.StatusCode)
		}
	}

	// The upstream hit count is the ground truth for provider traffic. Anything
	// above `cap` means real requests went out past a cap the operator set.
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

// TestRetryAfterShrinksAsTheWindowAges checks the hint is a live measurement of
// the window rather than a constant.
//
// This is where rounding is actually observable. The window is established by
// the first Select, and a run of fast local requests lands within a few
// milliseconds of it, so a wait of 59.99s rounds to 59 under BOTH a correct
// implementation and one that truncates. Asserting an upper bound alone
// therefore cannot tell them apart, which is why the first version of the
// Retry-After check passed against a mutant that rounds down.
//
// Sleeping puts the wait on a fractional second, where rounding up and rounding
// down differ by a full second. The real HTTP path is kept for everything else;
// only the ageing needs wall-clock time, because an injected clock is the one
// thing that would make this test lie about the real gateway. The arithmetic
// itself is pinned without a clock by TestWriteRetryAfterRoundsUp.
func TestRetryAfterShrinksAsTheWindowAges(t *testing.T) {
	upSrv, _ := setup(t)
	const cap = 1
	_, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

	send := func(session string) *http.Response {
		t.Helper()
		body := fmt.Sprintf(`{"model":"flash","messages":[{"role":"user","content":"%s"}]}`, session)
		return postWithSession(t, gw+"/v1/chat/completions", session, body)
	}

	// Spend the single slot, which establishes the window.
	if resp := send("seed"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", resp.StatusCode)
	}

	// The window is 60s. Age it by 3.5s: the seeded request completes in a few
	// milliseconds, so the wait at the next request is between 56.4s and 56.5s.
	// A correctly rounded implementation emits 57. A truncating one emits 56.
	//
	// 2.5s would NOT have worked, and this test used to use it while claiming
	// otherwise. The seeded request does not complete instantly: it goes out to
	// the upstream, so the window is established before it returns, and the
	// window start is therefore ALREADY a few milliseconds in the past by the
	// time the sleep begins. That is enough to push a nominal 57.5s to 57.49s,
	// whose truncated value is 57, not 56. Both a correct implementation and a
	// truncating one then produce 57 and the assertion cannot tell them apart.
	// A mutation test confirmed the truncating mutant passed this assertion.
	//
	// 3.5s clears the problem with real margin instead of relying on the request
	// being fast. The wait is at least 56.49s (at most 56.5s), so the rounded
	// value is 57 with 0.49s of slack on the rounding boundary and the
	// truncated value is 56 with a full second of margin. Sleeping LONGER only
	// moves the value DOWN: 4.5s gives 55.5s, which truncates to 55 and would
	// also pass a 55-or-57 range, so the bound has to track the sleep length.
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
	// The window is 60s and the sleep above is at least 3.5s, so the wait is at
	// least 56.49s. A correct implementation rounds that UP to 57; a truncating
	// one produces 56. The lower bound is the load bearing part of this
	// assertion: it rejects truncation, which is the bug this test exists to
	// catch, and machine speed cannot break it, because sleeping longer can only
	// push the value DOWN, so an over-long sleep is caught rather than hidden.
	//
	// The exact match above 57 is NOT a claim that this test pins the rounding in
	// both directions. A mutant that over-reports by a full second emits 58 here
	// and still passes, and that is a known and accepted limit of this test:
	// the ageing sleep would have to overshoot by over 0.5s to expose it, and
	// widening the bound to allow that would reintroduce exactly the slack that
	// hid the truncation bug. Over-reporting is pinned deterministically instead
	// by TestWriteRetryAfterRoundsUp in the proxy package, which drives the
	// conversion directly with no clock in the way. What only this test can show
	// is that the header is wired to the LIVE window rather than a constant.
	if secs != 57 {
		t.Errorf("Retry-After is %d seconds after ageing a 60s window by 3.5s, "+
			"want exactly 57: the wait is at least 56.49s, so 56 means the value "+
			"was truncated and the client returns before the window rolls, and 58 "+
			"means the gateway is over-reporting its own window",
			secs)
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
	a, gw, _ := startGatewayWithProviderRPM(t, upSrv.URL, cap)

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
