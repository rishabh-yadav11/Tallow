package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestKeyCmdSubprocessHelper(t *testing.T) {
	if os.Getenv("TALLOWCTL_KEY_SUBPROCESS") != "1" {
		t.Skip("parent process: this test only runs as the child spawned by runKeyCmd")
	}

	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("TALLOWCTL_KEY_ARGS_JSON")), &args); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	keyCmd(args)

}
