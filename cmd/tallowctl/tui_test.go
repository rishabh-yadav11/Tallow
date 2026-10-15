package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlexObjectMatchesAcrossNamingConventions(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		candidates []string
		wantNum    int64
	}{
		{"snake_case wire, CamelCase candidate", `{"cost_micros": 700}`, []string{"CostMicros", "cost_micros"}, 700},
		{"CamelCase wire, snake_case candidate", `{"CostMicros": 700}`, []string{"CostMicros", "cost_micros"}, 700},
		{"exact match preferred", `{"cost_micros": 1, "CostMicros": 2}`, []string{"CostMicros"}, 2},
		{"kebab-case wire", `{"cost-micros": 700}`, []string{"CostMicros"}, 700},
		{"mixed case", `{"COST_MICROS": 700}`, []string{"CostMicros"}, 700},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseFlex([]byte(c.body)).num(c.candidates...); got != c.wantNum {
				t.Errorf("num(%v) on %s = %d, want %d", c.candidates, c.body, got, c.wantNum)
			}
		})
	}
}

func TestFlexObjectMissingFieldsDegradeToZero(t *testing.T) {
	f := parseFlex([]byte(`{"provider": "p1"}`))

	if got := f.str("Provider"); got != "p1" {
		t.Errorf("present field addressed by the other spelling = %q, want p1", got)
	}

	if got := f.num("CostMicros"); got != 0 {
		t.Errorf("missing num = %d, want 0", got)
	}
	if got := f.str("UpstreamModel"); got != "" {
		t.Errorf("missing str = %q, want \"\"", got)
	}
	if f.boolean("Cached") {
		t.Error("missing bool = true, want false")
	}
	if v := f.get("NoSuchField"); v != nil {
		t.Errorf("get of a missing key = %s, want nil", v)
	}
}

func TestFlexObjectMalformedJSONIsEmpty(t *testing.T) {
	for _, body := range []string{``, `{`, `not json`, `[]`, `null`, `123`} {
		f := parseFlex([]byte(body))
		if got := f.num("anything"); got != 0 {
			t.Errorf("parseFlex(%q).num = %d, want 0", body, got)
		}
		if got := f.str("anything"); got != "" {
			t.Errorf("parseFlex(%q).str = %q, want \"\"", body, got)
		}
	}
}

func TestFlexObjectTypeMismatchDegrades(t *testing.T) {
	f := parseFlex([]byte(`{"cost_micros": "seven hundred", "provider": 42, "cached": "yes"}`))

	if got := f.num("cost_micros"); got != 0 {
		t.Errorf("num over a string = %d, want 0", got)
	}
	if got := f.str("provider"); got != "" {
		t.Errorf("str over a number = %q, want \"\"", got)
	}

	if f.boolean("cached") {
		t.Error("boolean over a non-numeric string = true, want false")
	}
}

