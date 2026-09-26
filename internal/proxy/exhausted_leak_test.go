package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/budget"
	"github.com/rishabh-yadav11/tallow/internal/cache"
	"github.com/rishabh-yadav11/tallow/internal/health"
	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/observ"
	"github.com/rishabh-yadav11/tallow/internal/registry"
	"github.com/rishabh-yadav11/tallow/internal/routing"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

// The exhausted-attempts message added in 5f787b3 is the first place a
// TRANSPORT error's text reaches a client. Before it, an exhausted retryable
// chain returned a constant string, so nothing about the internal upstream
// topology escaped.
//
// forwardError carries the raw error from Client.Do, which for a transport
// failure is a *url.Error. A url.Error renders as
// `Post "<full base_url>": <cause>`, so the message leaked the provider's full
// base_url to the calling client and into the audit row.
//
// That is a real disclosure and it contradicts a deliberate decision already in
// this package: upstreamErrorMsg returns only http.StatusText and logs the
// detail, with a comment saying "Never return this to a client". The new
// message routed around that rule rather than through it.
//
// Why it matters. base_url decides where a request carrying the provider
// Authorization header and the user's full prompt is sent. Telling an
// untrusted client "openrouter failed against https://internal.corp.example/
// v1" hands over the internal network layout that the SSRF guard exists to
// keep private, and does so for free on every request that exhausts its chain.
// The client already knows which alias it asked for; it does not get to learn
// the addresses behind it.
//
// Why the suite stayed green. Every other test in this area drives a live
// httptest server that returns an HTTP status, so the transport branch never
// executes and every attempt reason is already a safe http.StatusText. The
// SSRF guard blocks the loopback case that would make this easiest to notice
// from a test, and config.SetAllowPrivateBaseURL(true) is an exported escape
// hatch, so the case is reachable in production too.

// TestExhaustedChainDoesNotLeakTheUpstreamBaseURL is the regression. The
// upstream is closed, so every attempt fails in transport, and the chain
// exhausts: exactly the branch that leaked.
func TestExhaustedChainDoesNotLeakTheUpstreamBaseURL(t *testing.T) {
	// A base_url that would be recognisable in a leak. Port 1 is privileged and
	// nothing listens there, so the dial fails immediately in transport.
	const baseURL = "http://127.0.0.1:1/internal-secret-host"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the upstream must never be reached; the base_url is closed")
	}))
	srv.Close() // closed before use, so every dial fails in transport

	reg := registry.New(
		[]model.Provider{{
			Name:    "alpha",
			BaseURL: baseURL,
			Keys:    []model.Key{{ID: "k1", Secret: "sk-super-secret-value", RPM: 10000}},
		}, {
			Name:    "beta",
			BaseURL: baseURL,
			Keys:    []model.Key{{ID: "k2", Secret: "sk-other-secret", RPM: 10000}},
		}},
		[]model.Alias{{
			Name: "flash",
			Targets: []model.Target{
				{Provider: "alpha", Model: "a-v4"},
				{Provider: "beta", Model: "b-v4"},
			},
		}},
	)
	r := routing.NewRouter(reg, health.NewRegistry(), time.Minute, time.Now)
	r.SetBudgets(map[string]budget.KeyLimits{
		"alpha/k1": {RPM: 10000, MaxRequests: 10000, Window: time.Minute, MaxConcurrent: 100},
		"beta/k2":  {RPM: 10000, MaxRequests: 10000, Window: time.Minute, MaxConcurrent: 100},
	}, map[string]int{"alpha": 10000, "beta": 10000})
	h := newHarness(t, Deps{
		Router:   r,
		Registry: reg,
		Cache:    cache.New(64, time.Minute, 0),
		Observ:   observ.New(true),
	})

	rec := doPost(t, h, "", simpleBody)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()

	// The host is the disclosure. Everything below is about not leaking
	// deployment internals to a client that only asked for a model name.
	for _, leak := range []string{"127.0.0.1", "internal-secret-host", "http://", "://"} {
		if strings.Contains(body, leak) {
			t.Errorf("exhausted-chain response body leaks the upstream base_url "+
				"(%q appears in %s): a transport error renders as a *url.Error "+
				"carrying the full URL, and base_url is the field the SSRF guard "+
				"exists to keep private. The client must not learn it",
				leak, body)
		}
	}
	// A credential must never appear either, in any branch.
	for _, secret := range []string{"sk-super-secret-value", "sk-other-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("exhausted-chain response body leaks a provider credential "+
				"(%q appears in %s)", secret, body)
		}
	}
	// The fix must not regress into the useless constant either: the route and
	// a safe reason are still required for the message to be worth anything.
	if !strings.Contains(body, "alpha") || !strings.Contains(body, "beta") {
		t.Errorf("exhausted-chain body %q stopped naming the failed routes: "+
			"redacting the URL must not remove the diagnostic that was the "+
			"point of the change", body)
	}
	if !strings.Contains(body, "connection failed") {
		t.Errorf("exhausted-chain body %q carries no description of a "+
			"transport failure: the operator still needs to know the request "+
			"never reached an upstream, without learning the address", body)
	}
}

