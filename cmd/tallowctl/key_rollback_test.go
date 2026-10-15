package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lockedKeystoreDir(t *testing.T) (keyfile, keystore string) {
	t.Helper()
	keyfile, keystore = e2ePaths(t)
	if err := os.Chmod(filepath.Dir(keystore), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(keystore), 0o700) })

	probe := filepath.Join(filepath.Dir(keystore), ".probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(probe)
		t.Skip("filesystem allows writes despite a read-only directory mode")
	}
	return keyfile, keystore
}

func unlock(t *testing.T, keystore string) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(keystore), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestFailedKeyAddLeavesNothingOnDiskForTheNextProcess(t *testing.T) {
	keyfile, keystore := lockedKeystoreDir(t)
	t.Setenv("TALLOWCTL_TEST_SECRET_GHOST", "sk-ghost-should-not-persist")

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

	assertNoPlaintext(t, keystore, "provider/real", "sk-ghost-should-not-persist")
}

func TestFailedKeyRmKeepsTheOldCredentialLive(t *testing.T) {

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

	if got == "sk-new-secret-0987654321" {
		t.Error("the new secret was persisted even though the rotation reported failure")
	}

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

	unlock(t, keystore)
	t.Setenv("TALLOWCTL_TEST_SECRET_D", "sk-d-secret-2233445566")
	if out, code := runKeyCmd(t, []string{
		"add", "provider/d",
		"--secret-env", "TALLOWCTL_TEST_SECRET_D",
		"--keyfile", keyfile, "--keystore", keystore,
	}); code != 0 {
		t.Fatalf("the follow-up add should have succeeded: exit %d, %q", code, out)
	}

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

	if data, err := os.ReadFile(keystore); err == nil {
		if strings.Contains(string(data), "provider/blank") {
			t.Errorf("the rejected ref reached the keystore file: %s", data)
		}
	}
}
