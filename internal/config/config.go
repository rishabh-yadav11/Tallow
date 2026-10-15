package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const SchemaVersion = 1

type Config struct {
	Version       int           `toml:"version" json:"version"`
	Server        Server        `toml:"server" json:"server"`
	Auth          Auth          `toml:"auth" json:"auth"`
	Secret        SecretCfg     `toml:"secret" json:"secret"`
	Store         StoreCfg      `toml:"store" json:"store"`
	Cache         CacheCfg      `toml:"cache" json:"cache"`
	Retention     Retention     `toml:"retention" json:"retention"`
	Compaction    Compaction    `toml:"compaction" json:"compaction"`
	Observability Observability `toml:"observability" json:"observability"`
	Routing       Routing       `toml:"routing" json:"routing"`
	Provider      []Provider    `toml:"provider" json:"provider"`
	Alias         []Alias       `toml:"alias" json:"alias"`
}

type Routing struct {
	Strategy string `toml:"strategy" json:"strategy"`

	RequestRetry int `toml:"request_retry" json:"request_retry"`

	MaxRetryCredentials int `toml:"max_retry_credentials" json:"max_retry_credentials"`

	MaxRetryInterval string `toml:"max_retry_interval" json:"max_retry_interval"`

	CooldownFile string `toml:"cooldown_file" json:"cooldown_file"`

	SessionAffinity bool `toml:"session_affinity" json:"session_affinity"`

	SessionAffinityTTL string `toml:"session_affinity_ttl" json:"session_affinity_ttl"`

	SessionAffinitySubagents bool `toml:"session_affinity_subagents" json:"session_affinity_subagents"`

	SessionAffinityRebindOn string `toml:"session_affinity_rebind_on" json:"session_affinity_rebind_on"`
}

type Server struct {
	Listen        string `toml:"listen" json:"listen"`
	AdminSocket   string `toml:"admin_socket" json:"admin_socket"`
	MaxConcurrent int    `toml:"max_concurrent" json:"max_concurrent"`
	QueueTimeout  string `toml:"queue_timeout" json:"queue_timeout"`
	ReadTimeout   string `toml:"read_timeout" json:"read_timeout"`

	WriteTimeout string `toml:"write_timeout" json:"write_timeout"`

	IdleTimeout string `toml:"idle_timeout" json:"idle_timeout"`

	MaxRequestBytes int64 `toml:"max_request_bytes" json:"max_request_bytes"`

	StickyTTL string `toml:"sticky_ttl" json:"sticky_ttl"`

	TLSCert         string `toml:"tls_cert" json:"tls_cert"`
	TLSKey          string `toml:"tls_key" json:"tls_key"`
	StreamKeepalive string `toml:"stream_keepalive" json:"stream_keepalive"`
}

type Auth struct {
	APIKeys []string `toml:"api_keys" json:"api_keys"`

	Open bool `toml:"open" json:"open"`
}

type SecretCfg struct {
	Keyfile string `toml:"keyfile" json:"keyfile"`

	Keystore string `toml:"keystore" json:"keystore"`
}

type StoreCfg struct {
	Path      string `toml:"path" json:"path"`
	RawBodies *bool  `toml:"raw_bodies" json:"raw_bodies"`
}

type CacheCfg struct {
	Enabled    *bool  `toml:"enabled" json:"enabled"`
	MaxEntries int    `toml:"max_entries" json:"max_entries"`
	TTL        string `toml:"ttl" json:"ttl"`
	MaxBytes   int64  `toml:"max_bytes" json:"max_bytes"`
}

type Retention struct {
	RawBodiesDays *int   `toml:"raw_bodies_days" json:"raw_bodies_days"`
	MetadataDays  *int   `toml:"metadata_days" json:"metadata_days"`
	ErrorsDays    *int   `toml:"errors_days" json:"errors_days"`
	RollupCron    string `toml:"rollup_interval" json:"rollup_interval"`
	VacuumCron    string `toml:"vacuum_interval" json:"vacuum_interval"`
}

type Compaction struct {
	Enabled            *bool `toml:"enabled" json:"enabled"`
	MaxToolOutputChars int   `toml:"max_tool_output_chars" json:"max_tool_output_chars"`
}

type Observability struct {
	Enabled bool `toml:"enabled" json:"enabled"`
}

