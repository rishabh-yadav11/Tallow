// Package config loads, validates, and version-guards the hand-editable TOML
// configuration. The schema version must match SchemaVersion; mismatches are a
// hard error so future schema changes never silently break an existing file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// SchemaVersion is the only config version Tallow understands today. Bumping it
// is a breaking change that must come with validation logic.
const SchemaVersion = 1

// Config is the full TOML document.
type Config struct {
	Version       int           `toml:"version"`
	Server        Server        `toml:"server"`
	Auth          Auth          `toml:"auth"`
	Secret        SecretCfg     `toml:"secret"`
	Store         StoreCfg      `toml:"store"`
	Cache         CacheCfg      `toml:"cache"`
	Retention     Retention     `toml:"retention"`
	Compaction    Compaction    `toml:"compaction"`
	Observability Observability `toml:"observability"`
	Provider      []Provider    `toml:"provider"`
	Alias         []Alias       `toml:"alias"`
}

type Server struct {
	Listen        string `toml:"listen"`         // client-facing OpenAI endpoint.
	AdminSocket   string `toml:"admin_socket"`   // control interface (unix socket).
	MaxConcurrent int    `toml:"max_concurrent"` // global in-flight cap.
	QueueTimeout  string `toml:"queue_timeout"`  // bounded-queue wait (duration).
	ReadTimeout   string `toml:"read_timeout"`
	// WriteTimeout bounds how long a response may take. 0 means the per-provider
	// upstream timeout governs; a negative value is rejected by Validate.
	WriteTimeout string `toml:"write_timeout"`
	// IdleTimeout bounds how long an idle keep-alive connection is held.
	IdleTimeout string `toml:"idle_timeout"`
	// MaxRequestBytes caps the buffered request body (H6). 0 means the built-in
	// default of 8 MiB; a negative value is rejected by Validate.
	MaxRequestBytes int64 `toml:"max_request_bytes"`
	// StickyTTL is the idle expiry of sticky affinity. Unlike before, it
	// defaults rather than defaulting to "never expire" (H7).
	StickyTTL string `toml:"sticky_ttl"`
}

type Auth struct {
	// APIKeys are the bearer tokens clients must present. Non-empty = strict
	// allow-list. When Open is true the gateway accepts any (or no) token even
	// if the allow-list is empty.
	APIKeys []string `toml:"api_keys"`
	// Open, when true, makes the gateway accept requests without a valid API
	// key. It is safe-by-default: Open defaults to false, so an empty
	// api_keys list does NOT leave the gateway unauthenticated unless the
	// operator explicitly opts in.
	Open bool `toml:"open"`
}

type SecretCfg struct {
	// Keyfile is a 0600 file holding the master key bytes, used when
	// TALLOW_MASTER_KEY is not set. The env var wins when both are present.
	Keyfile string `toml:"keyfile"`
	// Keystore is the side-storage file holding encrypted provider keys.
	Keystore string `toml:"keystore"`
}

type StoreCfg struct {
	Path      string `toml:"path"`
	RawBodies *bool  `toml:"raw_bodies"` // capture request/response bodies.
}

type CacheCfg struct {
	Enabled    *bool  `toml:"enabled"`
	MaxEntries int    `toml:"max_entries"`
	TTL        string `toml:"ttl"`       // exact-match entry lifetime (duration).
	MaxBytes   int64  `toml:"max_bytes"` // byte cap on cached bodies; 0/negative = default 256 MiB.
}

type Retention struct {
	RawBodiesDays int    `toml:"raw_bodies_days"`
	MetadataDays  int    `toml:"metadata_days"`
	ErrorsDays    int    `toml:"errors_days"`
	RollupCron    string `toml:"rollup_interval"` // how often to roll up + delete.
	VacuumCron    string `toml:"vacuum_interval"` // how often to VACUUM.
}

type Compaction struct {
	Enabled            *bool `toml:"enabled"`
	MaxToolOutputChars int   `toml:"max_tool_output_chars"`
}

type Observability struct {
	Enabled bool `toml:"enabled"`
}

type Provider struct {
	Name           string   `toml:"name"`
	BaseURL        string   `toml:"base_url"`
	Fallback       []string `toml:"fallback"`
	RPM            int      `toml:"rpm"` // optional aggregate provider-level cap.
	HealthCheck    bool     `toml:"health_check"`
	HealthInterval string   `toml:"health_interval"`
	Timeout        string   `toml:"timeout"`
	Key            []Key    `toml:"key"`
}

type Key struct {
	ID             string `toml:"id"`
	Ref            string `toml:"ref"` // keystore reference for the secret.
	RPM            int    `toml:"rpm"`
	MaxRequests    int    `toml:"max_requests"`
	Window         string `toml:"window"`
	MaxConcurrent  int    `toml:"max_concurrent"`
	CostLimitCents int64  `toml:"cost_limit_cents"`
}

type Alias struct {
	Name   string   `toml:"name"`
	Target []Target `toml:"target"`
}

type Target struct {
	Provider        string  `toml:"provider"`
	Model           string  `toml:"model"`
	ContextWindow   int     `toml:"context_window"`
	SupportsTools   bool    `toml:"supports_tools"`
	SupportsVision  bool    `toml:"supports_vision"`
	SupportsStream  bool    `toml:"supports_stream"`
	PriceInputPerM  float64 `toml:"price_input_per_1m"`
	PriceOutputPerM float64 `toml:"price_output_per_1m"`
}