func TestFlexObjectBooleanAcceptsNumeric(t *testing.T) {
	if !parseFlex([]byte(`{"cached": 1}`)).boolean("cached") {
		t.Error(`boolean over 1 = false, want true`)
	}
	if parseFlex([]byte(`{"cached": 0}`)).boolean("cached") {
		t.Error(`boolean over 0 = true, want false`)
	}
	if !parseFlex([]byte(`{"cached": true}`)).boolean("cached") {
		t.Error("boolean over true = false, want true")
	}
	if !parseFlex([]byte(`{"cached": 2}`)).boolean("cached") {
		t.Error("boolean over 2 = false, want true; any non-zero is true")
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"CostMicros":   "costmicros",
		"cost_micros":  "costmicros",
		"cost-micros":  "costmicros",
		"COST_MICROS":  "costmicros",
		"Cost_Micros":  "costmicros",
		"":             "",
		"already_fine": "alreadyfine",
	}
	for in, want := range cases {
		if got := normalizeKey(in); got != want {
			t.Errorf("normalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyTailTrimsForDisplay(t *testing.T) {
	cases := []struct{ in, want string }{
		{"account-1", "account-1"},
		{"p1/account-1", "account-1"},
		{"my.provider.account", "account"},
		{"a/b/c", "c"},
		{"a.b/c", "c"},
		{"", ""},
		{"trailing/", "trailing/"},
		{"trailing.", "trailing."},
		{"/leading", "leading"},
		{".leading", "leading"},
	}
	for _, c := range cases {
		got := keyTail(c.in)
		if got != c.want {
			t.Errorf("keyTail(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.in != "" && got == "" {
			t.Errorf("keyTail(%q) = \"\"; an empty tail renders as \"no route\"", c.in)
		}
	}
}

func TestKeyTailNeverEmptiesAKeyID(t *testing.T) {
	for _, in := range []string{"k", "k1", "p/k1", "p.k1", "p/p/k1", "a-b_c"} {
		if keyTail(in) == "" {
			t.Errorf("keyTail(%q) = \"\"", in)
		}
	}
}

func TestUnmarshalJSONPopulatesFromEitherSpelling(t *testing.T) {
	t.Run("snake_case", func(t *testing.T) {
		var m metricsResp
		body := `{"total":11,"errors":2,"cached":3,"cost_micros":4000,
			"prompt_tokens":50,"completion_tokens":25,
			"latency_p50_ms":10,"latency_p95_ms":90,
			"latency_window":"last_512_requests","latency_samples":11,
			"by_provider":{"p1":{"requests":5,"errors":1,"cost_micros":900}}}`
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m.Total != 11 || m.Errors != 2 || m.Cached != 3 {
			t.Errorf("totals = %d/%d/%d, want 11/2/3", m.Total, m.Errors, m.Cached)
		}
		if m.CostMicros != 4000 || m.PromptTokens != 50 || m.CompletionTokens != 25 {
			t.Errorf("money/tokens = %d/%d/%d, want 4000/50/25", m.CostMicros, m.PromptTokens, m.CompletionTokens)
		}
		if m.LatencyP50Ms != 10 || m.LatencyP95Ms != 90 {
			t.Errorf("latency = %d/%d, want 10/90", m.LatencyP50Ms, m.LatencyP95Ms)
		}

		if m.LatencyWindow != "last_512_requests" || m.LatencySamples != 11 {
			t.Errorf("window/samples = %q/%d, want last_512_requests/11", m.LatencyWindow, m.LatencySamples)
		}
		if got := m.ByProvider["p1"]; got.Requests != 5 || got.Errors != 1 || got.CostMicros != 900 {
			t.Errorf("by_provider[p1] = %+v, want {5 1 900}", got)
		}
	})

	t.Run("CamelCase", func(t *testing.T) {
		var m metricsResp
		body := `{"Total":11,"Errors":2,"Cached":3,"CostMicros":4000,
			"PromptTokens":50,"CompletionTokens":25,
			"LatencyP50Ms":10,"LatencyP95Ms":90,
			"LatencyWindow":"last_512_requests","LatencySamples":11,
			"ByProvider":{"p1":{"Requests":5,"Errors":1,"CostMicros":900}}}`
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m.Total != 11 || m.CostMicros != 4000 || m.LatencyP95Ms != 90 {
			t.Errorf("got %+v, want the same values as the snake_case form", m)
		}
		if m.LatencyWindow != "last_512_requests" || m.LatencySamples != 11 {
			t.Errorf("window/samples = %q/%d, want last_512_requests/11", m.LatencyWindow, m.LatencySamples)
		}
		if got := m.ByProvider["p1"]; got.Requests != 5 {
			t.Errorf("by_provider[p1] = %+v, want Requests 5", got)
		}
	})

	t.Run("log entry, both spellings", func(t *testing.T) {
		for _, body := range []string{
			`{"provider":"p1","key":"k1","model":"m","route_reason":"sticky:p1/k1","dur_ms":12,"prompt_tokens":10,"completion_tokens":5,"status":"ok"}`,
			`{"Provider":"p1","Key":"k1","Model":"m","RouteReason":"sticky:p1/k1","DurMillis":12,"PromptTokens":10,"CompletionTokens":5,"Status":"ok"}`,
		} {
			var l logEntry
			if err := json.Unmarshal([]byte(body), &l); err != nil {
				t.Fatalf("unmarshal %s: %v", body, err)
			}
			if l.Provider != "p1" || l.Key != "k1" || l.Status != "ok" {
				t.Errorf("got %+v, want provider p1 key k1 status ok", l)
			}
			if l.DurMillis != 12 || l.PromptTokens != 10 || l.CompletionTokens != 5 {
				t.Errorf("got %+v, want 12ms and 10/5 tokens", l)
			}

			if l.RouteReason != "sticky:p1/k1" {
				t.Errorf("RouteReason = %q, want %q", l.RouteReason, "sticky:p1/k1")
			}
		}
	})

	t.Run("provider stat, both spellings", func(t *testing.T) {

		for _, body := range []string{
			`{"requests":5,"errors":1,"cost_micros":900}`,
			`{"Requests":5,"Errors":1,"CostMicros":900}`,
		} {
			var p providerStat
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.Fatalf("unmarshal %s: %v", body, err)
			}
			if p.Requests != 5 || p.Errors != 1 || p.CostMicros != 900 {
				t.Errorf("got %+v, want 5/1/900", p)
			}
		}
	})
}

func TestRenderDashboardStatesTheLatencyWindow(t *testing.T) {
	s := &snapshot{
		metrics: metricsResp{
			Total:          11,
			Errors:         2,
			LatencyP50Ms:   10,
			LatencyP95Ms:   40,
			LatencyWindow:  "last_512_requests",
			LatencySamples: 7,
		},
		health: healthResp{Status: "ok", Version: "test"},
		config: configResp{Version: "1"},
	}

	out := stripANSI(renderDashboard(s))
	if !strings.Contains(out, "last_512_requests") {
		t.Errorf("dashboard does not name the latency window:\n%s", out)
	}
	if !strings.Contains(out, "7 sampled") {
		t.Errorf("dashboard does not report the sample count:\n%s", out)
	}

	s.metrics.LatencyWindow = ""
	if out := stripANSI(renderDashboard(s)); strings.Contains(out, "latency over") {
		t.Errorf("dashboard claims a window it was not given:\n%s", out)
	}
}

func TestRenderDashboardSurvivesAnEmptySnapshot(t *testing.T) {
	for _, s := range []*snapshot{
		{},
		{metrics: metricsResp{}},
		{providers: []providerView{{Name: "p1", Keys: []keyView{{ID: "k1"}}}}},
		{logs: []logEntry{{Provider: "", Key: "", Status: "cached"}}},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("renderDashboard panicked on %+v: %v", s, r)
				}
			}()
			_ = renderDashboard(s)
		}()
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestLogEntryDecodesACacheHitWithoutAProvider(t *testing.T) {
	var l logEntry
	body := `{"provider":"","key":"","model":"m","status":"cached",
		"route_reason":"cache_hit_from:p1","dur_ms":1,"cached":true}`
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if l.Provider != "" {
		t.Errorf("Provider = %q, want \"\"; a cache hit has no provider", l.Provider)
	}

	if l.RouteReason != "cache_hit_from:p1" {
		t.Errorf("RouteReason = %q, want cache_hit_from:p1", l.RouteReason)
	}
	if l.Status != "cached" {
		t.Errorf("Status = %q, want cached", l.Status)
	}
}