type Provider struct {
	Name           string   `toml:"name" json:"name"`
	BaseURL        string   `toml:"base_url" json:"base_url"`
	Fallback       []string `toml:"fallback" json:"fallback"`
	RPM            int      `toml:"rpm" json:"rpm"`
	HealthCheck    bool     `toml:"health_check" json:"health_check"`
	HealthInterval string   `toml:"health_interval" json:"health_interval"`
	Timeout        string   `toml:"timeout" json:"timeout"`
	Key            []Key    `toml:"key" json:"key"`

	ProxyURL string `toml:"proxy_url" json:"proxy_url"`

	Disabled bool `toml:"disabled" json:"disabled"`

	Headers map[string]string `toml:"headers" json:"headers"`

	ExcludedModels []string `toml:"excluded_models" json:"excluded_models"`

	Catalog bool `toml:"catalog" json:"catalog"`

	RequestRetry *int `toml:"request_retry" json:"request_retry"`

	DisableCooling bool `toml:"disable_cooling" json:"disable_cooling"`
}

type Key struct {
	ID             string `toml:"id" json:"id"`
	Ref            string `toml:"ref" json:"ref"`
	RPM            int    `toml:"rpm" json:"rpm"`
	MaxRequests    int    `toml:"max_requests" json:"max_requests"`
	Window         string `toml:"window" json:"window"`
	MaxConcurrent  int    `toml:"max_concurrent" json:"max_concurrent"`
	CostLimitCents int64  `toml:"cost_limit_cents" json:"cost_limit_cents"`

	Weight   int `toml:"weight" json:"weight"`
	Priority int `toml:"priority" json:"priority"`

	Headers map[string]string `toml:"headers" json:"headers"`

	ProxyURL string `toml:"proxy_url" json:"proxy_url"`

	RequestRetry *int `toml:"request_retry" json:"request_retry"`

	DisableCooling *bool `toml:"disable_cooling" json:"disable_cooling"`

	ErrorRules []ErrorRule `toml:"error_rules" json:"error_rules"`
}

type ErrorRule struct {
	Status      int      `toml:"status" json:"status"`
	Match       []string `toml:"match" json:"match"`
	MatchRegexp []string `toml:"match_regexp" json:"match_regexp"`

	Action string `toml:"action" json:"action"`
}

type Alias struct {
	Name   string   `toml:"name" json:"name"`
	Target []Target `toml:"target" json:"target"`

	DisplayName string `toml:"display_name" json:"display_name"`

	ExcludedModels []string `toml:"excluded_models" json:"excluded_models"`
}

