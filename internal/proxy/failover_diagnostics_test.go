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

func TestExhaustedChainNamesEveryFailedRoute(t *testing.T) {

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

	if !strings.Contains(body, "after 2") {
		t.Errorf("exhausted-chain message %q does not report the number of "+
			"attempts, so a short-circuited chain is indistinguishable from a "+
			"fully-walked one", body)
	}
}

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

	doPost(t, h, "", `{"model":"flash","stream":true,"input":"hi"}`)

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

	if !strings.Contains(msg, "after 50") {
		t.Errorf("truncated message %q loses the true attempt count", msg)
	}
	if !strings.Contains(msg, "more") {
		t.Errorf("truncated message %q does not say that reasons were omitted, "+
			"which would make it look like the whole chain was reported", msg)
	}
}

func TestExhaustedMessageWithNoAttempts(t *testing.T) {
	if got := exhaustedMessage(nil); got != "upstream attempts exhausted" {
		t.Errorf("exhaustedMessage(nil) = %q, want the plain message", got)
	}
}
