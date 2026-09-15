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
	StickyTTL     string `toml:"sticky_ttl"` // idle expiry of sticky affinity.
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
	MaxBytes   int64  `toml:"max_bytes"` // upper bound on cached bytes (<=0 unbounded).
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
func (c *Config) Validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("unsupported config version %d (want %d)", c.Version, SchemaVersion)
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
		if p.BaseURL == "" {
			return fmt.Errorf("provider %q: base_url required", p.Name)
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

// Dur parses a duration string, returning 0 for empty.
func Dur(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// DurOrZero is a lenient variant used where a bad duration should not abort
// startup silently in non-validated paths (kept for symmetry).
func DurOrZero(s string) time.Duration {
	d, err := Dur(s)
	if err != nil {
		return 0
	}
	return d
}
