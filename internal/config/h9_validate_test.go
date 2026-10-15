package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseConfig() Config {
	c := Config{Version: SchemaVersion}
	c.Provider = []Provider{{
		Name:    "p1",
		BaseURL: "https://api.example.com",
		Key:     []Key{{ID: "k1"}},
	}}
	c.Alias = []Alias{{
		Name:   "gpt-4",
		Target: []Target{{Provider: "p1", Model: "m"}},
	}}
	return c
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tallow.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestH9ValidateRejectsBadDurations(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		wantK string
	}{
		{
			name:  "garbage queue timeout",
			mut:   func(c *Config) { c.Server.QueueTimeout = "not-a-duration" },
			wantK: "server.queue_timeout",
		},
		{
			name:  "negative queue timeout",
			mut:   func(c *Config) { c.Server.QueueTimeout = "-1h" },
			wantK: "server.queue_timeout",
		},
		{
			name:  "garbage read timeout",
			mut:   func(c *Config) { c.Server.ReadTimeout = "soon" },
			wantK: "server.read_timeout",
		},
		{
			name:  "negative read timeout",
			mut:   func(c *Config) { c.Server.ReadTimeout = "-5m" },
			wantK: "server.read_timeout",
		},
		{
			name:  "garbage write timeout",
			mut:   func(c *Config) { c.Server.WriteTimeout = "later" },
			wantK: "server.write_timeout",
		},
		{
			name:  "negative write timeout",
			mut:   func(c *Config) { c.Server.WriteTimeout = "-1s" },
			wantK: "server.write_timeout",
		},
		{
			name:  "garbage idle timeout",
			mut:   func(c *Config) { c.Server.IdleTimeout = "whenever" },
			wantK: "server.idle_timeout",
		},
		{
			name:  "negative idle timeout",
			mut:   func(c *Config) { c.Server.IdleTimeout = "-1s" },
			wantK: "server.idle_timeout",
		},
		{
			name:  "garbage sticky ttl",
			mut:   func(c *Config) { c.Server.StickyTTL = "ages" },
			wantK: "server.sticky_ttl",
		},
		{
			name:  "negative sticky ttl",
			mut:   func(c *Config) { c.Server.StickyTTL = "-15m" },
			wantK: "server.sticky_ttl",
		},
		{

			name:  "zero sticky ttl means never expire",
			mut:   func(c *Config) { c.Server.StickyTTL = "0s" },
			wantK: "server.sticky_ttl",
		},
		{
			name:  "garbage cache ttl",
			mut:   func(c *Config) { c.Cache.TTL = "a while" },
			wantK: "cache.ttl",
		},
		{
			name:  "zero cache ttl",
			mut:   func(c *Config) { c.Cache.TTL = "0s" },
			wantK: "cache.ttl",
		},
		{
			name:  "negative cache ttl",
			mut:   func(c *Config) { c.Cache.TTL = "-1h" },
			wantK: "cache.ttl",
		},
		{
			name:  "garbage rollup cron",
			mut:   func(c *Config) { c.Retention.RollupCron = "nightly" },
			wantK: "retention.rollup_interval",
		},
		{
			name:  "negative rollup cron",
			mut:   func(c *Config) { c.Retention.RollupCron = "-1h" },
			wantK: "retention.rollup_interval",
		},
		{
			name:  "garbage vacuum cron",
			mut:   func(c *Config) { c.Retention.VacuumCron = "sometimes" },
			wantK: "retention.vacuum_interval",
		},
		{
			name:  "negative vacuum cron",
			mut:   func(c *Config) { c.Retention.VacuumCron = "-1h" },
			wantK: "retention.vacuum_interval",
		},
		{
			name:  "garbage provider health interval",
			mut:   func(c *Config) { c.Provider[0].HealthInterval = "often" },
			wantK: "health_interval",
		},
		{
			name:  "negative provider timeout",
			mut:   func(c *Config) { c.Provider[0].Timeout = "-30s" },
			wantK: "timeout",
		},
		{
			name:  "zero provider timeout",
			mut:   func(c *Config) { c.Provider[0].Timeout = "0s" },
			wantK: "timeout",
		},
		{
			name:  "negative key window",
			mut:   func(c *Config) { c.Provider[0].Key[0].Window = "-1m" },
			wantK: "window",
		},
		{
			name:  "zero key window",
			mut:   func(c *Config) { c.Provider[0].Key[0].Window = "0s" },
			wantK: "window",
		},
		{
			name:  "garbage key window",
			mut:   func(c *Config) { c.Provider[0].Key[0].Window = "sometimes" },
			wantK: "window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("H9 NOT FIXED: Validate accepted an invalid duration; the error only surfaces later in BuildParams")
			}
			if !strings.Contains(err.Error(), tc.wantK) {
				t.Fatalf("Validate rejected the config but the error does not name %q, so an operator cannot tell which line to fix: %v", tc.wantK, err)
			}
		})
	}
}

