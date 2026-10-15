package observ

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
)

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

func New(enabled bool) *Collector {
	c := &Collector{
		byProvider:  map[string]*perProvider{},
		latCapacity: 512,
		start:       time.Now(),
	}
	c.enabled.Store(enabled)
	return c
}

func (c *Collector) Enabled() bool { return c.enabled.Load() }

func (c *Collector) SetEnabled(v bool) { c.enabled.Store(v) }

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

	LatencyWindow  string                  `json:"latency_window"`
	LatencySamples int                     `json:"latency_samples"`
	ByProvider     map[string]ProviderStat `json:"by_provider"`
}

type ProviderStat struct {
	Requests   int64 `json:"requests"`
	Errors     int64 `json:"errors"`
	CostMicros int64 `json:"cost_micros"`
}

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

	s.LatencyWindow = fmt.Sprintf("last_%d_requests", c.latCapacity)
	for p, v := range c.byProvider {
		s.ByProvider[p] = ProviderStat{Requests: v.requests, Errors: v.errors, CostMicros: v.costMicros}
	}
	return s
}