// TestExhaustedChainLeakDoesNotReachTheAuditLog guards the second surface. The
// recorded row is read by operators and exported by tooling, and it must hold
// the same redacted text the client sees.
func TestExhaustedChainLeakDoesNotReachTheAuditLog(t *testing.T) {
	const baseURL = "http://127.0.0.1:1/internal-secret-host"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	reg := registry.New(
		[]model.Provider{{
			Name:    "alpha",
			BaseURL: baseURL,
			Keys:    []model.Key{{ID: "k1", Secret: "sk-super-secret-value", RPM: 10000}},
		}, {
			Name:    "beta",
			BaseURL: baseURL,
			Keys:    []model.Key{{ID: "k2", Secret: "sk-other-secret", RPM: 10000}},
		}},
		[]model.Alias{{
			Name: "flash",
			Targets: []model.Target{
				{Provider: "alpha", Model: "a-v4"},
				{Provider: "beta", Model: "b-v4"},
			},
		}},
	)
	r := routing.NewRouter(reg, health.NewRegistry(), time.Minute, time.Now)
	r.SetBudgets(map[string]budget.KeyLimits{
		"alpha/k1": {RPM: 10000, MaxRequests: 10000, Window: time.Minute, MaxConcurrent: 100},
		"beta/k2":  {RPM: 10000, MaxRequests: 10000, Window: time.Minute, MaxConcurrent: 100},
	}, map[string]int{"alpha": 10000, "beta": 10000})

	st, err := store.Open(store.StoreConfig{Path: filepath.Join(t.TempDir(), "tallow.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	h := newHarness(t, Deps{
		Router:       r,
		Registry:     reg,
		Cache:        cache.New(64, time.Minute, 0),
		CacheEnabled: true,
		Observ:       observ.New(true),
		Store:        st,
	})

	doPost(t, h, "", `{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	st.Flush()

	rows, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	var recorded string
	for _, row := range rows {
		if row.Err != "" {
			recorded = row.Err
			break
		}
	}
	if recorded == "" {
		t.Fatal("no request row carried an error: the audit log is the " +
			"operator's primary view, so an empty one proves nothing")
	}
	for _, leak := range []string{"127.0.0.1", "internal-secret-host", "http://"} {
		if strings.Contains(recorded, leak) {
			t.Errorf("the recorded audit row leaks the upstream base_url (%q "+
				"appears in %q): the row is exported by tooling, so it needs "+
				"the same redaction the client response gets", leak, recorded)
		}
	}
}

// TestTransportReasonIsRedacted is the unit-level control. It pins the
// invariant that a forwardError's client-facing msg never carries the address
// while the private field does, so a future change to the retry loop cannot
// quietly reintroduce the raw error by using a different accessor.
func TestTransportReasonIsRedacted(t *testing.T) {
	const addr = `10.0.0.5:11434`
	sel := &routing.Selection{
		Provider: "alpha",
		Key:      "k1",
		BaseURL:  "http://" + addr + "/v1",
	}
	ferr := &forwardError{
		retryable: true,
		transport: true,
		msg:       "connection failed",
		private:   `upstream: Post "http://` + addr + `/v1/chat/completions": ` + `dial tcp ` + addr + `: connect: connection refused`,
	}

	got := attemptReason(sel, ferr)
	// attemptReason is a pass-through, so the guarantee it relies on is the
	// msg itself. Assert that directly, then assert what the caller sees.
	for _, leak := range []string{addr, "10.0.0.5", "11434", "http://"} {
		if strings.Contains(ferr.msg, leak) {
			t.Errorf("forwardError.msg = %q leaks the upstream address (%q): "+
				"msg reaches response bodies and audit rows, so it must be "+
				"client-safe at construction", ferr.msg, leak)
		}
		if strings.Contains(got, leak) {
			t.Errorf("attemptReason returns %q, which leaks the upstream address "+
				"(%q): a transport error's text carries the full base_url and "+
				"must never be rendered to a client", got, leak)
		}
	}
	if !strings.Contains(got, "alpha/k1") {
		t.Errorf("attemptReason = %q, want it to still name the route", got)
	}
	if !strings.Contains(got, "connection failed") {
		t.Errorf("attemptReason = %q, want it to keep a description of a "+
			"transport failure: the operator still needs to know the request "+
			"never reached an upstream", got)
	}

	// The detail must not be lost, it must only be withheld from the caller.
	if !strings.Contains(ferr.logDetail(), addr) {
		t.Errorf("forwardError.logDetail() = %q, want it to retain the address "+
			"for the operator log: the client must not learn it, but the "+
			"diagnostic must still exist somewhere the operator can read",
			ferr.logDetail())
	}
	if ferr.logDetail() == ferr.Error() {
		t.Error("forwardError.logDetail() returned the same text as Error(): " +
			"the private detail is not reaching the log")
	}
}

// TestTransportErrorFromARealDialIsClientSafe closes the loop on the real
// production path. The unit test above hand-builds a forwardError, so it cannot
// catch a construction site that forgets to be safe. This drives an actual
// Client.Do dial against a closed port and checks the error that the retry loop
// will hand to a client, which is where a real *url.Error arrives.
func TestTransportErrorFromARealDialIsClientSafe(t *testing.T) {
	const addr = "127.0.0.1:1"
	_, ferr := (&Handler{deps: Deps{Client: &http.Client{}}}).send(
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")),
		&routing.Selection{BaseURL: "http://" + addr, Secret: "sk-secret"},
		[]byte(`{}`),
	)
	if ferr == nil {
		t.Fatal("send returned no error for a dial to a closed port: the test " +
			"is not exercising the transport branch at all")
	}
	if !ferr.transport {
		t.Fatalf("ferr.transport = false, want true for a dial failure (%+v)", ferr)
	}
	for _, leak := range []string{addr, "127.0.0.1", "http://"} {
		if strings.Contains(ferr.Error(), leak) {
			t.Errorf("the transport error a client would see, %q, contains %q: "+
				"send must put only a safe category in msg and the full text in "+
				"private", ferr.Error(), leak)
		}
	}
	if !strings.Contains(ferr.logDetail(), addr) {
		t.Errorf("logDetail() = %q, want the real dial error with the address "+
			"for the log", ferr.logDetail())
	}
}

// TestHTTPStatusReasonIsUnchanged is the positive control for the redaction: a
// status-based reason was already safe and must stay exactly as useful as
// before. Without this, a fix that replaced every reason with a generic string
// would pass the leak test above.
func TestHTTPStatusReasonIsUnchanged(t *testing.T) {
	sel := &routing.Selection{Provider: "alpha", Key: "k1"}
	ferr := &forwardError{retryable: true, status: http.StatusTooManyRequests,
		msg: "Too Many Requests"}
	got := attemptReason(sel, ferr)
	if !strings.Contains(got, "alpha/k1") {
		t.Errorf("attemptReason = %q, want the route named", got)
	}
	if !strings.Contains(got, "Too Many Requests") {
		t.Errorf("attemptReason = %q, want the status reason preserved: "+
			"redacting transport URLs must not cost the status detail", got)
	}
}
