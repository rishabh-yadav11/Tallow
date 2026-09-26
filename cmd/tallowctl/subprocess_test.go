package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// runKeyCmd drives `tallowctl key` in a child process and returns its combined
// output and exit code.
//
// A child process is the only honest way to test the failure paths of these
// commands. keyCmd reports failure by printing to stderr and calling os.Exit,
// so an in-process call cannot observe it: the test binary itself would be gone,
// with no exit code to assert and nothing left to run the follow-up checks in.
// That is why the sibling tests here use captureStdout for success paths and
// this is used for failure paths.
//
// The arguments travel as JSON in the environment rather than as command-line
// arguments after "--", because how the testing package treats those is an
// implementation detail of flag parsing, not a contract worth depending on. The
// observable under test is the command's own exit code.
func runKeyCmd(t *testing.T, args []string) (output string, exitCode int) {
	t.Helper()
	if os.Getenv("TALLOWCTL_KEY_SUBPROCESS") == "1" {
		// Only meaningful in the child; the helper test calls keyCmd directly.
		t.Fatal("runKeyCmd was called from inside the child process")
	}

	// The arguments travel as JSON in the environment. JSON rather than a
	// NUL-separated list because arguments can contain any byte except NUL and
	// exec rejects NUL in an env value. JSON also escapes whatever an argument
	// contains, so a ref or flag value with a quote or backslash survives.
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestKeyCmdSubprocessHelper$")
	cmd.Env = append(os.Environ(),
		"TALLOWCTL_KEY_SUBPROCESS=1",
		"TALLOWCTL_KEY_ARGS_JSON="+string(b),
	)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	// Any non-nil error from a child is an exit error, and its code is the
	// command's own. There is no other failure mode here: the binary exists
	// (it is this test binary) and the args file was just written.
	var ee *exec.ExitError
	if !asExitError(err, &ee) {
		t.Fatalf("running the tallowctl child: %v", err)
	}
	return string(out), ee.ExitCode()
}

// asExitError is errors.As specialised to *exec.ExitError, so subprocess_test.go
// does not need to import errors for one call.
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
