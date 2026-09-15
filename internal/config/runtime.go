package config

import (
	"fmt"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
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
	StickyTTL    time.Duration
	CacheTTL     time.Duration
	RollupEvery  time.Duration
	VacuumEvery  time.Duration
}

// BuildParams parses the duration-derived runtime knobs.
func (c *Config) BuildParams() (Params, error) {
	q, err := Dur(c.Server.QueueTimeout)
	if err != nil {
		return Params{}, fmt.Errorf("queue_timeout: %w", err)
	}
	rt, err := Dur(c.Server.ReadTimeout)
	if err != nil {
		return Params{}, fmt.Errorf("read_timeout: %w", err)
	}
	ttl, err := Dur(c.Cache.TTL)
	if err != nil {
		return Params{}, fmt.Errorf("cache ttl: %w", err)
	}
	sticky, err := Dur(c.Server.StickyTTL)
	if err != nil {
		return Params{}, fmt.Errorf("sticky_ttl: %w", err)
	}
	rollup, err := Dur(c.Retention.RollupCron)
	if err != nil {
		return Params{}, fmt.Errorf("rollup_interval: %w", err)
	}
	vacuum, err := Dur(c.Retention.VacuumCron)
	if err != nil {
		return Params{}, fmt.Errorf("vacuum_interval: %w", err)
	}
	return Params{
		QueueTimeout: q,
		ReadTimeout:  rt,
		StickyTTL:    sticky,
		CacheTTL:     ttl,
		RollupEvery:  rollup,
		VacuumEvery:  vacuum,
	}, nil
}
