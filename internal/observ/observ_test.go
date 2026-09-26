package observ

import (
	"sync"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

func meta(provider, status string, prompt, completion int, cached bool, costMicros int64) model.RequestMeta {
	return model.RequestMeta{
		Provider:         provider,
		Status:           status,
		Cached:           cached,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CostMicros:       costMicros,
	}
}

func assertSnapshot(t *testing.T, got, want Snapshot) {
	t.Helper()
	if got.Enabled != want.Enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.Enabled)
	}
	if got.Total != want.Total {
		t.Errorf("Total = %d, want %d", got.Total, want.Total)
	}
	if got.Errors != want.Errors {
		t.Errorf("Errors = %d, want %d", got.Errors, want.Errors)
	}
	if got.Cached != want.Cached {
		t.Errorf("Cached = %d, want %d", got.Cached, want.Cached)
	}
	if got.PromptTokens != want.PromptTokens {
		t.Errorf("PromptTokens = %d, want %d", got.PromptTokens, want.PromptTokens)
	}
	if got.CompletionTokens != want.CompletionTokens {
		t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, want.CompletionTokens)
	}
	if got.CostMicros != want.CostMicros {
		t.Errorf("CostMicros = %d, want %d", got.CostMicros, want.CostMicros)
	}
	if got.LatencyP50Ms != want.LatencyP50Ms {
		t.Errorf("LatencyP50Ms = %d, want %d", got.LatencyP50Ms, want.LatencyP50Ms)
	}
	if got.LatencyP95Ms != want.LatencyP95Ms {
		t.Errorf("LatencyP95Ms = %d, want %d", got.LatencyP95Ms, want.LatencyP95Ms)
	}
	if len(got.ByProvider) != len(want.ByProvider) {
		t.Errorf("len(ByProvider) = %d (%v), want %d", len(got.ByProvider), got.ByProvider, len(want.ByProvider))
	}
	for p, w := range want.ByProvider {
		g, ok := got.ByProvider[p]
		if !ok {
			t.Errorf("ByProvider missing %q", p)
			continue
		}
		if g != w {
			t.Errorf("ByProvider[%q] = %+v, want %+v", p, g, w)
		}
	}
}

func TestDisabledCollectorIsNoop(t *testing.T) {
	c := New(false)
	if c.Enabled() {
		t.Fatal("Enabled() = true for New(false)")
	}
	c.Record(meta("alpha", "ok", 10, 20, true, 5), 100)
	c.Record(meta("alpha", "error", 7, 3, false, 9), 200)
	c.Record(meta("beta", "ok", 1, 1, false, 1), 300)

	s := c.Snapshot()
	assertSnapshot(t, s, Snapshot{Enabled: false})
	if s.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %d, want >= 0", s.UptimeSeconds)
	}
}

func TestEnabledAggregation(t *testing.T) {
	c := New(true)
	if !c.Enabled() {
		t.Fatal("Enabled() = false for New(true)")
	}
	records := []struct {
		m     model.RequestMeta
		durMs int64
	}{
		{meta("alpha", "ok", 100, 50, false, 10), 40},
		{meta("alpha", "ok", 100, 50, true, 10), 5},
		{meta("alpha", "error", 100, 50, false, 10), 90},
		{meta("beta", "ok", 7, 3, false, 4), 25},
	}
	for _, r := range records {
		c.Record(r.m, r.durMs)
	}

	lat := []int64{5, 25, 40, 90} // sorted ascending: Snapshot sorts internally before indexing
	want := Snapshot{
		Enabled:          true,
		Total:            4,
		Errors:           1,
		Cached:           1,
		PromptTokens:     307,
		CompletionTokens: 153,
		CostMicros:       34,
		LatencyP50Ms:     lat[len(lat)*50/100],
		LatencyP95Ms:     lat[len(lat)*95/100],
		ByProvider: map[string]ProviderStat{
			"alpha": {Requests: 3, Errors: 1, CostMicros: 30},
			"beta":  {Requests: 1, Errors: 0, CostMicros: 4},
		},
	}
	s := c.Snapshot()
	assertSnapshot(t, s, want)
	if s.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %d, want >= 0", s.UptimeSeconds)
	}
}

// TestLatencyReservoirBounded records more durations than the reservoir
// capacity (512) with strictly increasing values. If the reservoir kept every
// sample, the early tiny latencies would drag the percentiles down (p95 would
// sit at 570 for 600 records); keeping only the last 512 puts p95 at 574, so
// the assertion window below deliberately excludes the unbounded value. The
// previous window [560,590] contained 570 and therefore could never fail.
func TestLatencyReservoirBounded(t *testing.T) {
	c := New(true)
	const records = 600
	for i := 0; i < records; i++ {
		c.Record(meta("p", "ok", 1, 1, false, 1), int64(i)) // 0..599 ms
	}

	s := c.Snapshot()
	if s.Total != records {
		t.Fatalf("Total = %d, want %d (all records must count)", s.Total, records)
	}
	if s.LatencyP50Ms < 330 || s.LatencyP50Ms > 360 {
		t.Errorf("LatencyP50Ms = %d, want in [330,360] (bounded reservoir ~344; unbounded would be 300)", s.LatencyP50Ms)
	}
	// Unbounded p95 = 570; bounded (last 512 of 0..599, i.e. 88..599) = 574.
	if s.LatencyP95Ms < 571 || s.LatencyP95Ms > 590 {
		t.Errorf("LatencyP95Ms = %d, want in [571,590] (bounded reservoir ~574; unbounded would be 570)", s.LatencyP95Ms)
	}
	if s.LatencyP95Ms > records-1 {
		t.Errorf("LatencyP95Ms = %d exceeds max recorded %d", s.LatencyP95Ms, records-1)
	}
	// The window and sample count must be reported so a reader knows these
	// percentiles are a sliding window, not lifetime percentiles.
	if s.LatencySamples != 512 {
		t.Errorf("LatencySamples = %d, want 512 (bounded reservoir)", s.LatencySamples)
	}
	if s.LatencyWindow != "last_512_requests" {
		t.Errorf("LatencyWindow = %q, want %q", s.LatencyWindow, "last_512_requests")
	}
}