func TestH9ValidateRejectsNegativeCounts(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		wantK string
	}{
		{"negative max_concurrent", func(c *Config) { c.Server.MaxConcurrent = -8 }, "server.max_concurrent"},
		{"negative max_request_bytes", func(c *Config) { c.Server.MaxRequestBytes = -1 }, "server.max_request_bytes"},
		{"negative cache max_entries", func(c *Config) { c.Cache.MaxEntries = -1 }, "cache.max_entries"},
		{"negative cache max_bytes", func(c *Config) { c.Cache.MaxBytes = -1 }, "cache.max_bytes"},

		{"negative raw_bodies_days", func(c *Config) { v := -30; c.Retention.RawBodiesDays = &v }, "retention.raw_bodies_days"},
		{"negative metadata_days", func(c *Config) { v := -30; c.Retention.MetadataDays = &v }, "retention.metadata_days"},
		{"negative errors_days", func(c *Config) { v := -1; c.Retention.ErrorsDays = &v }, "retention.errors_days"},
		{"negative max_tool_output_chars", func(c *Config) { c.Compaction.MaxToolOutputChars = -1 }, "compaction.max_tool_output_chars"},
		{"negative key rpm", func(c *Config) { c.Provider[0].Key[0].RPM = -1 }, "negative limit"},
		{"negative key max_requests", func(c *Config) { c.Provider[0].Key[0].MaxRequests = -1 }, "negative limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("H9 NOT FIXED: Validate accepted a negative value")
			}
			if !strings.Contains(err.Error(), tc.wantK) {
				t.Fatalf("error does not name %q: %v", tc.wantK, err)
			}
		})
	}
}

func TestH9BadDurationFailsAtLoadNotLater(t *testing.T) {
	body := `version = 1

[server]
queue_timeout = "not-a-duration"

[[provider]]
name = "p1"
base_url = "https://api.example.com"
  [[provider.key]]
  id = "k1"

[[alias]]
name = "gpt-4"
  [[alias.target]]
  provider = "p1"
  model = "m"
`
	path := writeConfig(t, body)
	cfg, err := Load(path)
	if err == nil {

		if verr := cfg.Validate(); verr == nil {
			t.Fatalf("H9 NOT FIXED: both Load and Validate accepted queue_timeout = %q", "not-a-duration")
		}
		return
	}
	if !strings.Contains(err.Error(), "queue_timeout") {
		t.Fatalf("Load failed but the error does not name queue_timeout: %v", err)
	}
}

func TestH9ValidDurationsStillPass(t *testing.T) {
	c := baseConfig()
	c.Server.QueueTimeout = "30s"
	c.Server.ReadTimeout = "30s"
	c.Server.WriteTimeout = "0s"
	c.Server.IdleTimeout = "60s"
	c.Server.StickyTTL = "15m"
	c.Cache.TTL = "5m"
	c.Retention.RollupCron = "1h"
	c.Retention.VacuumCron = "6h"
	c.Provider[0].HealthInterval = "30s"
	c.Provider[0].Timeout = "120s"
	c.Provider[0].Key[0].Window = "1m"

	if err := c.Validate(); err != nil {
		t.Fatalf("a fully valid config was rejected: %v", err)
	}
	if _, err := c.BuildParams(); err != nil {
		t.Fatalf("BuildParams rejected a config Validate accepted: %v", err)
	}
}

func TestH9EmptyDurationsAreSkipped(t *testing.T) {
	c := baseConfig()

	if err := c.Validate(); err != nil {
		t.Fatalf("a Config with empty duration fields was rejected; empty means unset, not invalid: %v", err)
	}
}
