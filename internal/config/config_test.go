package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalValid returns a Config that passes Validate with one provider, one
// key, and one alias targeting it.
func minimalValid() *Config {
	return &Config{
		Version: SchemaVersion,
		Provider: []Provider{
			{
				Name:    "p1",
				BaseURL: "https://p1.example/v1",
				Key:     []Key{{ID: "k1", RPM: 10, MaxRequests: 100}},
			},
		},
		Alias: []Alias{
			{Name: "m", Target: []Target{{Provider: "p1", Model: "model-a"}}},
		},
	}
}

func TestValidateAcceptsMinimalValidConfig(t *testing.T) {
	c := minimalValid()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate on minimal valid config: %v", err)
	}
}

func TestValidateRejectsWrongSchemaVersion(t *testing.T) {
	for _, v := range []int{0, SchemaVersion + 1, 99} {
		c := minimalValid()
		c.Version = v
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "unsupported config version") {
			t.Fatalf("version %d: expected unsupported-version error, got %v", v, err)
		}
	}
}

func TestValidateProviderInvariants(t *testing.T) {
	t.Run("empty provider name", func(t *testing.T) {
		c := minimalValid()
		c.Provider[0].Name = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for empty provider name")
		}
	})
	t.Run("duplicate provider", func(t *testing.T) {
		c := minimalValid()
		c.Provider = append(c.Provider, c.Provider[0])
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicate provider") {
			t.Fatalf("expected duplicate-provider error, got %v", err)
		}
	})
	t.Run("missing base_url", func(t *testing.T) {
		c := minimalValid()
		c.Provider[0].BaseURL = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for provider missing base_url")
		}
	})
	t.Run("duplicate key id", func(t *testing.T) {
		c := minimalValid()
		c.Provider[0].Key = append(c.Provider[0].Key, Key{ID: "k1"})
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for duplicate key id")
		}
	})
	t.Run("negative limits", func(t *testing.T) {
		c := minimalValid()
		c.Provider[0].Key[0].RPM = -1
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for negative RPM")
		}
		c = minimalValid()
		c.Provider[0].Key[0].MaxRequests = -5
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for negative MaxRequests")
		}
	})
}

func TestValidateAliasInvariants(t *testing.T) {
	t.Run("unknown provider target", func(t *testing.T) {
		c := minimalValid()
		c.Alias[0].Target[0] = Target{Provider: "nope", Model: "m"}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "unknown provider") {
			t.Fatalf("expected unknown-provider error, got %v", err)
		}
	})
	t.Run("missing model", func(t *testing.T) {
		c := minimalValid()
		c.Alias[0].Target[0] = Target{Provider: "p1"}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for target missing model")
		}
	})
	t.Run("empty target list", func(t *testing.T) {
		c := minimalValid()
		c.Alias[0].Target = nil
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for alias with no targets")
		}
	})
	t.Run("duplicate alias name", func(t *testing.T) {
		c := minimalValid()
		c.Alias = append(c.Alias, c.Alias[0])
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for duplicate alias name")
		}
	})
}

// TestLoadValidatesExampleConfigShape exercises Load end-to-end with an
// inline TOML doc mirroring examples/config.toml, so the parse -> defaults ->
// validate pipeline used at every startup is actually tested.
func TestLoadValidatesExampleConfigShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	doc := `
version = 1

[server]
listen = "127.0.0.1:8080"
admin_socket = "~/test-admin.sock"

[[provider]]
name = "p1"
base_url = "https://p1.example/v1"
[[provider.key]]
id = "k1"
ref = "p1:k1"

[[alias]]
name = "fast"
[[alias.target]]
provider = "p1"
model = "m-1"
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("default listen not applied: %q", c.Server.Listen)
	}
	if c.Retention.MetadataDays != 30 {
		t.Fatalf("default metadata retention not applied: %d", c.Retention.MetadataDays)
	}
	if c.Cache.MaxEntries != 1024 {
		t.Fatalf("default cache entries not applied: %d", c.Cache.MaxEntries)
	}
	if c.Server.AdminSocket == "" {
		t.Fatal("admin socket default not applied")
	}
	if !strings.Contains(c.Server.AdminSocket, "test-admin.sock") {
		t.Fatalf("tilde expansion not applied: %q", c.Server.AdminSocket)
	}
}

func TestLoadRejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("version = 999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unsupported config version") {
		t.Fatalf("expected version-guard error, got %v", err)
	}
}
