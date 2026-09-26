// Package observ collects cheap in-memory live metrics. It is entirely
// toggleable: when disabled, Record is a no-op and no per-request aggregation
// cost is paid. Per-request metadata (provider/key/latency/tokens/cost/status)
// is still persisted by the retention store regardless; observability governs
// the extra live aggregation and the route-reason detail surfaced here.
package observ

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

// Collector aggregates request-level counters and a bounded latency reservoir.
type Collector struct {
	enabled atomic.Bool

	mu                 sync.Mutex
	total, errors      int64
	cached             int64
	prompt, completion int64
	costMicros         int64
	byProvider         map[string]*perProvider
	latencies          []int64
	latCapacity        int
	start              time.Time
}

type perProvider struct {
	requests, errors int64
	costMicros       int64
}

// New builds a collector; enabled gates whether Record does work.
func New(enabled bool) *Collector {
	c := &Collector{
		byProvider:  map[string]*perProvider{},
		latCapacity: 512,
		start:       time.Now(),
	}
	c.enabled.Store(enabled)
	return c
}

// Enabled reports the current toggle.
func (c *Collector) Enabled() bool { return c.enabled.Load() }

// SetEnabled toggles observation live (used by config hot-reload).
func (c *Collector) SetEnabled(v bool) { c.enabled.Store(v) }

// Record folds one request into the live totals. No-op when disabled.
//
// M9: the per-provider map used to be keyed unconditionally by meta.Provider.
// Gateway-level failures - a request for an unroutable alias, a body that
// never selected a target - carry no provider, so each one created a permanent
// bucket keyed by the empty string. That bucket's error rate was then visible
// alongside every real provider, where it read as an eleventh provider that
// nobody operated, and it diluted the provider health picture exactly when
// routing was broken. A request with no provider is now counted only in the
// global totals; it is not attributed to a provider that never served it.
//
// The failure is NOT lost. It still increments c.errors, so the gateway-level
// error rate and the /stats error count reflect it, and the row is still
// persisted by the retention store with its error message.
func (c *Collector) Record(m model.RequestMeta, durMs int64) {
	if !c.enabled.Load() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	if m.Status == "error" {
		c.errors++
	}
	if m.Cached {
		c.cached++
	}
	c.prompt += int64(m.PromptTokens)
	c.completion += int64(m.CompletionTokens)
	c.costMicros += m.CostMicros
	// An empty provider name means the gateway failed before or instead of
	// choosing one. Bucketing it would invent a provider (M9), so it is left
	// out of the per-provider view while remaining in the totals above.
	if m.Provider != "" {
		pp := c.byProvider[m.Provider]
		if pp == nil {
			pp = &perProvider{}
			c.byProvider[m.Provider] = pp
		}
		pp.requests++
		if m.Status == "error" {
			pp.errors++
		}
		pp.costMicros += m.CostMicros
	}
	c.latencies = append(c.latencies, durMs)
	if len(c.latencies) > c.latCapacity {
		c.latencies = c.latencies[len(c.latencies)-c.latCapacity:]
	}
}

// Snapshot is a point-in-time metric dump for the admin API.
type Snapshot struct {
	Enabled          bool  `json:"enabled"`
	UptimeSeconds    int64 `json:"uptime_seconds"`
	Total            int64 `json:"total"`
	Errors           int64 `json:"errors"`
	Cached           int64 `json:"cached"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CostMicros       int64 `json:"cost_micros"`
	LatencyP50Ms     int64 `json:"latency_p50_ms"`
	LatencyP95Ms     int64 `json:"latency_p95_ms"`
	// LatencyWindow and LatencySamples make the percentile basis explicit.
	// The percentiles are computed from a bounded sliding reservoir, not from
	// the lifetime counters beside them, so an operator reading
	// latency_p95_ms during an incident needs to know the window is the most
	// recent LatencyWindow requests and not all-time traffic.
	LatencyWindow  string                  `json:"latency_window"`
	LatencySamples int                     `json:"latency_samples"`
	ByProvider     map[string]ProviderStat `json:"by_provider"`
}

// ProviderStat is per-provider counters.
type ProviderStat struct {
	Requests   int64 `json:"requests"`
	Errors     int64 `json:"errors"`
	CostMicros int64 `json:"cost_micros"`
}

// Snapshot returns a copy of current totals.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Snapshot{
		Enabled:          c.enabled.Load(),
		UptimeSeconds:    int64(time.Since(c.start).Seconds()),
		Total:            c.total,
		Errors:           c.errors,
		Cached:           c.cached,
		PromptTokens:     c.prompt,
		CompletionTokens: c.completion,
		CostMicros:       c.costMicros,
		ByProvider:       map[string]ProviderStat{},
	}
	if n := len(c.latencies); n > 0 {
		sorted := make([]int64, n)
		copy(sorted, c.latencies)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		s.LatencyP50Ms = sorted[n*50/100]
		s.LatencyP95Ms = sorted[n*95/100]
		s.LatencySamples = n
	}
	// Always name the window, even with no samples, so consumers never have to
	// infer whether a zero percentile means "no traffic" or "all-time 0 ms".
	s.LatencyWindow = fmt.Sprintf("last_%d_requests", c.latCapacity)
	for p, v := range c.byProvider {
		s.ByProvider[p] = ProviderStat{Requests: v.requests, Errors: v.errors, CostMicros: v.costMicros}
	}
	return s
}
