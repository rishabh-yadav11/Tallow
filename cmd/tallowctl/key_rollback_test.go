package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rollback contract in internal/secret is proven at the Store level in
// internal/secret, including across a reload. What it is not proven through is
// the command an operator actually runs.
//
// Set and Delete roll back on a failed save precisely so that an error printed
// by `tallowctl key add` or `key rm` is trustworthy: the operator is told the
// operation failed, and the keystore really is unchanged. That promise belongs
// to the CLI's output, not only to the library underneath it, and the CLI is
// where a misreading turns into a broken deployment: a key the operator
// believes was rotated still being the old one, or a credential that was
// reported as removed still being live.
//
// So these tests drive keyCmd end to end with a real read-only keystore
// directory, and check the two things an operator can observe: what the command
// printed, and what a fresh process would load from disk afterwards.

// lockedKeystoreDir makes the keystore's directory read-only, which makes the
// atomic save fail with a real EACCES from the OS rather than an injected
// error. It is the same injection the internal/secret suites use, so the CLI
// path fails for the genuine reason rather than a simulated one.
func lockedKeystoreDir(t *testing.T) (keyfile, keystore string) {
	t.Helper()
	keyfile, keystore = e2ePaths(t)
	if err := os.Chmod(filepath.Dir(keystore), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(keystore), 0o700) })
	// Guard against a filesystem that ignores the mode: a test that cannot fail
	// the save is not testing rollback, it is testing nothing.
	probe := filepath.Join(filepath.Dir(keystore), ".probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(probe)
		t.Skip("filesystem allows writes despite a read-only directory mode")
	}
	return keyfile, keystore
}

// unlock repairs the filesystem so a later command can succeed. This is what
// makes the tests able to observe a failed command's effect: without it there
// is no way to distinguish "the rollback worked" from "everything fails".
func unlock(t *testing.T, keystore string) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(keystore), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestFailedKeyAddLeavesNothingOnDiskForTheNextProcess is the add half, at the
// level the operator experiences it. The add reports an error, so the operator
// believes no key was stored. A later `key list` and a fresh LoadStore must
// agree, or the operator is wrong about which credentials the gateway will use.
func TestFailedKeyAddLeavesNothingOnDiskForTheNextProcess(t *testing.T) {
	keyfile, keystore := lockedKeystoreDir(t)
	t.Setenv("TALLOWCTL_TEST_SECRET_GHOST", "sk-ghost-should-not-persist")

	// The add must fail, and keyCmd exits the process on that path, so it is
	// driven in a child process where the exit code is observable.
	out, code := runKeyCmd(t, []string{
		"add", "provider/ghost",
		"--secret-env", "TALLOWCTL_TEST_SECRET_GHOST",
		"--keyfile", keyfile, "--keystore", keystore,
	})
	if code == 0 {
		t.Fatalf("`key add` reported SUCCESS (exit 0) while the keystore "+
			"directory is read-only: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "error") &&
		!strings.Contains(strings.ToLower(out), "denied") {
		t.Errorf("the failing add printed no recognizable error, so an "+
			"operator would not know it failed: %q", out)
	}

	// A later, unrelated key operation must not resurrect the ghost. This is
	// the specific way the old bug was dangerous: the entry sat in the map and
	// the next key add wrote the whole map out, persisting a credential that
	// was reported as never stored.
	unlock(t, keystore)
	t.Setenv("TALLOWCTL_TEST_SECRET_REAL", "sk-real-secret-1234567890")
	if out, code := runKeyCmd(t, []string{
		"add", "provider/real",
		"--secret-env", "TALLOWCTL_TEST_SECRET_REAL",
		"--keyfile", keyfile, "--keystore", keystore,
	}); code != 0 {
		t.Fatalf("the follow-up add should have succeeded: exit %d, %q", code, out)
	}

	listOut := captureStdout(t, func() {
		keyCmd([]string{"list", "--keyfile", keyfile, "--keystore", keystore})
	})
	if strings.Contains(listOut, "provider/ghost") {
		t.Errorf("`key list` shows provider/ghost, whose add reported FAILURE: "+
			"the keystore disagrees with what the operator was told. list=%q", listOut)
	}
	if !strings.Contains(listOut, "provider/real") {
		t.Errorf("`key list` is missing provider/real, the key that did get "+
			"stored: %q", listOut)
	}
	if _, err := openStore(t, keyfile, keystore).Get("provider/ghost"); err == nil {
		t.Error("a fresh process loads provider/ghost from disk: the failed add " +
			"was persisted after all")
	}
	if got, err := openStore(t, keyfile, keystore).Get("provider/real"); err != nil ||
		got != "sk-real-secret-1234567890" {
		t.Errorf("the key that was actually stored did not survive: %q, %v", got, err)
	}
	// The failed secret must not be anywhere on disk in any form.
	assertNoPlaintext(t, keystore, "provider/real", "sk-ghost-should-not-persist")
}

// TestFailedKeyRmKeepsTheOldCredentialLive is the rm half, and it is the more
// dangerous direction. A delete that reports an error while having already
// removed the key stops the gateway from using a working credential, so the
// operator rotates or re-issues against a key that is still live.
func TestFailedKeyRmKeepsTheOldCredentialLive(t *testing.T) {
	// Seed while the directory is still writable.
	keyfile, keystore := e2ePaths(t)
	t.Setenv("TALLOWCTL_TEST_SECRET_LIVE", "sk-live-secret-abcdef1234")
	if out := captureStdout(t, func() {
		keyCmd([]string{
			"add", "provider/live",
			"--secret-env", "TALLOWCTL_TEST_SECRET_LIVE",
			"--keyfile", keyfile, "--keystore", keystore,
		})
	}); !strings.Contains(out, `stored key "provider/live"`) {
		t.Fatalf("seeding the add failed: %q", out)
	}
	if err := os.Chmod(filepath.Dir(keystore), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(keystore), 0o700) })

	out, code := runKeyCmd(t, []string{
		"rm", "provider/live", "--keyfile", keyfile, "--keystore", keystore,
	})
	if code == 0 {
		t.Fatalf("`key rm` reported SUCCESS (exit 0) against a read-only "+
			"keystore directory: %q", out)
	}

	// The credential must still be usable by a fresh process, which is what
	// "the remove failed" has to mean for the gateway.
	got, err := openStore(t, keyfile, keystore).Get("provider/live")
	if err != nil {
		t.Fatalf("the remove reported failure but provider/live is gone: %v", err)
	}
	if got != "sk-live-secret-abcdef1234" {
		t.Errorf("provider/live decrypted to %q, want the original credential", got)
	}
	listOut := captureStdout(t, func() {
		keyCmd([]string{"list", "--keyfile", keyfile, "--keystore", keystore})
	})
	if !strings.Contains(listOut, "provider/live") {
		t.Errorf("`key list` no longer shows provider/live after a failed rm: %q", listOut)
	}
}

