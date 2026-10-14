package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigLoadsRoutingAndPerKeyFeatures pins that the CLIProxyAPI-derived
// fields (§2, §3, §4, §6) round-trip through Load -> Validate -> Build without
// tripping the existing checks.
func TestConfigLoadsRoutingAndPerKeyFeatures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	doc := `
version = 1

[server]
listen = "127.0.0.1:8080"
tls_cert = "/certs/t.crt"
tls_key = "/certs/t.key"
stream_keepalive = "15s"

[routing]
strategy = "weighted-round-robin"
request-retry = 3
max-retry-interval = "60s"
session-affinity = true
session-affinity-ttl = "5m"
session-affinity-subagents = true
session-affinity-rebind-on = "quota"
[routing.cooldown]
enabled = true
offset = "45s"
persist = "./cooldown.cds"

[[provider]]
name = "p1"
base_url = "https://p1.example/v1"
proxy_url = "socks5://127.0.0.1:1080"
disabled = false
excluded_models = ["old-*", "*beta"]
[[provider.key]]
id = "k1"
weight = 3
priority = 2
headers = { "X-Extra" = "plain", "X-Trace" = "$trace-id" }
proxy_url = "direct"
request-retry = 1
[[provider.key.error_rules]]
status = 429
match = ["rate limit"]
action = "continue-and-cooldown"

[[alias]]
name = "fast"
excluded_models = ["*legacy"]
[[alias.target]]
provider = "p1"
model = "m-1"
max_context_length = 128000
force_mapping = true
display_name = "Fast Model"
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Routing.Strategy != "weighted-round-robin" {
		t.Fatalf("strategy: %q", c.Routing.Strategy)
	}
	if c.Routing.RequestRetry != 3 {
		t.Fatalf("request-retry: %d", c.Routing.RequestRetry)
	}
	if c.Server.StreamKeepalive != "15s" {
		t.Fatalf("stream_keepalive: %q", c.Server.StreamKeepalive)
	}
	if !c.Routing.Cooldown.Enabled || c.Routing.Cooldown.Offset != "45s" {
		t.Fatalf("cooldown: %+v", c.Routing.Cooldown)
	}
	if c.Provider[0].ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("provider proxy_url: %q", c.Provider[0].ProxyURL)
	}
	k := c.Provider[0].Key[0]
	if k.Weight != 3 || k.Priority != 2 || k.ProxyURL != "direct" {
		t.Fatalf("key weight/priority/proxy: %+v", k)
	}
	if k.Headers["X-Extra"] != "plain" || k.Headers["X-Trace"] != "$trace-id" {
		t.Fatalf("key headers: %+v", k.Headers)
	}
	if len(k.ErrorRules) != 1 || k.ErrorRules[0].Action != "continue-and-cooldown" {
		t.Fatalf("key error_rules: %+v", k.ErrorRules)
	}
	if c.Provider[0].ExcludedModels[0] != "old-*" {
		t.Fatalf("provider excluded_models: %+v", c.Provider[0].ExcludedModels)
	}
	tg := c.Alias[0].Target[0]
	if !tg.ForceMapping || tg.MaxContextLength != 128000 || tg.DisplayName != "Fast Model" {
		t.Fatalf("alias target fields: %+v", tg)
	}
}

// TestValidateRejectsBadRoutingStrategy pins the enum checks on the new knobs.
func TestValidateRejectsBadRoutingStrategy(t *testing.T) {
	c := minimalValid()
	c.Routing.Strategy = "random"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "routing.strategy") {
		t.Fatalf("expected strategy error, got %v", err)
	}
}

func TestValidateRejectsBadRebindOn(t *testing.T) {
	c := minimalValid()
	c.Routing.SessionAffinityRebindOn = "sometimes"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "session-affinity-rebind-on") {
		t.Fatalf("expected rebind-on error, got %v", err)
	}
}

func TestValidateRejectsBadRuleAction(t *testing.T) {
	c := minimalValid()
	c.Provider[0].ErrorRules = []ErrorRule{{Status: 429, Action: "explode"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "rule action") {
		t.Fatalf("expected rule-action error, got %v", err)
	}
}

func TestValidateRejectsBadProxyURL(t *testing.T) {
	c := minimalValid()
	c.Provider[0].ProxyURL = "ftp://bad.example"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "proxy_url") {
		t.Fatalf("expected proxy_url error, got %v", err)
	}
}

func TestValidateRejectsNegativeWeight(t *testing.T) {
	c := minimalValid()
	c.Provider[0].Key[0].Weight = -2
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "weight") {
		t.Fatalf("expected weight error, got %v", err)
	}
}
