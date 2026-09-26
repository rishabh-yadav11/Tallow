package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The README's first instruction is `cp examples/config.toml ./config.toml`.
// That file is therefore the first thing a new user runs, and until now nothing
// loaded it: config_test.go only inlined a doc "mirroring
// examples/config.toml", so the two could drift apart indefinitely and the suite
// would stay green.
//
// The drift is silent in the worst direction. A renamed or misspelled key in the
// example is not a parse error, because TOML ignores unknown keys, so the file
// keeps loading while quietly not configuring what it says it configures. This
// is not hypothetical: while this file was being written, a test config using
// `price_input_per_m` instead of `price_input_per_1m` priced every request at
// zero with no error anywhere, and the budgets assertions in
// internal/app/admin_socket_acceptance_test.go were passing for the wrong reason
// until the key was spelled correctly.

// TestShippedExampleConfigLoads loads the file the README tells users to copy,
// through the same Load path the binary uses.
func TestShippedExampleConfigLoads(t *testing.T) {
	path := exampleConfigPath(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("examples/config.toml does not load, but the README tells "+
			"users to copy it as their starting config: %v", err)
	}
	// Load validates, so reaching here already proves the file is well formed.
	// These assert the fields a new user depends on being set by the file
	// itself rather than silently defaulted.
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
	// Every provider key references the keystore rather than carrying a secret,
	// which is the property the README and SECURITY.md both promise.
	for _, p := range cfg.Provider {
		for _, k := range p.Key {
			if k.Ref == "" {
				t.Errorf("provider %q key %q has no ref: the example documents "+
					"that secrets live in the keystore, not the config", p.Name, k.ID)
			}
		}
	}
}

// TestShippedExampleConfigParsesEveryKeyItDeclares is the guard against the
// silent-drift case, and it needs a second decode to work at all.
//
// A single Load into Config cannot detect a misspelled key. The parser drops
// the unknown key and still populates the correctly spelled one, so the target
// simply reads as half-priced with no error. Three earlier versions of this
// check all passed against a misspelled `price_input_per_1m`, each for a
// different reason worth recording, because each looked like a reasonable way
// to compare the file against the parse:
//
//   - Counting `strings.Count(text, "price_input_per_1m")` cannot see a typo.
//     The misspelled line no longer contains the correctly spelled key, so the
//     count drops to match the parse and the two agree.
//   - Counting a target as "priced" if EITHER price parsed non-zero passed
//     because the output price was still recognised.
//   - Filtering lines by `HasPrefix(s, "price_input_per_1m")` is the first idea
//     again in disguise: the misspelled line does not match the prefix, so it
//     is never examined at all.
//
// The fix is to decode the same file into a shape that PRESERVES unknown keys.
// Decoding into map[string]any keeps the key exactly as written, so a typo
// becomes visible as a key that the typed load did not consume. Comparing the
// two is what actually answers "does every key in the example correspond to a
// field the struct declares?".
func TestShippedExampleConfigParsesEveryKeyItDeclares(t *testing.T) {
	path := exampleConfigPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A generic decode, so every key is retained regardless of whether the
	// struct knows it.
	var loose map[string]any
	if err := toml.Unmarshal(raw, &loose); err != nil {
		t.Fatalf("strict decode of the example config failed: %v", err)
	}

	// The keys the config struct actually reads, scoped to the table they belong
	// to. Scoping matters: a flat list has to carry sticky_ttl,
	// max_tool_output_chars and the seven retention keys alongside everything
	// else, and such a list is exactly the kind of thing that quietly drifts
	// from the struct. Grouped by table, a key that moves tables becomes a
	// failure rather than a silent mismatch.
	//
	// The list is explicit rather than reflection-derived on purpose. Deriving
	// it would mean trusting that each toml tag is spelled as intended, which
	// is the very thing under test, and a wrong entry here fails loudly by
	// flagging real keys rather than by passing.
	known := map[string]map[string]bool{
		"": { // top level
			"version": true,
		},
		"server": {
			"listen": true, "admin_socket": true, "max_concurrent": true,
			"max_request_bytes": true, "queue_timeout": true, "sticky_ttl": true,
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
			"fallback": true,
		},
		"provider.key": {
			"id": true, "ref": true, "rpm": true, "max_requests": true,
			"window": true, "max_concurrent": true, "cost_limit_cents": true,
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

// walkKeys collects every key path in a decoded document whose key is not
// declared in the struct for that table. Decoding a TOML document into
// map[string]any yields maps all the way down, so a leaf value is reached
// through the map branch and its key checked there.
func walkKeys(prefix string, v any, known map[string]map[string]bool, out *[]string) {
	m, ok := v.(map[string]any)
	if !ok {
		// An array of tables (a [[provider]] or [[alias]] list) decodes to a
		// slice of maps; walk each entry under the same table name.
		if arr, isArr := v.([]map[string]any); isArr {
			for _, entry := range arr {
				walkKeys(prefix, entry, known, out)
			}
		}
		return
	}
	for k, child := range m {
		if _, isMap := child.(map[string]any); isMap {
			// A nested table such as [store]: its header is not a setting, and
			// its keys are checked when recursing.
			walkKeys(joinKey(prefix, k), child, known, out)
			continue
		}
		if _, isArr := child.([]map[string]any); isArr {
			// An array of tables: the header is not a setting either.
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
