package proxy

import (
	"fmt"
	"io"
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

// The failover loop used to throw away the reason for every failed attempt and
// report only "upstream attempts exhausted". That single string was returned
// for a refused private base_url, a rejected API key, and a dead socket alike.
//
// The cost is not cosmetic. An operator who sees that message has to reproduce
// the failure by hand to learn which of those it was, and the three cases have
// three different fixes: move the host, rotate the key, or restart the service.
// The audit log was equally silent, because the same truncated string was what
// got stored.
//
// This was found while validating the documented quick start: pointing a
// provider at a loopback upstream produced a 502 whose message named no
// provider, no key, and no cause, while /logs showed only "upstream attempts
// exhausted".

// TestExhaustedChainNamesEveryFailedRoute is the core regression. Each target
// fails with a distinct, identifiable reason, and all of them must appear.
func TestExhaustedChainNamesEveryFailedRoute(t *testing.T) {
	// A distinct status per target, so each failure is separately identifiable
	// in the final message.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := registry.New(
		[]model.Provider{{
			Name:    "alpha",
			BaseURL: srv.URL,
			Keys:    []model.Key{{ID: "k1", Secret: "sk-a", RPM: 10000}},
		}, {
			Name:    "beta",
			BaseURL: srv.URL,
			Keys:    []model.Key{{ID: "k2", Secret: "sk-b", RPM: 10000}},
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

	// Both targets 500, which is retryable, so the chain is genuinely exhausted
	// rather than short-circuited on a non-retryable error.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"upstream boom"}`)
	})

	rec := doPost(t, h, "", simpleBody)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: a fully-failed chain is a gateway error", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(body, want) {
			t.Errorf("exhausted-chain message %q does not name the %s target that "+
				"failed: the operator cannot tell which routes were tried",
				body, want)
		}
	}
	// The attempt count lets an operator confirm the chain was actually walked
	// to the end, rather than stopping silently partway.
	if !strings.Contains(body, "after 2") {
		t.Errorf("exhausted-chain message %q does not report the number of "+
			"attempts, so a short-circuited chain is indistinguishable from a "+
			"fully-walked one", body)
	}
}

// TestExhaustedChainNamesTheCause proves the message carries the actual cause,
// not just the route. Two different upstream statuses must be distinguishable,
// because "which provider" and "why" are both needed to act.
func TestExhaustedChainNamesTheCause(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := registry.New(
		[]model.Provider{{
			Name:    "alpha",
			BaseURL: srv.URL,
			Keys:    []model.Key{{ID: "k1", Secret: "sk-a", RPM: 10000}},
		}, {
			Name:    "beta",
			BaseURL: srv.URL,
			Keys:    []model.Key{{ID: "k2", Secret: "sk-b", RPM: 10000}},
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

	// alpha is rate limited, beta is broken. Both are retryable, so both are
	// tried and both reasons must survive.
	//
	// The targets are told apart by the model name in the request body, which is
	// what actually distinguishes an upstream attempt for alpha from one for
	// beta. A test that guessed at some other marker (a path, a header the proxy
	// does not set) would silently never exercise the branch it claims to cover.
	var alphaSeen, betaSeen bool
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "a-v4") {
			alphaSeen = true
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate limited"}`)
			return
		}
		betaSeen = true
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":"upstream broken"}`)
	})

	rec := doPost(t, h, "", simpleBody)
	body := rec.Body.String()

	if !alphaSeen || !betaSeen {
		t.Fatalf("test setup did not exercise both targets (alpha=%v beta=%v); "+
			"the chain did not run as this test assumes", alphaSeen, betaSeen)
	}
	// The two causes must be tellable apart. A message naming both routes but
	// not the distinct status of each has saved the operator far less than it
	// looks: "alpha/k1: Too Many Requests" and "beta/k2: Bad Gateway" point at
	// different fixes (wait out a quota versus investigate an outage), and a
	// message that rendered both as the same generic text would not.
	if !strings.Contains(body, "Too Many Requests") {
		t.Errorf("exhausted-chain message %q does not carry the 429 that "+
			"alpha returned: a rate limit is indistinguishable from an outage",
			body)
	}
	if !strings.Contains(body, "Bad Gateway") {
		t.Errorf("exhausted-chain message %q does not carry the 502 that beta "+
			"returned: a bad gateway is indistinguishable from a rate limit",
			body)
	}
}

