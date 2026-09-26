package main

import (
	"encoding/json"
	"os"
	"testing"
)

// TestKeyCmdSubprocessHelper is the child half of runKeyCmd. The test binary
// re-invokes itself with TALLOWCTL_KEY_SUBPROCESS=1 and the path to a JSON file
// holding the arguments; this test then calls the real keyCmd with them, so the
// command's own os.Exit on failure becomes the child's exit status and the
// parent can assert on it.
//
// It carries no assertions of its own. The parent asserts, because the parent
// is what holds the keystore state and the follow-up checks.
func TestKeyCmdSubprocessHelper(t *testing.T) {
	if os.Getenv("TALLOWCTL_KEY_SUBPROCESS") != "1" {
		t.Skip("parent process: this test only runs as the child spawned by runKeyCmd")
	}
	// The args file's CONTENTS are passed, not its path, so this test does not
	// depend on how the testing package treats arguments after "--".
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("TALLOWCTL_KEY_ARGS_JSON")), &args); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	keyCmd(args)
	// keyCmd returns normally only when it succeeded; it os.Exit(1)s otherwise.
}