// TestFailedKeyOverwriteKeepsTheOldSecret is the third direction, and the one
// with no library-level counterpart: a failed overwrite of an EXISTING key.
// This is the rotation path, so a failure that loses the new value or clobbers
// the old one either breaks rotation or silently keeps a revoked credential.
func TestFailedKeyOverwriteKeepsTheOldSecret(t *testing.T) {
	keyfile, keystore := e2ePaths(t)
	const oldSecret = "sk-old-secret-1234567890"
	t.Setenv("TALLOWCTL_TEST_SECRET_OLD", oldSecret)
	if out := captureStdout(t, func() {
		keyCmd([]string{
			"add", "provider/rotating",
			"--secret-env", "TALLOWCTL_TEST_SECRET_OLD",
			"--keyfile", keyfile, "--keystore", keystore,
		})
	}); !strings.Contains(out, `stored key "provider/rotating"`) {
		t.Fatalf("seeding the add failed: %q", out)
	}

	if err := os.Chmod(filepath.Dir(keystore), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(keystore), 0o700) })
	t.Setenv("TALLOWCTL_TEST_SECRET_NEW", "sk-new-secret-0987654321")

	out, code := runKeyCmd(t, []string{
		"add", "provider/rotating",
		"--secret-env", "TALLOWCTL_TEST_SECRET_NEW",
		"--keyfile", keyfile, "--keystore", keystore,
	})
	if code == 0 {
		t.Fatalf("the overwrite reported SUCCESS (exit 0) against a read-only "+
			"keystore: %q", out)
	}

	got, err := openStore(t, keyfile, keystore).Get("provider/rotating")
	if err != nil {
		t.Fatalf("provider/rotating is unreadable after a failed overwrite: %v", err)
	}
	if got != oldSecret {
		t.Errorf("provider/rotating now holds %q, want the ORIGINAL %q: a failed "+
			"rotation must leave the live credential untouched", got, oldSecret)
	}
	// The value that was supposed to replace it must not have landed.
	if got == "sk-new-secret-0987654321" {
		t.Error("the new secret was persisted even though the rotation reported failure")
	}
	// And once the filesystem is repaired, the rotation must actually work, or
	// the rollback has merely blocked rotation permanently.
	unlock(t, keystore)
	if out, code := runKeyCmd(t, []string{
		"add", "provider/rotating",
		"--secret-env", "TALLOWCTL_TEST_SECRET_NEW",
		"--keyfile", keyfile, "--keystore", keystore,
	}); code != 0 {
		t.Fatalf("the retried rotation should have succeeded: exit %d, %q", code, out)
	}
	if got, err := openStore(t, keyfile, keystore).Get("provider/rotating"); err != nil ||
		got != "sk-new-secret-0987654321" {
		t.Errorf("after a repaired filesystem the rotation did not take effect: "+
			"%q, %v", got, err)
	}
}

