package config

import (
	"fmt"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/model"
	"github.com/rishabh-yadav11/tallow/internal/money"
)

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
			ProxyURL:       p.ProxyURL,
			Disabled:       p.Disabled,
			Headers:        p.Headers,
			ExcludedModels: p.ExcludedModels,
			Catalog:        p.Catalog,
			RequestRetry:   p.RequestRetry,
			DisableCooling: p.DisableCooling,
		}
		for _, k := range p.Key {
			w, err := Dur(k.Window)
			if err != nil {
				return nil, nil, fmt.Errorf("provider %q key %q: window: %w", p.Name, k.ID, err)
			}
			mk := model.Key{
				ID:             k.ID,
				Ref:            k.Ref,
				RPM:            k.RPM,
				MaxRequests:    k.MaxRequests,
				Window:         w,
				MaxConcurrent:  k.MaxConcurrent,
				CostLimitCents: k.CostLimitCents,

				CostLimitMicros: money.CentsToMicroUSD(k.CostLimitCents),
				Weight:          k.Weight,
				Priority:        k.Priority,
				Headers:         k.Headers,
				ProxyURL:        k.ProxyURL,
				RequestRetry:    k.RequestRetry,
				ErrorRules:      copyErrorRules(k.ErrorRules),
			}

			dc := p.DisableCooling
			if k.DisableCooling != nil {
				dc = *k.DisableCooling
			}
			mk.DisableCooling = dc
			mp.Keys = append(mp.Keys, mk)
		}
		provs = append(provs, mp)
	}

	aliases := make([]model.Alias, 0, len(c.Alias))
	for _, a := range c.Alias {
		ma := model.Alias{
			Name:           a.Name,
			DisplayName:    a.DisplayName,
			ExcludedModels: a.ExcludedModels,
		}
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
				ForceMapping:    t.ForceMapping,
				DisplayName:     t.DisplayName,
			})
		}
		aliases = append(aliases, ma)
	}
	return provs, aliases, nil
}

func copyErrorRules(in []ErrorRule) []model.ErrorRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.ErrorRule, len(in))
	for i, r := range in {
		out[i] = model.ErrorRule{
			Status:      r.Status,
			Match:       r.Match,
			MatchRegexp: r.MatchRegexp,
			Action:      r.Action,
		}
	}
	return out
}

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

	Strategy                 string
	RequestRetry             int
	MaxRetryCredentials      int
	MaxRetryInterval         time.Duration
	StreamKeepalive          time.Duration
	SessionAffinityEnabled   bool
	SessionAffinityTTL       time.Duration
	SessionAffinitySubagents bool
	SessionAffinityRebindOn  string
}

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

	mri, err := Dur(c.Routing.MaxRetryInterval)
	if err != nil {
		return Params{}, fmt.Errorf("routing.max_retry_interval: %w", err)
	}
	if mri < 0 {
		return Params{}, fmt.Errorf("routing.max_retry_interval: must not be negative, got %s", mri)
	}
	sk, err := Dur(c.Server.StreamKeepalive)
	if err != nil {
		return Params{}, fmt.Errorf("server.stream_keepalive: %w", err)
	}
	if sk < 0 {
		return Params{}, fmt.Errorf("server.stream_keepalive: must not be negative, got %s", sk)
	}
	saTTL, err := Dur(c.Routing.SessionAffinityTTL)
	if err != nil {
		return Params{}, fmt.Errorf("routing.session_affinity_ttl: %w", err)
	}
	if saTTL < 0 {
		return Params{}, fmt.Errorf("routing.session_affinity_ttl: must not be negative, got %s", saTTL)
	}
	strategy := c.Routing.Strategy
	if strategy == "" {
		strategy = "round-robin"
	}
	return Params{
		QueueTimeout:             q,
		ReadTimeout:              rt,
		WriteTimeout:             wt,
		IdleTimeout:              idle,
		StickyTTL:                sticky,
		CacheTTL:                 ttl,
		RollupEvery:              rollup,
		VacuumEvery:              vacuum,
		MaxBodyBytes:             c.Server.MaxRequestBytes,
		Strategy:                 strategy,
		RequestRetry:             c.Routing.RequestRetry,
		MaxRetryCredentials:      c.Routing.MaxRetryCredentials,
		MaxRetryInterval:         mri,
		StreamKeepalive:          sk,
		SessionAffinityEnabled:   c.Routing.SessionAffinity,
		SessionAffinityTTL:       saTTL,
		SessionAffinitySubagents: c.Routing.SessionAffinitySubagents,
		SessionAffinityRebindOn:  c.Routing.SessionAffinityRebindOn,
	}, nil
}
