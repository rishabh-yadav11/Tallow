package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func runKeyCmd(t *testing.T, args []string) (output string, exitCode int) {
	t.Helper()
	if os.Getenv("TALLOWCTL_KEY_SUBPROCESS") == "1" {

		t.Fatal("runKeyCmd was called from inside the child process")
	}

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

	var ee *exec.ExitError
	if !asExitError(err, &ee) {
		t.Fatalf("running the tallowctl child: %v", err)
	}
	return string(out), ee.ExitCode()
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