// TestKeyCmdAddOverExistingDoesNotCorruptOtherKeys is the corruption case, and
// it is why the add path is not just two isolated commands. Each `tallowctl key`
// invocation loads its own Store and writes the whole map, so an add that
// carries a stale in-memory view can drop every other ref from the keystore.
//
// The stale view is reachable the same way the ghost was: a failed add leaves
// its entry in the map, and a successful one overwrites before the write. In
// production that is one process handling several operator actions in sequence,
// and tallowctl is documented as reloadable without a restart, so this asserts
// what the keystore holds after such a sequence rather than assuming each
// command is a clean slate.
func TestKeyCmdAddOverExistingDoesNotCorruptOtherKeys(t *testing.T) {
	keyfile, keystore := e2ePaths(t)
	t.Setenv("TALLOWCTL_TEST_SECRET_A", "sk-a-secret-1234567890")
	t.Setenv("TALLOWCTL_TEST_SECRET_B", "sk-b-secret-0987654321")

	if out := captureStdout(t, func() {
		keyCmd([]string{
			"add", "provider/a",
			"--secret-env", "TALLOWCTL_TEST_SECRET_A",
			"--keyfile", keyfile, "--keystore", keystore,
		})
	}); !strings.Contains(out, `stored key "provider/a"`) {
		t.Fatalf("seeding provider/a failed: %q", out)
	}

	// Lock the keystore directory so the next add's save fails.
	if err := os.Chmod(filepath.Dir(keystore), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(keystore), 0o700) })
	t.Setenv("TALLOWCTL_TEST_SECRET_C", "sk-c-secret-1122334455")

	out, code := runKeyCmd(t, []string{
		"add", "provider/c",
		"--secret-env", "TALLOWCTL_TEST_SECRET_C",
		"--keyfile", keyfile, "--keystore", keystore,
	})
	if code == 0 {
		t.Fatalf("the add reported SUCCESS (exit 0) against a read-only "+
			"keystore: %q", out)
	}

	// Repair the filesystem, then perform a further add through the public
	// command. If the failed one left a stale entry in a map that is still
	// live, that entry is written out now.
	unlock(t, keystore)
	t.Setenv("TALLOWCTL_TEST_SECRET_D", "sk-d-secret-2233445566")
	if out, code := runKeyCmd(t, []string{
		"add", "provider/d",
		"--secret-env", "TALLOWCTL_TEST_SECRET_D",
		"--keyfile", keyfile, "--keystore", keystore,
	}); code != 0 {
		t.Fatalf("the follow-up add should have succeeded: exit %d, %q", code, out)
	}

	// Whatever the mechanism, the two keys the operator definitely stored must
	// still be there and must still decrypt.
	for _, ref := range []string{"provider/a", "provider/d"} {
		if _, err := openStore(t, keyfile, keystore).Get(ref); err != nil {
			t.Errorf("%s is missing after a failed add followed by a "+
				"successful one: the keystore lost a key the operator had "+
				"successfully stored (%v)", ref, err)
		}
	}
	listOut := captureStdout(t, func() {
		keyCmd([]string{"list", "--keyfile", keyfile, "--keystore", keystore})
	})
	if strings.Contains(listOut, "provider/c") {
		t.Errorf("`key list` shows provider/c, whose add reported FAILURE: %q", listOut)
	}
}

// TestKeyAddRefusesAnEmptySecret is the input-validation edge on the same
// public path. A blank key would be stored and would then be selected by the
// router as a credential that can never authenticate, so the CLI has to refuse
// it before the keystore is touched.
func TestKeyAddRefusesAnEmptySecret(t *testing.T) {
	keyfile, keystore := e2ePaths(t)
	t.Setenv("TALLOWCTL_TEST_SECRET_BLANK", "   \n")

	out, code := runKeyCmd(t, []string{
		"add", "provider/blank",
		"--secret-env", "TALLOWCTL_TEST_SECRET_BLANK",
		"--keyfile", keyfile, "--keystore", keystore,
	})
	if code == 0 {
		t.Errorf("`key add` accepted a whitespace-only secret (exit 0): %q", out)
	}
	if _, err := openStore(t, keyfile, keystore).Get("provider/blank"); err == nil {
		t.Error("a whitespace-only secret was stored: it would be selected as a " +
			"live credential that can never authenticate")
	}
	// A rejected add must not leave a keystore file behind claiming entries.
	if data, err := os.ReadFile(keystore); err == nil {
		if strings.Contains(string(data), "provider/blank") {
			t.Errorf("the rejected ref reached the keystore file: %s", data)
		}
	}
}
