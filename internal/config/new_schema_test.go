package config

import (
	"strings"
	"testing"
)

func newSchemaBase() *Config {
	c := minimalValid()
	return c
}

func TestValidateStrategy(t *testing.T) {
	t.Run("accepts each valid strategy", func(t *testing.T) {
		for _, s := range []string{"round-robin", "weighted-round-robin", "fill-first"} {
			c := newSchemaBase()
			c.Routing.Strategy = s
			if err := c.Validate(); err != nil {
				t.Fatalf("strategy %q rejected: %v", s, err)
			}
		}
	})
	t.Run("accepts empty strategy (default applied on Load path)", func(t *testing.T) {
		c := newSchemaBase()
		c.Routing.Strategy = ""
		if err := c.Validate(); err != nil {
			t.Fatalf("empty strategy rejected; applyDefaults fills round-robin on Load: %v", err)
		}
	})
	t.Run("rejects unknown strategy", func(t *testing.T) {
		c := newSchemaBase()
		c.Routing.Strategy = "priority-fair"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "routing.strategy") {
			t.Fatalf("expected routing.strategy error, got %v", err)
		}
	})
	t.Run("rejects invalid rebind_on", func(t *testing.T) {
		c := newSchemaBase()
		c.Routing.SessionAffinityRebindOn = "sometimes"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "session_affinity_rebind_on") {
			t.Fatalf("expected session_affinity_rebind_on error, got %v", err)
		}
	})
}

func TestValidateErrorRule(t *testing.T) {
	t.Run("accepts valid error rule", func(t *testing.T) {
		c := newSchemaBase()
		c.Provider[0].Key[0].ErrorRules = []ErrorRule{
			{Status: 429, Match: []string{"rate limited"}, Action: "stop-and-cooldown"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("valid error rule rejected: %v", err)
		}
	})
	t.Run("rejects invalid action", func(t *testing.T) {
		c := newSchemaBase()
		c.Provider[0].Key[0].ErrorRules = []ErrorRule{{Status: 0, Action: "panic"}}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "action") {
			t.Fatalf("expected action error, got %v", err)
		}
	})
	t.Run("rejects out-of-range status", func(t *testing.T) {
		c := newSchemaBase()
		c.Provider[0].Key[0].ErrorRules = []ErrorRule{{Status: 300, Action: "stop"}}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "status 300 out of range") {
			t.Fatalf("expected status-range error, got %v", err)
		}
	})
	t.Run("rejects uncompilable regexp", func(t *testing.T) {
		c := newSchemaBase()
		c.Provider[0].Key[0].ErrorRules = []ErrorRule{{Action: "continue", MatchRegexp: []string{"(["}}}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "match_regexp") {
			t.Fatalf("expected match_regexp compile error, got %v", err)
		}
	})
}

func TestValidateProxyURL(t *testing.T) {
	t.Run("rejects ftp scheme", func(t *testing.T) {
		c := newSchemaBase()
		c.Provider[0].ProxyURL = "ftp://files.example/upload"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
			t.Fatalf("expected unsupported-scheme error, got %v", err)
		}
	})
	for _, u := range []string{"socks5://127.0.0.1:1080", "socks5h://proxy.example:1080", "socks://10.0.0.1:1080", "http://proxy.example:3128", "https://proxy.example:443", "direct", ""} {
		c := newSchemaBase()
		c.Provider[0].ProxyURL = u
		if err := c.Validate(); err != nil {
			t.Fatalf("proxy_url %q rejected: %v", u, err)
		}
	}
	t.Run("rejects ftp on a key", func(t *testing.T) {
		c := newSchemaBase()
		c.Provider[0].Key[0].ProxyURL = "ftp://files.example/upload"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "proxy_url") {
			t.Fatalf("expected key proxy_url error, got %v", err)
		}
	})
	t.Run("accepts direct on a key", func(t *testing.T) {
		c := newSchemaBase()
		c.Provider[0].Key[0].ProxyURL = "direct"
		if err := c.Validate(); err != nil {
			t.Fatalf("key proxy_url = direct rejected: %v", err)
		}
	})
}

func TestValidateTLSPair(t *testing.T) {
	t.Run("rejects cert without key", func(t *testing.T) {
		c := newSchemaBase()
		c.Server.TLSCert = "server.crt"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "tls_cert") {
			t.Fatalf("expected both-or-neither error, got %v", err)
		}
	})
	t.Run("rejects key without cert", func(t *testing.T) {
		c := newSchemaBase()
		c.Server.TLSKey = "server.key"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "tls_key") {
			t.Fatalf("expected both-or-neither error, got %v", err)
		}
	})
	t.Run("accepts both", func(t *testing.T) {
		c := newSchemaBase()
		c.Server.TLSCert = "server.crt"
		c.Server.TLSKey = "server.key"
		if err := c.Validate(); err != nil {
			t.Fatalf("tls cert+key pair rejected: %v", err)
		}
	})
}