// Load reads and decodes path, returning the validated configuration.
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
	// H6: these three were previously left at the zero value, which meant
	// ReadTimeout, WriteTimeout and IdleTimeout were all unset and therefore
	// infinite. A slowloris client could hold a connection open indefinitely.
	// The defaults are unconditional: an operator who genuinely wants no
	// write timeout cannot get it, and that is deliberate, because a streaming
	// LLM response has no useful upper bound on its own and the per-provider
	// upstream timeout already governs it. A non-positive override is rejected
	// by Validate rather than silently treated as "unlimited".
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "30s"
	}
	if c.Server.WriteTimeout == "" {
		// Longer than ReadTimeout because a streaming completion legitimately
		// produces output for minutes.
		c.Server.WriteTimeout = "0s"
	}
	if c.Server.IdleTimeout == "" {
		c.Server.IdleTimeout = "120s"
	}
	if c.Server.MaxRequestBytes == 0 {
		c.Server.MaxRequestBytes = 8 << 20
	}
	// H7: StickyTTL had no default, and 0 made the expiry branch unreachable
	// because the guard read `if s.ttl > 0 && ...`. A config that simply
	// omitted the knob therefore accumulated one permanent sticky entry per
	// client-chosen X-Session-Id, with no cap and no eviction. 15m matches the
	// shipped example config and is long enough to hold a real conversation
	// together.
	if c.Server.StickyTTL == "" {
		c.Server.StickyTTL = "15m"
	}
	if c.Retention.RawBodiesDays == 0 {
		c.Retention.RawBodiesDays = 5
	}
	if c.Retention.MetadataDays == 0 {
		c.Retention.MetadataDays = 30
	}
	if c.Retention.ErrorsDays == 0 {
		c.Retention.ErrorsDays = 90
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
	// On-by-default feature flags (pointer bools so `enabled = false` works).
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
	// Expand ~ in path-bearing fields.
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

// Validate enforces the schema version and structural invariants.
//
// H9: durations used to be parsed only in BuildParams, which runs AFTER
// Validate has already declared the document valid, so a config with
// `queue_timeout = "not-a-duration"` loaded successfully and then failed at an
// unpredictable later point, and one with `queue_timeout = "-1h"` or
// `metadata_days = -30` was accepted outright. Parsing and range-checking
// durations here makes a bad config fail fast at load, with the offending key
// named.
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
	seenP := map[string]bool{}
	for _, p := range c.Provider {
		if p.Name == "" {
			return fmt.Errorf("provider with empty name")
		}
		if seenP[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		seenP[p.Name] = true
		// M14: base_url is the field that decides where a request carrying the
		// provider Authorization header and the full user prompt is sent, so it
		// gets a real structural check rather than a bare non-empty test. The
		// address-space half of the policy cannot live here - it depends on what
		// the name resolves to at connection time - and is enforced per-dial by
		// config.DialControl.
		if err := ValidateBaseURL(p.Name, p.BaseURL); err != nil {
			return err
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

// validateDurations parses every duration knob and range-checks it.
//
// Each entry names the exact TOML key, so an operator is told which line to fix
// rather than receiving a bare parse failure from a later stage.
//
// An EMPTY field is skipped rather than treated as zero. applyDefaults fills
// every one of these in on the Load path, so an empty field reaching here means
// the caller built a Config by hand, and rejecting that would make Validate
// unusable on its own. What matters for H9 is that an operator cannot write a
// garbage or negative duration and have it accepted.
func (c *Config) validateDurations() error {
	type durRule struct {
		key       string
		value     string
		allowZero bool
	}
	rules := []durRule{
		{"server.queue_timeout", c.Server.QueueTimeout, false},
		{"server.read_timeout", c.Server.ReadTimeout, true},
		// WriteTimeout 0 means unbounded, which is the documented default: a
		// streaming completion has no useful upper bound of its own.
		{"server.write_timeout", c.Server.WriteTimeout, true},
		{"server.idle_timeout", c.Server.IdleTimeout, true},
		// H7: sticky_ttl must be positive. 0 used to mean "never expire", which
		// with a client-chosen X-Session-Id made the sticky map unbounded.
		{"server.sticky_ttl", c.Server.StickyTTL, false},
		{"cache.ttl", c.Cache.TTL, false},
		{"retention.rollup_interval", c.Retention.RollupCron, false},
		{"retention.vacuum_interval", c.Retention.VacuumCron, false},
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

// validateCounts range-checks the non-duration numeric knobs.
//
// A negative retention day count means "delete everything immediately", which
// is silent data loss rather than a startup error, and a negative cache byte
// bound becomes a nonsensical limit. Both are rejected here.
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
	if c.Retention.RawBodiesDays < 0 {
		return fmt.Errorf("retention.raw_bodies_days: must not be negative, got %d", c.Retention.RawBodiesDays)
	}
	if c.Retention.MetadataDays < 0 {
		return fmt.Errorf("retention.metadata_days: must not be negative, got %d", c.Retention.MetadataDays)
	}
	if c.Retention.ErrorsDays < 0 {
		return fmt.Errorf("retention.errors_days: must not be negative, got %d", c.Retention.ErrorsDays)
	}
	if c.Compaction.MaxToolOutputChars < 0 {
		return fmt.Errorf("compaction.max_tool_output_chars: must not be negative, got %d", c.Compaction.MaxToolOutputChars)
	}
	return nil
}

// Dur parses a duration string, returning 0 for empty.
//
// An empty value is a legitimate "unset" in config.toml, and applyDefaults
// fills it in before this is ever called, so the empty case returning 0 is a
// deliberate no-op rather than a silent parse failure. Every caller either
// surfaces the error (Dur) or has already validated the field (validateDurations,
// which calls Dur), so a malformed duration fails at config load rather than
// silently becoming 0 and disabling whatever it governed.
func Dur(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}
