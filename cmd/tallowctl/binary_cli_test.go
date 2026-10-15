package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildTallowctl(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the binary build in -short mode")
	}
	bin := filepath.Join(t.TempDir(), "tallowctl")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the tallowctl binary: %v\n%s", err, out)
	}
	return bin
}

func TestTheRealBinaryDispatchesToKeyCmd(t *testing.T) {
	bin := buildTallowctl(t)
	dir := t.TempDir()
	keystore := filepath.Join(dir, "keys.json")
	t.Setenv("TALLOW_MASTER_KEY", "integration-master-key")

	t.Setenv("TALLOW_CONFIG", exampleConfigPath(t))

	if out, code := runBin(t, bin, "key", "list", "--keystore", keystore); code != 0 {
		t.Fatalf("tallowctl key list on an empty keystore exited %d: %s",
			code, out)
	}

	const secretVal = "sk-real-binary-secret"
	t.Setenv("TALLOW_TEST_KEY_VALUE", secretVal)

	out, code := runBin(t, bin, "key", "add", "p1:k1",
		"--keystore", keystore, "--secret-env", "TALLOW_TEST_KEY_VALUE")
	if code != 0 {
		t.Fatalf("tallowctl key add exited %d: %s", code, out)
	}
	if _, err := os.Stat(keystore); err != nil {
		t.Fatalf("the real binary did not create the keystore: %v", err)
	}

	raw, err := os.ReadFile(keystore)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secretVal) {
		t.Errorf("the keystore written by the real binary holds the secret in "+
			"plaintext: %s", raw)
	}
	if !strings.Contains(string(raw), "p1:k1") {
		t.Errorf("the keystore does not even mention the ref, so it cannot be "+
			"usable: %s", raw)
	}

	out, code = runBin(t, bin, "key", "list", "--keystore", keystore)
	if code != 0 {
		t.Fatalf("tallowctl key list after add exited %d: %s", code, out)
	}
	if !strings.Contains(out, "p1:k1") {
		t.Errorf("key list does not mention the ref the binary just stored:\n%s", out)
	}

	if out, code = runBin(t, bin, "key", "rm", "p1:k1", "--keystore", keystore); code != 0 {
		t.Fatalf("tallowctl key rm exited %d: %s", code, out)
	}
	out, code = runBin(t, bin, "key", "list", "--keystore", keystore)
	if code != 0 {
		t.Fatalf("tallowctl key list after rm exited %d: %s", code, out)
	}
	if strings.Contains(out, "p1:k1") {
		t.Errorf("key list still reports p1:k1 after the real binary removed it:\n%s", out)
	}
}

func TestTheRealBinaryRejectsUnknownCommands(t *testing.T) {
	bin := buildTallowctl(t)
	for _, args := range [][]string{
		{"keys"},
		{"Key", "list"},
		{"totally-not-a-command"},
		{"key", "keys"},
		{"key", "Set", "p1:k1"},
		{"key", "add"},
		{"key", "rm"},
	} {
		out, code := runBin(t, bin, args...)
		if code == 0 {
			t.Errorf("tallowctl %s exited 0, want non-zero: a rejected command "+
				"must not look like it succeeded. output:\n%s",
				strings.Join(args, " "), out)
		}
	}
}

func TestTheRealBinaryFailsClosedOnABadKeyRef(t *testing.T) {
	bin := buildTallowctl(t)
	keystore := filepath.Join(t.TempDir(), "keys.json")
	t.Setenv("TALLOW_MASTER_KEY", "integration-master-key")
	t.Setenv("TALLOW_CONFIG", exampleConfigPath(t))

	out, code := runBin(t, bin, "key", "rm", "p1:never-stored", "--keystore", keystore)
	if code == 0 {
		t.Errorf("removing an absent ref exited 0, want non-zero: a script "+
			"revoking credentials would believe it succeeded.\n%s", out)
	}

	t.Setenv("TALLOW_TEST_EMPTY", "   ")
	if out, code = runBin(t, bin, "key", "add", "p1:empty",
		"--keystore", keystore, "--secret-env", "TALLOW_TEST_EMPTY"); code == 0 {
		t.Errorf("adding an empty secret exited 0, want non-zero:\n%s", out)
	}
}

func exampleConfigPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(wd, "..", "..", "examples", "config.toml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the shipped example config is missing, so the CLI tests "+
			"cannot use the path a real operator takes: %v", err)
	}
	return p
}

func runBin(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("run %s %s: %v", bin, strings.Join(args, " "), err)
	}
	return string(out), ee.ExitCode()
}