func TestValidateNegativeWeight(t *testing.T) {
	c := newSchemaBase()
	c.Provider[0].Key[0].Weight = -1
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "weight") {
		t.Fatalf("expected weight error, got %v", err)
	}
	c = newSchemaBase()
	c.Provider[0].Key[0].Priority = -1
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "priority") {
		t.Fatalf("expected priority error, got %v", err)
	}

	c = newSchemaBase()
	c.Provider[0].Key[0].Weight = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("weight 0 rejected: %v", err)
	}
}

func TestMaxContextLengthFolding(t *testing.T) {
	load := func(body string) *Config {
		t.Helper()
		path := writeConfig(t, "version = 1\n"+body)
		c, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return c
	}
	base := `
[[provider]]
name = "p1"
base_url = "https://p1.example/v1"
[[provider.key]]
id = "k1"

[[alias]]
name = "m"
[[alias.target]]
provider = "p1"
model = "model-a"
`

	t.Run("folds when context_window absent", func(t *testing.T) {
		c := load(base + "\nmax_context_length = 8192\n")
		if got := c.Alias[0].Target[0].ContextWindow; got != 8192 {
			t.Fatalf("max_context_length not folded into context_window: got %d", got)
		}
	})
	t.Run("context_window wins when both set", func(t *testing.T) {
		c := load(base + "\ncontext_window = 16384\nmax_context_length = 8192\n")
		if got := c.Alias[0].Target[0].ContextWindow; got != 16384 {
			t.Fatalf("context_window should win over max_context_length: got %d", got)
		}
	})
}

func TestBuildCopiesNewFields(t *testing.T) {
	c := newSchemaBase()
	p := &c.Provider[0]
	p.ProxyURL = "socks5://127.0.0.1:1080"
	p.Disabled = true
	p.Headers = map[string]string{"X-Default": "a"}
	p.ExcludedModels = []string{"*-preview"}
	p.Catalog = true
	rr := 2
	p.RequestRetry = &rr
	p.DisableCooling = true
	k := &p.Key[0]
	k.Weight = 7
	k.Priority = 3
	k.Headers = map[string]string{"X-Key": "b"}
	k.ProxyURL = "direct"
	krr := 1
	k.RequestRetry = &krr
	dcFalse := false
	k.DisableCooling = &dcFalse
	k.ErrorRules = []ErrorRule{{Status: 429, Action: "stop-and-cooldown"}}
	c.Alias[0].DisplayName = "Pretty Name"
	c.Alias[0].ExcludedModels = []string{"*flash*"}
	c.Alias[0].Target[0].ForceMapping = true
	c.Alias[0].Target[0].DisplayName = "Pretty Target"

	provs, aliases, err := c.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bp := provs[0]
	if bp.ProxyURL != p.ProxyURL || !bp.Disabled || !bp.Catalog || !bp.DisableCooling {
		t.Fatalf("provider fields not copied: %+v", bp)
	}
	if bp.RequestRetry == nil || *bp.RequestRetry != 2 {
		t.Fatalf("provider request_retry not copied: %v", bp.RequestRetry)
	}
	bk := bp.Keys[0]
	if bk.Weight != 7 || bk.Priority != 3 || bk.ProxyURL != "direct" {
		t.Fatalf("key fields not copied: %+v", bk)
	}
	if bk.RequestRetry == nil || *bk.RequestRetry != 1 {
		t.Fatalf("key request_retry not copied: %v", bk.RequestRetry)
	}
	if bk.DisableCooling {
		t.Fatal("key DisableCooling explicit false should win over provider true")
	}
	if len(bk.ErrorRules) != 1 || bk.ErrorRules[0].Status != 429 || bk.ErrorRules[0].Action != "stop-and-cooldown" {
		t.Fatalf("error rules not copied: %+v", bk.ErrorRules)
	}
	ba := aliases[0]
	if ba.DisplayName != "Pretty Name" || len(ba.ExcludedModels) != 1 || !ba.Targets[0].ForceMapping || ba.Targets[0].DisplayName != "Pretty Target" {
		t.Fatalf("alias/target fields not copied: %+v %+v", ba, ba.Targets[0])
	}
}
