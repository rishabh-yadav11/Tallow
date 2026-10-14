package setup

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

func runInDir(t *testing.T, input string) (dir, cfg string, out *bytes.Buffer, err error) {
	dir = t.TempDir()
	t.Setenv("HOME", dir)
	cfg = filepath.Join(dir, "config.toml")
	out = &bytes.Buffer{}
	err = Run(cfg, strings.NewReader(input), out)
	return dir, cfg, out, err
}

func TestSetupHappyPath(t *testing.T) {
	secretInput := "sk-acme-secret-123"
	dir, cfgPath, out, err := runInDir(t, "acme\nhttps://api.acme.com/v1\n"+secretInput+"\nacme-7b\n")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if c.Version != config.SchemaVersion {
		t.Fatalf("version = %d, want %d", c.Version, config.SchemaVersion)
	}
	if len(c.Auth.APIKeys) != 1 || len(c.Auth.APIKeys[0]) != 32 {
		t.Fatalf("client api key = %q, want 16-byte hex", c.Auth.APIKeys[0])
	}
	if len(c.Provider) != 1 || c.Provider[0].Name != "acme" || c.Provider[0].BaseURL != "https://api.acme.com/v1" {
		t.Fatalf("provider wrong: %+v", c.Provider)
	}
	if len(c.Provider[0].Key) != 1 || c.Provider[0].Key[0].Ref != "acme:default" {
		t.Fatalf("provider key ref wrong: %+v", c.Provider[0].Key)
	}
	if len(c.Alias) != 1 || c.Alias[0].Name != "acme-7b" {
		t.Fatalf("alias wrong: %+v", c.Alias)
	}
	if c.Alias[0].Target[0].Model != "acme-7b" {
		t.Fatalf("alias target model wrong: %+v", c.Alias[0].Target[0])
	}

	home := dir
	keyfile := filepath.Join(home, ".tallow", "master.key")
	ksPath := filepath.Join(home, ".tallow", "keys.json")
	if _, err := os.Stat(keyfile); err != nil {
		t.Fatalf("keyfile missing: %v", err)
	}
	st, err := os.Stat(keyfile)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("keyfile mode = %o, want 600", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Dir(keyfile)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("keyfile parent mode = %v, want 700 dir (err=%v)", st, err)
	}

	raw, err := os.ReadFile(ksPath)
	if err != nil {
		t.Fatalf("read keystore: %v", err)
	}
	if strings.Contains(string(raw), secretInput) {
		t.Fatal("keystore file contains raw secret")
	}
	if strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte(secretInput))) {
		t.Fatal("keystore file contains base64 of secret")
	}
	mk, err := secret.LoadMasterKey(keyfile)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secret.NewBox(mk)
	if err != nil {
		t.Fatal(err)
	}
	store, err := secret.LoadStore(ksPath, box)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("acme:default")
	if err != nil || got != secretInput {
		t.Fatalf("Get = %q, %v; want %q", got, err, secretInput)
	}

	summary := out.String()
	for _, want := range []string{"acme", "acme-7b", "provider api key: stored in keystore"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestSetupRepromptAndSkipKey(t *testing.T) {
	input := "\n\nacme\nhttps://api.acme.com/v1\n\nacme-7b\n"
	_, cfgPath, out, err := runInDir(t, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "provider name:") {
		t.Fatal("missing provider prompt in output")
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if c.Provider[0].Name != "acme" {
		t.Fatalf("provider name = %q, want acme", c.Provider[0].Name)
	}
	if len(c.Provider[0].Key) != 0 {
		t.Fatal("no [[provider.key]] block should be emitted when api key was skipped")
	}
	if _, err := os.Stat(filepath.Join(c.Secret.Keystore)); !os.IsNotExist(err) {
		t.Fatal("keystore should not have been created when api key is empty")
	}
	if !strings.Contains(out.String(), "no provider api key stored") {
		t.Fatalf("missing add-later hint:\n%s", out.String())
	}
}

func TestSetupInvalidBaseURLReprompts(t *testing.T) {
	input := "acme\nnot-a-url\nhttps://api.acme.com/v1\nsk-x\nacme-7b\n"
	_, cfgPath, out, err := runInDir(t, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "base_url must include a scheme") {
		t.Fatalf("validation error not surfaced:\n%s", out.String())
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if c.Provider[0].BaseURL != "https://api.acme.com/v1" {
		t.Fatalf("base url = %q", c.Provider[0].BaseURL)
	}
}

func TestSetupPrivateBaseURLNote(t *testing.T) {
	_, _, out, err := runInDir(t, "local\nhttp://127.0.0.1:8080/v1\n\nlocal-7b\n")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "private base_url needs -allow-private-upstream on tallow") {
		t.Fatalf("private note missing:\n%s", out.String())
	}
}

func TestSetupAbortsOnEOF(t *testing.T) {
	for _, input := range []string{"acme\n", "acme\nhttps://api.acme.com/v1\nsk-x\n", "\n"} {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		cfg := filepath.Join(dir, "config.toml")
		var out bytes.Buffer
		err := Run(cfg, strings.NewReader(input), &out)
		if err == nil || !strings.Contains(err.Error(), "setup aborted: no config written") {
			t.Fatalf("input %q: err = %v, want abort", input, err)
		}
		if _, e := os.Stat(cfg); !os.IsNotExist(e) {
			t.Fatalf("input %q: config file written despite abort", input)
		}
		if _, e := os.Stat(filepath.Join(dir, ".tallow", "master.key")); !os.IsNotExist(e) {
			t.Fatalf("input %q: keyfile written despite abort", input)
		}
	}
}

func TestSetupRejectsExistingConfig(t *testing.T) {
	_, cfgPath, _, err := runInDir(t, "acme\nhttps://api.acme.com/v1\nsk-x\nacme-7b\n")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(cfgPath, strings.NewReader("other\nhttps://x.io\n\nother-1\n"), &out); err == nil {
		t.Fatal("Run succeeded on existing config, want error")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already-exists", err)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("existing config was modified")
	}
}

func TestSetupEnvMasterKeySkipsKeyfile(t *testing.T) {
	t.Setenv(secret.EnvMasterKey, "env-master-key")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := filepath.Join(dir, "config.toml")
	var out bytes.Buffer
	if err := Run(cfg, strings.NewReader("acme\nhttps://api.acme.com/v1\nsk-env-secret\nacme-7b\n"), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".tallow", "master.key")); !os.IsNotExist(err) {
		t.Fatal("keyfile created despite TALLOW_MASTER_KEY")
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestSetupUsesExistingKeyfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	keyfile := filepath.Join(dir, ".tallow", "master.key")
	os.MkdirAll(filepath.Dir(keyfile), 0o700)
	os.WriteFile(keyfile, []byte("pre-existing-key\n"), 0o600)

	cfg := filepath.Join(dir, "config.toml")
	var out bytes.Buffer
	if err := Run(cfg, strings.NewReader("acme\nhttps://api.acme.com/v1\nsk-x\nacme-7b\n"), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(keyfile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "pre-existing-key\n" {
		t.Fatalf("keyfile rewritten: %q", raw)
	}
	mk, err := secret.LoadMasterKey(keyfile)
	if err != nil {
		t.Fatal(err)
	}
	if string(mk) != "pre-existing-key" {
		t.Fatalf("reused key = %q, want pre-existing-key", mk)
	}
}
