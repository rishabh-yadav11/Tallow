package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/secret"
)

// captureStdout runs fn while capturing everything it writes to os.Stdout.
// keyCmd reports success only through stdout and hard-exits (os.Exit) on
// failure, so captured text is the in-process observable for its success
// paths. The exit paths themselves cannot be exercised in-process.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	os.Stdout = old
	return <-out
}

// e2ePaths sets up an isolated master-key file + keystore path and neutralizes
// any ambient TALLOW_MASTER_KEY so keyCmd resolves the key from the keyfile.
// Both CLI flags are always passed, so keyCmd never falls back to config.Load
// and ~/.tallow is never touched.
func e2ePaths(t *testing.T) (keyfile, keystore string) {
	t.Helper()
	t.Setenv(secret.EnvMasterKey, "")
	dir := t.TempDir()
	keyfile = filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyfile, []byte("tallowctl-e2e-master-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyfile, filepath.Join(dir, "keystore.json")
}

// openStore reopens the keystore through internal/secret — the same path the
// gateway uses to read provider keys.
func openStore(t *testing.T, keyfile, keystore string) *secret.Store {
	t.Helper()
	mk, err := secret.LoadMasterKey(keyfile)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	box, err := secret.NewBox(mk)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	st, err := secret.LoadStore(keystore, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return st
}

// assertNoPlaintext is the encryption contract: the keystore file must contain
// the ref (so the check cannot pass vacuously on an empty or ref-less file)
// but never the secrets, neither raw nor base64-obfuscated.
func assertNoPlaintext(t *testing.T, keystore, ref string, secrets ...string) {
	t.Helper()
	data, err := os.ReadFile(keystore)
	if err != nil {
		t.Fatalf("read keystore: %v", err)
	}
	if !bytes.Contains(data, []byte(ref)) {
		t.Fatalf("keystore does not contain ref %q; leak check would be vacuous", ref)
	}
	for _, s := range secrets {
		if bytes.Contains(data, []byte(s)) {
			t.Fatalf("keystore file contains plaintext secret %q", s)
		}
		if b64 := base64.StdEncoding.EncodeToString([]byte(s)); bytes.Contains(data, []byte(b64)) {
			t.Fatalf("keystore file contains base64 of secret %q (obfuscation is not encryption)", s)
		}
	}
}

// TestReadSecret covers readSecret's three sources and its error paths.
// It returns raw bytes — trimming is the add path's job, not readSecret's.
func TestReadSecret(t *testing.T) {
	t.Run("from env", func(t *testing.T) {
		t.Setenv("TALLOWCTL_TEST_SECRET_ENV", "sk-env-value")
		got, err := readSecret("", "TALLOWCTL_TEST_SECRET_ENV")
		if err != nil {
			t.Fatalf("readSecret: %v", err)
		}
		if got != "sk-env-value" {
			t.Fatalf("got %q, want %q", got, "sk-env-value")
		}
	})

	t.Run("env wins over file", func(t *testing.T) {
		t.Setenv("TALLOWCTL_TEST_SECRET_ENV", "sk-env-value")
		path := filepath.Join(t.TempDir(), "secret.txt")
		if err := os.WriteFile(path, []byte("sk-file-value"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readSecret(path, "TALLOWCTL_TEST_SECRET_ENV")
		if err != nil || got != "sk-env-value" {
			t.Fatalf("env must win: got %q, %v", got, err)
		}
	})

	t.Run("env set but empty", func(t *testing.T) {
		t.Setenv("TALLOWCTL_TEST_SECRET_ENV", "")
		if _, err := readSecret("", "TALLOWCTL_TEST_SECRET_ENV"); err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("expected empty-env error, got %v", err)
		}
	})

	t.Run("from file returns raw bytes", func(t *testing.T) {
		for _, content := range []string{"sk-file-value", "sk-file-value\n", "  padded  "} {
			path := filepath.Join(t.TempDir(), "secret.txt")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readSecret(path, "")
			if err != nil {
				t.Fatalf("readSecret(%q): %v", content, err)
			}
			if got != content {
				t.Fatalf("got %q, want raw %q (readSecret must not trim)", got, content)
			}
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := readSecret(filepath.Join(t.TempDir(), "absent.txt"), ""); err == nil {
			t.Fatal("expected error for missing secret file")
		}
	})

	t.Run("from stdin dash", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = r
		t.Cleanup(func() { os.Stdin = old })
		if _, err := w.WriteString("sk-stdin-value\n"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := readSecret("-", "")
		if err != nil {
			t.Fatalf("readSecret(-): %v", err)
		}
		if got != "sk-stdin-value\n" {
			t.Fatalf("got %q, want %q", got, "sk-stdin-value\n")
		}
	})

	t.Run("no source", func(t *testing.T) {
		_, err := readSecret("", "")
		if err == nil || !strings.Contains(err.Error(), "provide --secret-file or --secret-env") {
			t.Fatalf("expected no-source error, got %v", err)
		}
	})
}

// TestKeyCmdAddListRoundTrip drives the real flag scan + add + list path with
// post-positional flags ("key add <ref> --secret-env X"), an isolated master
// keyfile and keystore, and the secret sourced from an env var.
func TestKeyCmdAddListRoundTrip(t *testing.T) {
	keyfile, keystore := e2ePaths(t)
	t.Setenv("TALLOWCTL_TEST_SECRET_MAIN", "sk-main-secret-1234567890")

	addOut := captureStdout(t, func() {
		keyCmd([]string{
			"add", "provider/main",
			"--secret-env", "TALLOWCTL_TEST_SECRET_MAIN",
			"--keyfile", keyfile, "--keystore", keystore,
		})
	})
	if !strings.Contains(addOut, `stored key "provider/main"`) || !strings.Contains(addOut, "(encrypted)") {
		t.Fatalf("unexpected add output: %q", addOut)
	}

	listOut := captureStdout(t, func() {
		keyCmd([]string{"list", "--keyfile", keyfile, "--keystore", keystore})
	})
	if listOut != "provider/main\n" {
		t.Fatalf("unexpected list output: %q", listOut)
	}

	// Round trip: decrypt what the CLI stored, exactly as the gateway would.
	got, err := openStore(t, keyfile, keystore).Get("provider/main")
	if err != nil {
		t.Fatalf("Get after add: %v", err)
	}
	if got != "sk-main-secret-1234567890" {
		t.Fatalf("stored secret mismatch: got %q", got)
	}

	assertNoPlaintext(t, keystore, "provider/main", "sk-main-secret-1234567890")
}

// TestKeyCmdSecretFileAndRemove covers --secret-file (trailing newline,
// trimmed by the add path), sorted list output, and rm's persistence.
func TestKeyCmdSecretFileAndRemove(t *testing.T) {
	keyfile, keystore := e2ePaths(t)

	secretFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secretFile, []byte("sk-file-secret-0987654321\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		keyCmd([]string{
			"add", "provider/backup",
			"--secret-file", secretFile,
			"--keyfile", keyfile, "--keystore", keystore,
		})
	})
	t.Setenv("TALLOWCTL_TEST_SECRET_ALPHA", "sk-alpha-secret-1122334455")
	captureStdout(t, func() {
		keyCmd([]string{
			"add", "provider/alpha",
			"--secret-env", "TALLOWCTL_TEST_SECRET_ALPHA",
			"--keyfile", keyfile, "--keystore", keystore,
		})
	})

	st := openStore(t, keyfile, keystore)
	got, err := st.Get("provider/backup")
	if err != nil || got != "sk-file-secret-0987654321" {
		t.Fatalf("file secret mismatch: got %q, %v", got, err)
	}
	got, err = st.Get("provider/alpha")
	if err != nil || got != "sk-alpha-secret-1122334455" {
		t.Fatalf("env secret mismatch: got %q, %v", got, err)
	}

	listOut := captureStdout(t, func() {
		keyCmd([]string{"list", "--keyfile", keyfile, "--keystore", keystore})
	})
	if listOut != "provider/alpha\nprovider/backup\n" {
		t.Fatalf("list must be sorted; got %q", listOut)
	}

	captureStdout(t, func() {
		keyCmd([]string{"rm", "provider/backup", "--keyfile", keyfile, "--keystore", keystore})
	})
	// Store caches entries in memory at load; reopen from disk to observe the rm.
	got, err = openStore(t, keyfile, keystore).Get("provider/backup")
	if err == nil {
		t.Fatalf("Get succeeded for removed ref: %q", got)
	}
	listOut = captureStdout(t, func() {
		keyCmd([]string{"list", "--keyfile", keyfile, "--keystore", keystore})
	})
	if listOut != "provider/alpha\n" {
		t.Fatalf("list after rm: %q", listOut)
	}

	assertNoPlaintext(t, keystore, "provider/alpha",
		"sk-alpha-secret-1122334455", "sk-file-secret-0987654321")
}
