package config

import (
	"fmt"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/money"
)

// Build converts the validated TOML document into runtime model values.
// Key secrets are left empty here; the app resolves them via the keystore.
func (c *Config) Build() ([]model.Provider, []model.Alias, error) {
	provs := make([]model.Provider, 0, len(c.Provider))
	for _, p := range c.Provider {
		hi, err := Dur(p.HealthInterval)
		if err != nil {
			return nil, nil, fmt.Errorf("provider %q: health_interval: %w", p.Name, err)
		}
		to, err := Dur(p.Timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("provider %q: timeout: %w", p.Name, err)
		}
		mp := model.Provider{
			Name:           p.Name,
			BaseURL:        p.BaseURL,
			Fallback:       p.Fallback,
			HealthCheck:    p.HealthCheck,
			HealthInterval: hi,
			RPM:            p.RPM,
			Timeout:        to,
		}
		for _, k := range p.Key {
			w, err := Dur(k.Window)
			if err != nil {
				return nil, nil, fmt.Errorf("provider %q key %q: window: %w", p.Name, k.ID, err)
			}
			mp.Keys = append(mp.Keys, model.Key{
				ID:             k.ID,
				Ref:            k.Ref,
				RPM:            k.RPM,
				MaxRequests:    k.MaxRequests,
				Window:         w,
				MaxConcurrent:  k.MaxConcurrent,
				CostLimitCents: k.CostLimitCents,
				// The operator configures the cap in cents; the runtime
				// budget enforces it in micro-USD so sub-cent spend is
				// accounted instead of truncated away.
				CostLimitMicros: money.CentsToMicroUSD(k.CostLimitCents),
			})
		}
		provs = append(provs, mp)
	}

	aliases := make([]model.Alias, 0, len(c.Alias))
	for _, a := range c.Alias {
		ma := model.Alias{Name: a.Name}
		for _, t := range a.Target {
			ma.Targets = append(ma.Targets, model.Target{
				Provider:        t.Provider,
				Model:           t.Model,
				ContextWindow:   t.ContextWindow,
				SupportsTools:   t.SupportsTools,
				SupportsVision:  t.SupportsVision,
				SupportsStream:  t.SupportsStream,
				PriceInputPerM:  t.PriceInputPerM,
				PriceOutputPerM: t.PriceOutputPerM,
			})
		}
		aliases = append(aliases, ma)
	}
	return provs, aliases, nil
}

// Params bundles the parsed runtime knobs derived from config durations.
type Params struct {
	QueueTimeout time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	StickyTTL    time.Duration
	CacheTTL     time.Duration
	RollupEvery  time.Duration
	VacuumEvery  time.Duration
	MaxBodyBytes int64
}

// BuildParams parses the duration-derived runtime knobs.
//
// Every duration is also range-checked here rather than only being parsed, so a
// negative timeout cannot reach the HTTP server (H9). Validate rejects these
// documents earlier with a clearer message; this second check keeps
// BuildParams safe for any caller that constructs a Config directly.
func (c *Config) BuildParams() (Params, error) {
	q, err := Dur(c.Server.QueueTimeout)
	if err != nil {
		return Params{}, fmt.Errorf("queue_timeout: %w", err)
	}
	if q <= 0 {
		return Params{}, fmt.Errorf("queue_timeout: must be positive, got %s", q)
	}
	rt, err := Dur(c.Server.ReadTimeout)
	if err != nil {
		return Params{}, fmt.Errorf("read_timeout: %w", err)
	}
	if rt < 0 {
		return Params{}, fmt.Errorf("read_timeout: must not be negative, got %s", rt)
	}
	// WriteTimeout 0 means "unbounded", which is the documented default and is
	// correct for a streaming LLM response. Only a negative value is rejected.
	wt, err := Dur(c.Server.WriteTimeout)
	if err != nil {
		return Params{}, fmt.Errorf("write_timeout: %w", err)
	}
	if wt < 0 {
		return Params{}, fmt.Errorf("write_timeout: must not be negative, got %s", wt)
	}
	idle, err := Dur(c.Server.IdleTimeout)
	if err != nil {
		return Params{}, fmt.Errorf("idle_timeout: %w", err)
	}
	if idle < 0 {
		return Params{}, fmt.Errorf("idle_timeout: must not be negative, got %s", idle)
	}
	ttl, err := Dur(c.Cache.TTL)
	if err != nil {
		return Params{}, fmt.Errorf("cache ttl: %w", err)
	}
	if ttl <= 0 {
		return Params{}, fmt.Errorf("cache ttl: must be positive, got %s", ttl)
	}
	// H7: a sticky TTL of 0 used to mean "never expire", which combined with a
	// client-chosen X-Session-Id header to make the sticky map an unbounded
	// write-only structure. It is rejected outright, and the default is set in
	// applyDefaults.
	sticky, err := Dur(c.Server.StickyTTL)
	if err != nil {
		return Params{}, fmt.Errorf("sticky_ttl: %w", err)
	}
	if sticky <= 0 {
		return Params{}, fmt.Errorf("sticky_ttl: must be positive, got %s (0 would mean never expire)", sticky)
	}
	rollup, err := Dur(c.Retention.RollupCron)
	if err != nil {
		return Params{}, fmt.Errorf("rollup_interval: %w", err)
	}
	if rollup <= 0 {
		return Params{}, fmt.Errorf("rollup_interval: must be positive, got %s", rollup)
	}
	vacuum, err := Dur(c.Retention.VacuumCron)
	if err != nil {
		return Params{}, fmt.Errorf("vacuum_interval: %w", err)
	}
	if vacuum <= 0 {
		return Params{}, fmt.Errorf("vacuum_interval: must be positive, got %s", vacuum)
	}
	if c.Server.MaxRequestBytes < 0 {
		return Params{}, fmt.Errorf("max_request_bytes: must not be negative, got %d", c.Server.MaxRequestBytes)
	}
	return Params{
		QueueTimeout: q,
		ReadTimeout:  rt,
		WriteTimeout: wt,
		IdleTimeout:  idle,
		StickyTTL:    sticky,
		CacheTTL:     ttl,
		RollupEvery:  rollup,
		VacuumEvery:  vacuum,
		MaxBodyBytes: c.Server.MaxRequestBytes,
	}, nil
}
