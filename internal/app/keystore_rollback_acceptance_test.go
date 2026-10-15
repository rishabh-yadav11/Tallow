package app_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/app"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

const rollbackCfg = `version = 1
[auth]
open = true

[server]
listen = "127.0.0.1:0"
admin_socket = "%s"
max_concurrent = 8
queue_timeout = "5s"
sticky_ttl = "5s"

[secret]
keystore = "%s"

[store]
path = "%s"
raw_bodies = true

[retention]
raw_bodies_days = 5
metadata_days = 30
errors_days = 90

[observability]
enabled = true

[[provider]]
name = "p1"
base_url = "http://127.0.0.1:1"

  [[provider.key]]
  id = "k1"
  ref = "p1:k1"
  rpm = 100

[[alias]]
name = "flash"
  [[alias.target]]
  provider = "p1"
  model = "m"
`

func newGatewayWithKeystore(t *testing.T) (a *app.App, cfgPath, keystorePath string, dir string) {
	t.Helper()
	dir = t.TempDir()
	keystorePath = filepath.Join(dir, "keys.json")
	cfgPath = filepath.Join(dir, "config.toml")
	cfg := fmt.Sprintf(rollbackCfg,
		filepath.Join(dir, "admin.sock"),
		keystorePath,
		filepath.Join(dir, "tallow.db"),
	)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, "integration-master-key")

	a, err := app.New(cfgPath, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return a, cfgPath, keystorePath, dir
}

func TestAFailedKeyStoreSetIsNotResurrectedByTheNextSave(t *testing.T) {
	_, _, keystorePath, dir := newGatewayWithKeystore(t)

	box, err := secret.NewBox([]byte("integration-master-key"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Set("p1:k1", "sk-live-credential"); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(probe)
		t.Skip("filesystem allows writes despite a read-only directory mode")
	}

	const ghostRef, ghostSecret = "p1:ghost", "sk-ghost-should-never-persist"
	if err := st.Set(ghostRef, ghostSecret); err == nil {
		t.Fatal("Set succeeded against a read-only directory: the rollback path " +
			"is not being exercised")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("p1:k2", "sk-second-credential"); err != nil {
		t.Fatalf("the later Set should succeed: %v", err)
	}

	readBack, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatalf("reload keystore: %v", err)
	}
	if _, err := readBack.Get(ghostRef); err == nil {
		t.Errorf("%s was persisted even though its Set reported failure: the "+
			"failed write was resurrected by the next save. refs=%v",
			ghostRef, readBack.List())
	}

	for _, ref := range []string{"p1:k1", "p1:k2"} {
		if _, err := readBack.Get(ref); err != nil {
			t.Errorf("%s, which was stored successfully, is missing after the "+
				"failed Set: %v", ref, err)
		}
	}

	raw, err := os.ReadFile(keystorePath)
	if err != nil {
		t.Fatal(err)
	}
	if containsAny(string(raw), ghostRef, ghostSecret) {
		t.Errorf("the keystore file contains the ghost ref or its secret: %s", raw)
	}
}

func TestGatewayReloadDoesNotDisturbTheKeystore(t *testing.T) {
	a, _, keystorePath, _ := newGatewayWithKeystore(t)

	box, err := secret.NewBox([]byte("integration-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("p1:k1", "sk-live-credential"); err != nil {
		t.Fatal(err)
	}

	if err := a.Reload(); err != nil {
		t.Fatalf("App.Reload: %v", err)
	}

	readBack, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readBack.Get("p1:k1"); err != nil || got != "sk-live-credential" {
		t.Errorf("the credential did not survive a gateway reload: %q, %v", got, err)
	}
	if len(readBack.List()) != 1 {
		t.Errorf("reload changed the keystore contents: %v", readBack.List())
	}
}

func TestGatewayKeystoreIsEncryptedOnDisk(t *testing.T) {
	_, _, keystorePath, _ := newGatewayWithKeystore(t)

	box, err := secret.NewBox([]byte("integration-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatal(err)
	}
	const secretVal = "sk-plaintext-must-not-appear-1234567890"
	if err := st.Set("p1:k1", secretVal); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(keystorePath)
	if err != nil {
		t.Fatal(err)
	}

	if !containsAny(string(raw), "p1:k1") {
		t.Fatalf("keystore does not contain the ref; the leak check is vacuous: %s", raw)
	}
	if containsAny(string(raw), secretVal) {
		t.Errorf("the keystore holds the secret in plaintext: %s", raw)
	}

	wrong, err := secret.NewBox([]byte("a-different-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	wrongStore, err := secret.LoadStore(keystorePath, wrong)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := wrongStore.Get("p1:k1"); err == nil {
		t.Errorf("a keystore opened with the wrong master key returned %q: it "+
			"must fail closed", v)
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func TestAnUnreadableKeystoreFailsTheReloadClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permission bits are not enforced, so a " +
			"mode 0000 file is still readable and this test would pass vacuously")
	}
	a, _, keystorePath, _ := newGatewayWithKeystore(t)

	box, err := secret.NewBox([]byte("integration-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("p1:k1", "sk-live-credential"); err != nil {
		t.Fatal(err)
	}

	if err := a.Reload(); err != nil {
		t.Fatalf("the baseline reload failed: %v", err)
	}

	if err := os.Chmod(keystorePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(keystorePath, 0o600) })

	if _, err := os.ReadFile(keystorePath); err == nil {
		t.Skip("the filesystem still permits reading a mode 0000 file")
	}

	reloadErr := a.Reload()
	if reloadErr == nil {
		t.Errorf("App.Reload returned nil against an unreadable keystore: the " +
			"gateway reported a successful reload while holding no credentials")
	}

	if err := os.Chmod(keystorePath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("the recovery reload failed once the keystore was readable "+
			"again: %v", err)
	}
}