// TestExhaustedChainReasonIsRecordedInTheAuditLog guards the store-facing half.
// An operator reads /logs far more often than the client error, so a message
// that names the cause in the HTTP response but not in the recorded row would
// still leave the operator in the dark.
//
// This deliberately uses a TWO-target alias and a streaming request. Two
// details, each of which silently narrowed the first version of this test:
//   - A ONE-target chain never reaches the exhausted-attempts path. The single
//     failed attempt is recorded against that key, the next pass's Select finds
//     nothing and returns a "no route" error, and that early return carries its
//     own already-useful text. A one-target test therefore passed while
//     asserting nothing about the code path it was named for.
//   - A NON-streaming request goes through the coalesced-flight code, which sets
//     meta.Err from its own fetch value rather than from the streaming loop. So
//     a mutation that only reverts the streaming loop left it passing.
func TestExhaustedChainReasonIsRecordedInTheAuditLog(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := registry.New(
		[]model.Provider{{
			Name:    "alpha",
			BaseURL: srv.URL,
			Keys:    []model.Key{{ID: "k1", Secret: "sk-a", RPM: 10000}},
		}, {
			Name:    "beta",
			BaseURL: srv.URL,
			Keys:    []model.Key{{ID: "k2", Secret: "sk-b", RPM: 10000}},
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
	// Caching is enabled deliberately. cacheKey returns not-cacheable for a
	// streaming request regardless, so whether it is on is irrelevant to this
	// test's subject - and leaving it off would mean the cache fast path could
	// never be reached here, which is a silent way to narrow a test's coverage.
	h := newHarness(t, Deps{
		Router:       r,
		Registry:     reg,
		Cache:        cache.New(64, time.Minute, 0),
		CacheEnabled: true,
		Observ:       observ.New(true),
		Store:        st,
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	})

	// Stream so the request takes the path whose loop builds the message.
	// The upstream 500s, so nothing is ever streamed to the client; the request
	// fails before the first token, which is the retryable path under test.
	doPost(t, h, "", `{"model":"flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	// Store writes are queued, so the queue must be drained before the row is
	// readable. Reading before the drain would assert against an empty table.
	st.Flush()
	rows, err := st.RecentRequests(10)
	if err != nil {
		t.Fatalf("RecentRequests: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no request row was recorded: the audit log is the operator's " +
			"primary view, so an unrecorded failure is invisible")
	}
	var errText string
	for _, row := range rows {
		if row.Err != "" {
			errText = row.Err
			break
		}
	}
	if errText == "" {
		t.Fatal("the recorded request row carries no error text at all")
	}
	if strings.TrimSpace(errText) == "upstream attempts exhausted" {
		t.Errorf("the recorded error is still the bare %q: /logs is where an "+
			"operator actually looks, and it must name the cause too", errText)
	}
	if !strings.Contains(errText, "alpha") {
		t.Errorf("the recorded error %q does not name the provider that failed", errText)
	}
}

// TestExhaustedMessageStaysBoundedOnLongChains keeps the added detail from
// becoming a denial-of-service vector. A fallback chain can be long, and the
// error is written to the store and returned to a client, so it must be capped
// while still reporting the true total.
func TestExhaustedMessageStaysBoundedOnLongChains(t *testing.T) {
	var many []string
	for i := 0; i < 50; i++ {
		many = append(many, fmt.Sprintf("p%d/k: failed", i))
	}
	msg := exhaustedMessage(many)
	if len(msg) > 1024 {
		t.Errorf("exhausted message is %d bytes for a 50-target chain: the error "+
			"is stored per request and returned to clients, so it must be bounded",
			len(msg))
	}
	// The true count must survive the truncation, or the operator cannot tell
	// how much of the chain was actually walked.
	if !strings.Contains(msg, "after 50") {
		t.Errorf("truncated message %q loses the true attempt count", msg)
	}
	if !strings.Contains(msg, "more") {
		t.Errorf("truncated message %q does not say that reasons were omitted, "+
			"which would make it look like the whole chain was reported", msg)
	}
}

// TestExhaustedMessageWithNoAttempts is the boundary control: Select can fail
// before any attempt is made, in which case there is nothing to report and the
// message must still be well formed.
func TestExhaustedMessageWithNoAttempts(t *testing.T) {
	if got := exhaustedMessage(nil); got != "upstream attempts exhausted" {
		t.Errorf("exhaustedMessage(nil) = %q, want the plain message", got)
	}
}