type Target struct {
	Provider      string `toml:"provider" json:"provider"`
	Model         string `toml:"model" json:"model"`
	ContextWindow int    `toml:"context_window" json:"context_window"`

	MaxContextLength int     `toml:"max_context_length" json:"max_context_length"`
	SupportsTools    bool    `toml:"supports_tools" json:"supports_tools"`
	SupportsVision   bool    `toml:"supports_vision" json:"supports_vision"`
	SupportsStream   bool    `toml:"supports_stream" json:"supports_stream"`
	PriceInputPerM   float64 `toml:"price_input_per_1m" json:"price_input_per_1m"`
	PriceOutputPerM  float64 `toml:"price_output_per_1m" json:"price_output_per_1m"`

	ForceMapping bool `toml:"force_mapping" json:"force_mapping"`

	DisplayName string `toml:"display_name" json:"display_name"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8080"
	}
	if c.Server.MaxConcurrent == 0 {
		c.Server.MaxConcurrent = 16
	}
	if c.Server.QueueTimeout == "" {
		c.Server.QueueTimeout = "30s"
	}
	if c.Server.AdminSocket == "" {
		c.Server.AdminSocket = defaultAdminSocket()
	}

	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "30s"
	}
	if c.Server.WriteTimeout == "" {

		c.Server.WriteTimeout = "0s"
	}
	if c.Server.IdleTimeout == "" {
		c.Server.IdleTimeout = "120s"
	}
	if c.Server.MaxRequestBytes == 0 {
		c.Server.MaxRequestBytes = 8 << 20
	}

	if c.Server.StickyTTL == "" {
		c.Server.StickyTTL = "15m"
	}

	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "round-robin"
	}

	for ai := range c.Alias {
		for ti := range c.Alias[ai].Target {
			if c.Alias[ai].Target[ti].ContextWindow == 0 && c.Alias[ai].Target[ti].MaxContextLength > 0 {
				c.Alias[ai].Target[ti].ContextWindow = c.Alias[ai].Target[ti].MaxContextLength
			}
		}
	}

	if c.Retention.RawBodiesDays == nil {
		v := 5
		c.Retention.RawBodiesDays = &v
	}
	if c.Retention.MetadataDays == nil {
		v := 30
		c.Retention.MetadataDays = &v
	}
	if c.Retention.ErrorsDays == nil {
		v := 90
		c.Retention.ErrorsDays = &v
	}
	if c.Retention.RollupCron == "" {
		c.Retention.RollupCron = "1h"
	}
	if c.Retention.VacuumCron == "" {
		c.Retention.VacuumCron = "6h"
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = 1024
	}
	if c.Cache.TTL == "" {
		c.Cache.TTL = "10m"
	}
	if c.Cache.MaxBytes == 0 {
		c.Cache.MaxBytes = 256 * 1024 * 1024
	}
	if c.Compaction.MaxToolOutputChars == 0 {
		c.Compaction.MaxToolOutputChars = 8000
	}

	if c.Cache.Enabled == nil {
		v := true
		c.Cache.Enabled = &v
	}
	if c.Compaction.Enabled == nil {
		v := true
		c.Compaction.Enabled = &v
	}
	if c.Store.RawBodies == nil {
		v := true
		c.Store.RawBodies = &v
	}

	c.Server.AdminSocket = tilde(c.Server.AdminSocket)
	c.Secret.Keyfile = tilde(c.Secret.Keyfile)
	c.Secret.Keystore = tilde(c.Secret.Keystore)
	c.Store.Path = tilde(c.Store.Path)
	return nil
}

func tilde(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func defaultAdminSocket() string {
	if d, err := os.UserHomeDir(); err == nil && d != "" {
		return d + "/.tallow/admin.sock"
	}
	return "tallow-admin.sock"
}

func (c *Config) Validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("unsupported config version %d (want %d)", c.Version, SchemaVersion)
	}
	if err := c.validateDurations(); err != nil {
		return err
	}
	if err := c.validateCounts(); err != nil {
		return err
	}
	if err := c.validateRouting(); err != nil {
		return err
	}
	if err := c.validateTLS(); err != nil {
		return err
	}
	seenP := map[string]bool{}
	for _, p := range c.Provider {
		if p.Name == "" {
			return fmt.Errorf("provider with empty name")
		}
		if seenP[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		seenP[p.Name] = true

		if err := ValidateBaseURL(p.Name, p.BaseURL); err != nil {
			return err
		}
		if err := validateProxyURL(p.ProxyURL); err != nil {
			return fmt.Errorf("provider %q: proxy_url: %w", p.Name, err)
		}
		seenK := map[string]bool{}
		for _, k := range p.Key {
			if k.ID == "" {
				return fmt.Errorf("provider %q: key with empty id", p.Name)
			}
			if seenK[k.ID] {
				return fmt.Errorf("provider %q: duplicate key %q", p.Name, k.ID)
			}
			seenK[k.ID] = true
			if k.RPM < 0 || k.MaxRequests < 0 {
				return fmt.Errorf("provider %q key %q: negative limit", p.Name, k.ID)
			}
			if k.Weight < 0 {
				return fmt.Errorf("provider %q key %q: weight: must not be negative, got %d", p.Name, k.ID, k.Weight)
			}
			if k.Priority < 0 {
				return fmt.Errorf("provider %q key %q: priority: must not be negative, got %d", p.Name, k.ID, k.Priority)
			}
			if err := validateProxyURL(k.ProxyURL); err != nil {
				return fmt.Errorf("provider %q key %q: proxy_url: %w", p.Name, k.ID, err)
			}
			for ri, r := range k.ErrorRules {
				if err := validateErrorRule(r); err != nil {
					return fmt.Errorf("provider %q key %q: error rule %d: %w", p.Name, k.ID, ri, err)
				}
			}
		}
	}
	seenA := map[string]bool{}
	for _, a := range c.Alias {
		if a.Name == "" {
			return fmt.Errorf("alias with empty name")
		}
		if seenA[a.Name] {
			return fmt.Errorf("duplicate alias %q", a.Name)
		}
		seenA[a.Name] = true
		if len(a.Target) == 0 {
			return fmt.Errorf("alias %q: at least one target required", a.Name)
		}
		for _, t := range a.Target {
			if !seenP[t.Provider] {
				return fmt.Errorf("alias %q: unknown provider %q", a.Name, t.Provider)
			}
			if t.Model == "" {
				return fmt.Errorf("alias %q: target for provider %q missing model", a.Name, t.Provider)
			}
		}
	}
	return nil
}

func (c *Config) validateRouting() error {
	switch c.Routing.Strategy {
	case "", "round-robin", "weighted-round-robin", "fill-first":
	default:
		return fmt.Errorf("routing.strategy: invalid value %q (want round-robin, weighted-round-robin, or fill-first)", c.Routing.Strategy)
	}
	switch c.Routing.SessionAffinityRebindOn {
	case "", "any", "quota":
	default:
		return fmt.Errorf("routing.session_affinity_rebind_on: invalid value %q (want any or quota)", c.Routing.SessionAffinityRebindOn)
	}
	return nil
}

func (c *Config) validateTLS() error {
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		return fmt.Errorf("server.tls_cert and server.tls_key must be set together")
	}
	return nil
}

var proxySchemes = map[string]bool{
	"socks5": true, "socks5h": true, "socks": true, "http": true, "https": true,
}

func validateProxyURL(s string) error {
	if s == "" || s == "direct" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", s, err)
	}
	if !proxySchemes[u.Scheme] {
		return fmt.Errorf("unsupported scheme %q (want socks5, socks5h, socks, http, or https)", u.Scheme)
	}
	return nil
}

var errorActions = map[string]bool{
	"stop": true, "stop-and-cooldown": true, "continue": true, "continue-and-cooldown": true,
}

func validateErrorRule(r ErrorRule) error {
	if !errorActions[r.Action] {
		return fmt.Errorf("action %q (want stop, stop-and-cooldown, continue, or continue-and-cooldown)", r.Action)
	}
	if r.Status != 0 && (r.Status < 400 || r.Status > 599) {
		return fmt.Errorf("status %d out of range (0 means any, or 400..599)", r.Status)
	}
	for _, pat := range r.MatchRegexp {
		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("match_regexp %q does not compile: %w", pat, err)
		}
	}
	return nil
}

func (c *Config) validateDurations() error {
	type durRule struct {
		key       string
		value     string
		allowZero bool
	}
	rules := []durRule{
		{"server.queue_timeout", c.Server.QueueTimeout, false},
		{"server.read_timeout", c.Server.ReadTimeout, true},

		{"server.write_timeout", c.Server.WriteTimeout, true},
		{"server.idle_timeout", c.Server.IdleTimeout, true},

		{"server.sticky_ttl", c.Server.StickyTTL, false},

		{"server.stream_keepalive", c.Server.StreamKeepalive, true},
		{"cache.ttl", c.Cache.TTL, false},
		{"retention.rollup_interval", c.Retention.RollupCron, false},
		{"retention.vacuum_interval", c.Retention.VacuumCron, false},
		{"routing.max_retry_interval", c.Routing.MaxRetryInterval, true},
		{"routing.session_affinity_ttl", c.Routing.SessionAffinityTTL, true},
	}
	for _, r := range rules {
		if r.value == "" {
			continue
		}
		d, err := Dur(r.value)
		if err != nil {
			return fmt.Errorf("%s: %w", r.key, err)
		}
		if d < 0 {
			return fmt.Errorf("%s: must not be negative, got %s", r.key, d)
		}
		if d == 0 && !r.allowZero {
			return fmt.Errorf("%s: must be positive, got 0", r.key)
		}
	}
	for _, p := range c.Provider {
		if p.HealthInterval != "" {
			if _, err := Dur(p.HealthInterval); err != nil {
				return fmt.Errorf("provider %q: health_interval: %w", p.Name, err)
			}
		}
		if p.Timeout != "" {
			d, err := Dur(p.Timeout)
			if err != nil {
				return fmt.Errorf("provider %q: timeout: %w", p.Name, err)
			}
			if d <= 0 {
				return fmt.Errorf("provider %q: timeout: must be positive, got %s", p.Name, d)
			}
		}
		for ki, k := range p.Key {
			if k.Window == "" {
				continue
			}
			d, err := Dur(k.Window)
			if err != nil {
				return fmt.Errorf("provider %q key %q: window: %w", p.Name, k.ID, err)
			}
			if d <= 0 {
				return fmt.Errorf("provider %q key %q (%d): window: must be positive, got %s", p.Name, k.ID, ki, d)
			}
		}
	}
	return nil
}

func (c *Config) validateCounts() error {
	if c.Server.MaxConcurrent < 0 {
		return fmt.Errorf("server.max_concurrent: must not be negative, got %d", c.Server.MaxConcurrent)
	}
	if c.Server.MaxRequestBytes < 0 {
		return fmt.Errorf("server.max_request_bytes: must not be negative, got %d", c.Server.MaxRequestBytes)
	}
	if c.Cache.MaxEntries < 0 {
		return fmt.Errorf("cache.max_entries: must not be negative, got %d", c.Cache.MaxEntries)
	}
	if c.Cache.MaxBytes < 0 {
		return fmt.Errorf("cache.max_bytes: must not be negative, got %d", c.Cache.MaxBytes)
	}
	if dayCount(c.Retention.RawBodiesDays) < 0 {
		return fmt.Errorf("retention.raw_bodies_days: must not be negative, got %d", dayCount(c.Retention.RawBodiesDays))
	}
	if dayCount(c.Retention.MetadataDays) < 0 {
		return fmt.Errorf("retention.metadata_days: must not be negative, got %d", dayCount(c.Retention.MetadataDays))
	}
	if dayCount(c.Retention.ErrorsDays) < 0 {
		return fmt.Errorf("retention.errors_days: must not be negative, got %d", dayCount(c.Retention.ErrorsDays))
	}
	if c.Compaction.MaxToolOutputChars < 0 {
		return fmt.Errorf("compaction.max_tool_output_chars: must not be negative, got %d", c.Compaction.MaxToolOutputChars)
	}
	return nil
}

func dayCount(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func Dur(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}
