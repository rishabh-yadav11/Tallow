package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestShippedExampleConfigLoads(t *testing.T) {
	path := exampleConfigPath(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("examples/config.toml does not load, but the README tells "+
			"users to copy it as their starting config: %v", err)
	}

	if cfg.Server.Listen == "" {
		t.Error("the example config leaves server.listen empty")
	}
	if len(cfg.Provider) == 0 {
		t.Error("the example config declares no provider")
	}
	if len(cfg.Alias) == 0 {
		t.Error("the example config declares no alias")
	}
	if cfg.Secret.Keystore == "" {
		t.Error("the example config leaves secret.keystore empty, so provider " +
			"keys have no documented side-storage location")
	}
	if cfg.Store.Path == "" {
		t.Error("the example config leaves store.path empty")
	}

	for _, p := range cfg.Provider {
		for _, k := range p.Key {
			if k.Ref == "" {
				t.Errorf("provider %q key %q has no ref: the example documents "+
					"that secrets live in the keystore, not the config", p.Name, k.ID)
			}
		}
	}
}

func TestShippedExampleConfigParsesEveryKeyItDeclares(t *testing.T) {
	path := exampleConfigPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	var loose map[string]any
	if err := toml.Unmarshal(raw, &loose); err != nil {
		t.Fatalf("strict decode of the example config failed: %v", err)
	}

	known := map[string]map[string]bool{
		"": {
			"version": true,
		},
		"server": {
			"listen": true, "admin_socket": true, "max_concurrent": true,
			"max_request_bytes": true, "queue_timeout": true, "sticky_ttl": true,
			"stream_keepalive": true, "tls_cert": true, "tls_key": true,
		},
		"routing": {
			"strategy": true, "request_retry": true,
			"max_retry_credentials": true, "max_retry_interval": true,
			"cooldown_file": true, "session_affinity": true,
			"session_affinity_ttl": true, "session_affinity_subagents": true,
			"session_affinity_rebind_on": true,
		},
		"auth": {
			"open": true, "api_keys": true,
		},
		"cache": {
			"enabled": true, "ttl": true, "max_entries": true,
			"min_prompt_tokens": true,
		},
		"compaction": {
			"enabled": true, "max_tool_output_chars": true,
		},
		"observability": {
			"enabled": true,
		},
		"secret": {
			"keyfile": true, "keystore": true,
		},
		"store": {
			"path": true, "raw_bodies": true,
		},
		"retention": {
			"raw_bodies_days": true, "metadata_days": true, "errors_days": true,
			"rollup_interval": true, "vacuum_interval": true,
		},
		"provider": {
			"name": true, "base_url": true, "rpm": true,
			"health_check": true, "health_interval": true, "timeout": true,
			"fallback": true, "proxy_url": true, "disabled": true,
			"headers": true, "excluded_models": true, "catalog": true,
			"request_retry": true, "disable_cooling": true,
		},
		"provider.key": {
			"id": true, "ref": true, "rpm": true, "max_requests": true,
			"window": true, "max_concurrent": true, "cost_limit_cents": true,
			"weight": true, "priority": true, "headers": true,
			"proxy_url": true, "request_retry": true, "disable_cooling": true,
			"error_rules": true,
		},
		"alias": {
			"name": true,
		},
		"alias.target": {
			"provider": true, "model": true, "context_window": true,
			"supports_tools": true, "supports_vision": true,
			"supports_stream":    true,
			"price_input_per_1m": true, "price_output_per_1m": true,
		},
	}

	var unknown []string
	walkKeys("", loose, known, &unknown)
	if len(unknown) > 0 {
		missing := dedupe(unknown)
		sort.Strings(missing)
		t.Errorf("the example config declares key(s) the config struct does not "+
			"read: %s. TOML ignores unknown keys rather than rejecting them, so "+
			"each of these settings would have no effect while appearing to be "+
			"configured", strings.Join(missing, ", "))
	}
}

func walkKeys(prefix string, v any, known map[string]map[string]bool, out *[]string) {
	m, ok := v.(map[string]any)
	if !ok {

		if arr, isArr := v.([]map[string]any); isArr {
			for _, entry := range arr {
				walkKeys(prefix, entry, known, out)
			}
		}
		return
	}
	for k, child := range m {
		if _, isMap := child.(map[string]any); isMap {

			if known[prefix][k] {
				continue
			}

			walkKeys(joinKey(prefix, k), child, known, out)
			continue
		}
		if _, isArr := child.([]map[string]any); isArr {

			walkKeys(joinKey(prefix, k), child, known, out)
			continue
		}
		if !known[prefix][k] {
			*out = append(*out, joinKey(prefix, k))
		}
	}
}

func joinKey(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func exampleConfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "examples", "config.toml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cannot find the shipped example config at %s: %v", path, err)
	}
	return path
}
