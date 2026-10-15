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

func TestExhaustedChainDoesNotLeakTheUpstreamBaseURL(t *testing.T) {

	const baseURL = "http://127.0.0.1:1/internal-secret-host"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the upstream must never be reached; the base_url is closed")
	}))
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

	for _, leak := range []string{"127.0.0.1", "internal-secret-host", "http://", "://"} {
		if strings.Contains(body, leak) {
			t.Errorf("exhausted-chain response body leaks the upstream base_url "+
				"(%q appears in %s): a transport error renders as a *url.Error "+
				"carrying the full URL, and base_url is the field the SSRF guard "+
				"exists to keep private. The client must not learn it",
				leak, body)
		}
	}

	for _, secret := range []string{"sk-super-secret-value", "sk-other-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("exhausted-chain response body leaks a provider credential "+
				"(%q appears in %s)", secret, body)
		}
	}

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

	doPost(t, h, "", `{"model":"flash","stream":true,"input":"hi"}`)
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
		private:   `upstream: Post "http://` + addr + `/responses": ` + `dial tcp ` + addr + `: connect: connection refused`,
	}

	got := attemptReason(sel, ferr)

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

func TestTransportErrorFromARealDialIsClientSafe(t *testing.T) {
	const addr = "127.0.0.1:1"
	_, ferr := (&Handler{deps: Deps{Client: &http.Client{}}}).send(
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")),
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
