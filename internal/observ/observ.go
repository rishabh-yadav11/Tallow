// Package observ collects cheap in-memory live metrics. It is entirely
// toggleable: when disabled, Record is a no-op and no per-request aggregation
// cost is paid. Per-request metadata (provider/key/latency/tokens/cost/status)
// is still persisted by the retention store regardless; observability governs
// the extra live aggregation and the route-reason detail surfaced here.
package observ

import (
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
	costCents          int64
	byProvider         map[string]*perProvider
	latencies          []int64
	latCapacity        int
	start              time.Time
}

type perProvider struct {
	requests, errors int64
	costCents        int64
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
	c.costCents += m.CostCents
	pp := c.byProvider[m.Provider]
	if pp == nil {
		pp = &perProvider{}
		c.byProvider[m.Provider] = pp
	}
	pp.requests++
	if m.Status == "error" {
		pp.errors++
	}
	pp.costCents += m.CostCents
	c.latencies = append(c.latencies, durMs)
	if len(c.latencies) > c.latCapacity {
		c.latencies = c.latencies[len(c.latencies)-c.latCapacity:]
	}
}

// Snapshot is a point-in-time metric dump for the admin API.
type Snapshot struct {
	Enabled          bool                    `json:"enabled"`
	UptimeSeconds    int64                   `json:"uptime_seconds"`
	Total            int64                   `json:"total"`
	Errors           int64                   `json:"errors"`
	Cached           int64                   `json:"cached"`
	PromptTokens     int64                   `json:"prompt_tokens"`
	CompletionTokens int64                   `json:"completion_tokens"`
	CostCents        int64                   `json:"cost_cents"`
	LatencyP50Ms     int64                   `json:"latency_p50_ms"`
	LatencyP95Ms     int64                   `json:"latency_p95_ms"`
	ByProvider       map[string]ProviderStat `json:"by_provider"`
}

// ProviderStat is per-provider counters.
type ProviderStat struct {
	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors"`
	CostCents int64 `json:"cost_cents"`
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
		CostCents:        c.costCents,
		ByProvider:       map[string]ProviderStat{},
	}
	if n := len(c.latencies); n > 0 {
		sorted := make([]int64, n)
		copy(sorted, c.latencies)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		s.LatencyP50Ms = sorted[n*50/100]
		s.LatencyP95Ms = sorted[n*95/100]
	}
	for p, v := range c.byProvider {
		s.ByProvider[p] = ProviderStat{Requests: v.requests, Errors: v.errors, CostCents: v.costCents}
	}
	return s
}
