package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file runs the real, built tallowctl executable.
//
// The sibling suites in this package drive keyCmd two ways, and both stop short
// of the binary an operator actually runs. TestKeyCmdSubprocessHelper
// re-invokes the *test binary* with a flag and then calls keyCmd directly, so
// the command's own logic and its os.Exit are real, but main's argument
// dispatch never runs and the binary is never linked, flagged, or loaded. The
// in-process captureStdout tests are further from it still.
//
// That gap is worth closing because main is not a trivial pass-through. It
// decides which command runs from os.Args[1] and calls os.Exit(2) for anything
// it does not recognise. A typo in a subcommand name, a change that made
// `tallowctl key` unreachable from the command line, or a build that no longer
// links the key code at all would leave every existing test in this package
// green while breaking the tool for every user. None of them can see main,
// because they are the code main dispatches to.
//
// The binary is built with `go build` of the package under test rather than
// through a Makefile target, so the test cannot silently pass because a stale
// prebuilt binary happened to be on the path.

// buildTallowctl compiles the real binary into a temp dir and returns its path.
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

// TestTheRealBinaryDispatchesToKeyCmd walks the command line an operator types,
// against the built executable, and checks that main routes it to keyCmd and
// that the keystore on disk changes as a result.
func TestTheRealBinaryDispatchesToKeyCmd(t *testing.T) {
	bin := buildTallowctl(t)
	dir := t.TempDir()
	keystore := filepath.Join(dir, "keys.json")
	t.Setenv("TALLOW_MASTER_KEY", "integration-master-key")

	// The key command resolves a config before it touches the keystore, because
	// the keystore path can come from the config. TALLOW_CONFIG is set to the
	// example config the project ships, which is the path a new operator
	// actually takes, and --keystore overrides it for the store under test.
	// Without this the binary fails on a missing ./config.toml, which is a real
	// error but not the one under test.
	t.Setenv("TALLOW_CONFIG", exampleConfigPath(t))

	// Listing an empty keystore is not an error and must not be reported as one.
	if out, code := runBin(t, bin, "key", "list", "--keystore", keystore); code != 0 {
		t.Fatalf("tallowctl key list on an empty keystore exited %d: %s",
			code, out)
	}

	// The secret is passed through the environment rather than as a positional
	// argument, which is what keyCmd reads and which keeps the value out of the
	// process table.
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
	// Checked on the artifact the binary actually produced, not on an in-memory
	// Store: this is the file an attacker would read.
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

	// Readable back by the same binary, which proves the file it wrote is one it
	// can read rather than merely one that exists.
	out, code = runBin(t, bin, "key", "list", "--keystore", keystore)
	if code != 0 {
		t.Fatalf("tallowctl key list after add exited %d: %s", code, out)
	}
	if !strings.Contains(out, "p1:k1") {
		t.Errorf("key list does not mention the ref the binary just stored:\n%s", out)
	}

	// And removable by it.
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

// TestTheRealBinaryRejectsUnknownCommands covers both dispatch layers. main
// exits 2 for an unrecognised top-level command, and keyCmd exits 2 for an
// unrecognised key action. Without an explicit check, a typo would look like
// success to any script driving the tool.
func TestTheRealBinaryRejectsUnknownCommands(t *testing.T) {
	bin := buildTallowctl(t)
	for _, args := range [][]string{
		{"keys"},        // plausible typo for "key"
		{"Key", "list"}, // wrong case
		{"totally-not-a-command"},
		{"key", "keys"},         // unknown key action
		{"key", "Set", "p1:k1"}, // unknown key action
		{"key", "add"},          // missing the required ref
		{"key", "rm"},           // missing the required ref
	} {
		out, code := runBin(t, bin, args...)
		if code == 0 {
			t.Errorf("tallowctl %s exited 0, want non-zero: a rejected command "+
				"must not look like it succeeded. output:\n%s",
				strings.Join(args, " "), out)
		}
	}
}

// TestTheRealBinaryFailsClosedOnABadKeyRef re-observes, for the shipped
// artifact, the exit-status property the child-process suite exists to
// establish. The real binary is the one an operator or a script sees, and it is
// the only place the exit code actually reaches the caller.
func TestTheRealBinaryFailsClosedOnABadKeyRef(t *testing.T) {
	bin := buildTallowctl(t)
	keystore := filepath.Join(t.TempDir(), "keys.json")
	t.Setenv("TALLOW_MASTER_KEY", "integration-master-key")
	t.Setenv("TALLOW_CONFIG", exampleConfigPath(t))

	// Removing a ref that was never stored must fail rather than report success,
	// or a provisioning script would believe it had revoked a credential.
	out, code := runBin(t, bin, "key", "rm", "p1:never-stored", "--keystore", keystore)
	if code == 0 {
		t.Errorf("removing an absent ref exited 0, want non-zero: a script "+
			"revoking credentials would believe it succeeded.\n%s", out)
	}
	// An add with an empty value must fail too: storing an empty credential
	// produces a key that fails at the provider with a confusing 401 much later.
	t.Setenv("TALLOW_TEST_EMPTY", "   ")
	if out, code = runBin(t, bin, "key", "add", "p1:empty",
		"--keystore", keystore, "--secret-env", "TALLOW_TEST_EMPTY"); code == 0 {
		t.Errorf("adding an empty secret exited 0, want non-zero:\n%s", out)
	}
}

// exampleConfigPath returns the path to the config the project ships, failing
// the test if it is missing. Using the real example rather than a hand-written
// minimal one means these tests also notice if the example stops being a config
// the binary can load, which is the first thing a new operator runs.
func exampleConfigPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// The test runs in the package directory, so the example is two levels up.
	p := filepath.Join(wd, "..", "..", "examples", "config.toml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the shipped example config is missing, so the CLI tests "+
			"cannot use the path a real operator takes: %v", err)
	}
	return p
}

// runBin runs the built binary and returns its combined output and exit code.
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