// TestLatencySamplesBelowCapacity covers the unfilled-reservoir case: the
// sample count must reflect real samples rather than the capacity, and the
// window must still be named when there is no traffic at all, so a zero
// percentile is never ambiguous.
func TestLatencySamplesBelowCapacity(t *testing.T) {
	c := New(true)
	for i := 0; i < 7; i++ {
		c.Record(meta("p", "ok", 1, 1, false, 1), int64(i))
	}
	if s := c.Snapshot(); s.LatencySamples != 7 {
		t.Errorf("LatencySamples = %d, want 7", s.LatencySamples)
	}

	empty := New(true).Snapshot()
	if empty.LatencySamples != 0 {
		t.Errorf("empty collector LatencySamples = %d, want 0", empty.LatencySamples)
	}
	if empty.LatencyWindow != "last_512_requests" {
		t.Errorf("empty collector LatencyWindow = %q, want %q", empty.LatencyWindow, "last_512_requests")
	}
}

func TestToggleLive(t *testing.T) {
	c := New(true)
	c.Record(meta("p", "ok", 10, 5, false, 2), 50)
	afterFirst := c.Snapshot()
	wantFirst := Snapshot{
		Enabled:          true,
		Total:            1,
		PromptTokens:     10,
		CompletionTokens: 5,
		CostMicros:       2,
		LatencyP50Ms:     50,
		LatencyP95Ms:     50,
		ByProvider:       map[string]ProviderStat{"p": {Requests: 1, CostMicros: 2}},
	}
	assertSnapshot(t, afterFirst, wantFirst)

	c.SetEnabled(false)
	if c.Enabled() {
		t.Fatal("Enabled() = true after SetEnabled(false)")
	}
	c.Record(meta("p", "ok", 10, 5, false, 2), 50)
	c.Record(meta("p", "error", 10, 5, false, 2), 50)
	afterDisabled := c.Snapshot()
	// Counters must be frozen at wantFirst, but Enabled reflects the live toggle.
	wantDisabled := wantFirst
	wantDisabled.Enabled = false
	assertSnapshot(t, afterDisabled, wantDisabled)

	c.SetEnabled(true)
	c.Record(meta("p", "error", 10, 5, false, 2), 50)
	assertSnapshot(t, c.Snapshot(), Snapshot{
		Enabled:          true,
		Total:            2,
		Errors:           1,
		PromptTokens:     20,
		CompletionTokens: 10,
		CostMicros:       4,
		LatencyP50Ms:     50,
		LatencyP95Ms:     50,
		ByProvider:       map[string]ProviderStat{"p": {Requests: 2, Errors: 1, CostMicros: 4}},
	})
}

func TestConcurrentRecord(t *testing.T) {
	const workers = 8
	const perWorker = 250 // total 2000

	c := New(true)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			provider := "provA"
			if w%2 == 1 {
				provider = "provB"
			}
			for i := 0; i < perWorker; i++ {
				c.Record(meta(provider, "ok", 1, 1, false, 1), 10)
			}
		}(w)
	}
	wg.Wait()

	want := Snapshot{
		Enabled:          true,
		Total:            workers * perWorker,
		PromptTokens:     workers * perWorker,
		CompletionTokens: workers * perWorker,
		CostMicros:       workers * perWorker,
		LatencyP50Ms:     10,
		LatencyP95Ms:     10,
		ByProvider: map[string]ProviderStat{
			"provA": {Requests: workers / 2 * perWorker, CostMicros: workers / 2 * perWorker},
			"provB": {Requests: workers / 2 * perWorker, CostMicros: workers / 2 * perWorker},
		},
	}
	assertSnapshot(t, c.Snapshot(), want)
}

func TestSnapshotReturnsCopy(t *testing.T) {
	c := New(true)
	c.Record(meta("p", "ok", 10, 5, false, 2), 50)

	s := c.Snapshot()
	s.ByProvider["p"] = ProviderStat{Requests: 999, Errors: 999, CostMicros: 999}
	s.ByProvider["ghost"] = ProviderStat{Requests: 1}
	s.Total = 42

	after := c.Snapshot()
	want := Snapshot{
		Enabled:          true,
		Total:            1,
		PromptTokens:     10,
		CompletionTokens: 5,
		CostMicros:       2,
		LatencyP50Ms:     50,
		LatencyP95Ms:     50,
		ByProvider:       map[string]ProviderStat{"p": {Requests: 1, CostMicros: 2}},
	}
	assertSnapshot(t, after, want)
	if _, ok := after.ByProvider["ghost"]; ok {
		t.Error("mutating a returned Snapshot leaked the \"ghost\" provider into the collector")
	}
}
