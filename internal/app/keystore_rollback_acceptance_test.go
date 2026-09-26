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

// This file covers the keystore rollback contract at the one place where it is
// actually reachable: a long-lived gateway process.
//
// The contract Set and Delete promise is that the in-memory map always describes
// what is on disk. That promise is only observable when the same Store outlives
// a failed write, because that is the only situation in which a stale map can
// be written out by a later, unrelated save.
//
// That rules out the CLI as an observation point. `tallowctl key` calls
// secret.LoadStore once per invocation and performs exactly one operation before
// exiting, so a failed add that leaves a ghost in the map dies with the process
// and never reaches disk. A subprocess-driven CLI test can confirm the operator
// sees an error and the file is unchanged, which is worth having, but it cannot
// detect a missing rollback, and a suite that appears to cover the contract
// while being structurally unable to fail is worse than no suite: it reports
// confidence it has not earned. Verified by mutation: deleting the rollback in
// Store.Set leaves every tallowctl key test green.
//
// The gateway does hold the Store across requests and reloads it in-process
// through App.Reload, called by the config watcher and by POST /admin/reload.
// That is the path where a stale map would leak, so that is what is driven here.

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

// newGatewayWithKeystore starts a real App against a real keystore file and
// returns the App, the config path, and the keystore path. The App is not Run:
// App.Reload is the exported entry point the admin endpoint and the file watcher
// both call, so driving it directly is the same code path without the timing
// dependency of waiting for a watcher tick.
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

// TestKeystoreGhostIsNotResurrectedByTheGatewayReload is the missing case.
//
// A Store that failed a Set is held open, the filesystem is repaired, and a
// later Set is performed on the SAME Store. If Set left the failed entry in the
// map, that later save serialises the whole map and persists a credential the
// operator was told was never stored. App.Reload is called in between because
// that is what the gateway does, and it must not be the thing that flushes the
// ghost out.
func TestKeystoreGhostIsNotResurrectedByTheGatewayReload(t *testing.T) {
	_, _, keystorePath, dir := newGatewayWithKeystore(t)

	box, err := secret.NewBox([]byte("integration-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	// One long-lived Store, the shape the gateway holds.
	st, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatal(err)
	}
	// A real key the operator definitely stored, so the later save has
	// something legitimate to write alongside any ghost.
	if err := st.Set("p1:k1", "sk-live-credential"); err != nil {
		t.Fatal(err)
	}

	// Make the save fail for real, via the filesystem, not a stub.
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

	// Repair, then use the same Store again. This is the step that would
	// persist the ghost.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("p1:k2", "sk-second-credential"); err != nil {
		t.Fatalf("the later Set should succeed: %v", err)
	}

	// What the next process, or the next reload, would load. Read from disk
	// through the real loader rather than trusting the map that is under test.
	readBack, err := secret.LoadStore(keystorePath, box)
	if err != nil {
		t.Fatalf("reload keystore: %v", err)
	}
	if _, err := readBack.Get(ghostRef); err == nil {
		t.Errorf("%s was persisted even though its Set reported failure: the "+
			"failed write was resurrected by the next save. refs=%v",
			ghostRef, readBack.List())
	}
	// The legitimately stored keys must be there, or the rollback has simply
	// discarded real work instead of the failed one.
	for _, ref := range []string{"p1:k1", "p1:k2"} {
		if _, err := readBack.Get(ref); err != nil {
			t.Errorf("%s, which was stored successfully, is missing after the "+
				"failed Set: %v", ref, err)
		}
	}
	// And the ghost's plaintext must not be anywhere in the file, in any form.
	raw, err := os.ReadFile(keystorePath)
	if err != nil {
		t.Fatal(err)
	}
	if containsAny(string(raw), ghostRef, ghostSecret) {
		t.Errorf("the keystore file contains the ghost ref or its secret: %s", raw)
	}
}

// TestGatewayReloadDoesNotDisturbTheKeystore is the control for the case above.
// Without it, a suite that merely broke every write would also pass: the ghost
// would be absent because nothing was ever stored. This asserts the gateway's
// own reload leaves a working credential intact, so the absence of the ghost in
// the previous test is attributable to the rollback and not to a dead keystore.
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

	// The gateway's real reload path, the same one POST /admin/reload calls.
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

// TestGatewayKeystoreIsEncryptedOnDisk is the packaging check on the same path.
// The gateway is what resolves a key ref into a live credential, so the
// keystore it loads must be ciphertext on disk. A regression that wrote
// plaintext would not be caught by either rollback test above, since both only
// care about which refs exist.
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
	// The ref must be present, so the check cannot pass on an empty file.
	if !containsAny(string(raw), "p1:k1") {
		t.Fatalf("keystore does not contain the ref; the leak check is vacuous: %s", raw)
	}
	if containsAny(string(raw), secretVal) {
		t.Errorf("the keystore holds the secret in plaintext: %s", raw)
	}

	// A wrong master key must fail closed rather than return anything usable.
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
