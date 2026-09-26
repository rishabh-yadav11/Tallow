package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The audit's Coverage Limitations named the 650-line Bubble Tea TUI as one of
// the least-examined areas, and "given the pattern of defects found elsewhere,
// more findings are likely there".
//
// Reading it settles the risk in one respect: the TUI renders and never
// decides. It holds no credentials (it displays key IDs, which are IDs), and
// every routing, budget, and accounting decision belongs to internal/routing,
// internal/budget, and internal/observ. So there is no security logic here to
// get wrong.
//
// What is here is a real, non-trivial contract: flexObject. The TUI tolerates
// the admin API's field naming drifting between snake_case and CamelCase, and
// degrades to 0/""/false rather than crashing a dashboard mid-incident. That
// is a decoder, and a decoder has the same failure mode every other decoder in
// this audit had - silently returning a zero that reads like a real value.
//
// The concrete hazard: if a field stops matching, the dashboard shows a
// plausible 0 for requests, errors, or cost. An operator watching a cost meter
// read 0 during an incident is worse off than one seeing a blank panel, and
// nothing anywhere would tell them the number is not real.

// TestFlexObjectMatchesAcrossNamingConventions is the core contract. The same
// logical field is addressed as either spelling and must resolve to the same
// value, because the admin API and the TUI are separate writers of this
// contract and either may change.
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

// TestFlexObjectMissingFieldsDegradeToZero asserts the deliberate choice: a
// field the wire does not carry reads as 0, not as an error. The dashboard must
// render during a partial or evolving API response rather than showing nothing
// at all.
func TestFlexObjectMissingFieldsDegradeToZero(t *testing.T) {
	f := parseFlex([]byte(`{"provider": "p1"}`))

	// "provider" is present, so addressing it as "Provider" must find it -
	// that is the tolerance working, not a missing field.
	if got := f.str("Provider"); got != "p1" {
		t.Errorf("present field addressed by the other spelling = %q, want p1", got)
	}

	// These are genuinely absent, and must read as zero values.
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

// TestFlexObjectMalformedJSONIsEmpty pins the outer guard. parseFlex swallows
// an unmarshal error and returns an empty object, so a corrupt response
// renders a blank dashboard instead of panicking inside the render loop.
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

// TestFlexObjectTypeMismatchDegrades covers a field present with the wrong
// type. A string where a number is expected is the realistic case after a
// schema change, and it must read as 0 rather than abort the whole snapshot.
func TestFlexObjectTypeMismatchDegrades(t *testing.T) {
	f := parseFlex([]byte(`{"cost_micros": "seven hundred", "provider": 42, "cached": "yes"}`))

	if got := f.num("cost_micros"); got != 0 {
		t.Errorf("num over a string = %d, want 0", got)
	}
	if got := f.str("provider"); got != "" {
		t.Errorf("str over a number = %q, want \"\"", got)
	}
	// boolean is the one exception: it also accepts a number, so a JSON 1/0
	// from a looser producer still reads correctly.
	if f.boolean("cached") {
		t.Error("boolean over a non-numeric string = true, want false")
	}
}

// TestFlexObjectBooleanAcceptsNumeric is the lenient branch in boolean(). A
// producer that emits 1/0 instead of true/false must not read as "never
// cached", which would make cache effectiveness silently disappear from the
// dashboard.
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

// TestNormalizeKey is the matching rule itself, pinned so the tolerance is
// exactly as wide as intended and no wider. Widening it silently would make
// two genuinely different fields collide.
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

// TestKeyTailTrimsForDisplay covers the one TUI function that reshapes a value
// on its way to the screen.
//
// The invariant that matters is the no-op case. keyTail must return a short
// key ID unchanged, and must never return an empty string for a non-empty
// input, because an empty route column in the log view would read as "no
// provider" - the exact M9 confusion the audit found, reintroduced in the UI.
func TestKeyTailTrimsForDisplay(t *testing.T) {
	cases := []struct{ in, want string }{
		{"account-1", "account-1"},         // already short: unchanged
		{"p1/account-1", "account-1"},      // slash-delimited
		{"my.provider.account", "account"}, // dot-delimited
		{"a/b/c", "c"},                     // last component wins
		{"a.b/c", "c"},                     // slash beats dot
		{"", ""},                           // empty stays empty
		{"trailing/", "trailing/"},         // trailing sep: no empty tail
		{"trailing.", "trailing."},         // likewise
		{"/leading", "leading"},            // leading sep
		{".leading", "leading"},            // likewise
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

// TestKeyTailNeverEmptiesAKeyID is the stated invariant on its own, so a future
// edit to the trimming logic cannot regress it silently.
func TestKeyTailNeverEmptiesAKeyID(t *testing.T) {
	for _, in := range []string{"k", "k1", "p/k1", "p.k1", "p/p/k1", "a-b_c"} {
		if keyTail(in) == "" {
			t.Errorf("keyTail(%q) = \"\"", in)
		}
	}
}

// TestUnmarshalJSONPopulatesFromEitherSpelling exercises the three decoders
// that consume flexObject, on a realistic /metrics payload in each naming
// convention. These are the structs the dashboard actually renders, so a silent
// field drop here is invisible on screen.
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
		// M8: the window and sample count must survive decoding, or a reader
		// cannot tell a sliding percentile from a lifetime one.
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
			// A cache hit's route reason is the only place its provenance is
			// visible in the log column, so it must survive decoding.
			if l.RouteReason != "sticky:p1/k1" {
				t.Errorf("RouteReason = %q, want %q", l.RouteReason, "sticky:p1/k1")
			}
		}
	})

	t.Run("provider stat, both spellings", func(t *testing.T) {
		// providerStat carries only the counters it charts; name/state come
		// from /providers, not /metrics.
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

// TestRenderDashboardStatesTheLatencyWindow is the end of the M8 chain, in the
// one place an operator actually reads a percentile.
//
// The collector publishes latency_window and latency_samples precisely so a
// reader can tell a sliding percentile from an all-time one. If the dashboard
// decodes them and then does not show them, the fix exists only in the JSON and
// the defect M8 described is still live for the human using the tool: a bare
// "p95 40 ms" during an incident reads as a healthy all-time figure.
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

	// And with no window reported, the claim must be absent rather than
	// printed with empty values.
	s.metrics.LatencyWindow = ""
	if out := stripANSI(renderDashboard(s)); strings.Contains(out, "latency over") {
		t.Errorf("dashboard claims a window it was not given:\n%s", out)
	}
}

// TestRenderDashboardSurvivesAnEmptySnapshot is the robustness floor for the
// whole render path. Every field here is zero-valued, which is what a fresh
// gateway or a failed fetch produces, and the render must not panic - a panic
// inside the Bubble Tea render loop takes down the dashboard entirely.
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

// stripANSI removes SGR escape sequences so assertions read the text an
// operator sees rather than the styling around it.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // skip the 'm'
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TestLogEntryDecodesACacheHitWithoutAProvider is the M7/M9 shape reaching the
// UI. A cache hit and a gateway failure both arrive with no provider, and the
// dashboard must render them without inventing a provider or a route.
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
	// The provenance must survive: it is what tells the operator which route
	// originally served the answer.
	if l.RouteReason != "cache_hit_from:p1" {
		t.Errorf("RouteReason = %q, want cache_hit_from:p1", l.RouteReason)
	}
	if l.Status != "cached" {
		t.Errorf("Status = %q, want cached", l.Status)
	}
}
