package secret

import (
	"os"
	"path/filepath"
	"testing"
)

// The keystore holds every provider credential the gateway uses. These cover
// the states an operator can actually find on disk after an interrupted
// upgrade, a bad edit, or a permissions mistake - each of which must fail
// closed and say something actionable, rather than silently starting the
// gateway with an empty or half-read key set.

// TestLoadStoreRejectsCorruptJSON covers a truncated or hand-edited keystore.
// Returning an empty store here would be the worst outcome: the gateway would
// start normally and then fail every upstream call with an authentication
// error, pointing the operator at their provider rather than at the keystore.
func TestLoadStoreRejectsCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"truncated", `{"version":1,"entries":{"a":"xx"`},
		{"not an object", `[]`},
		{"empty file", ``},
		{"wrong field type", `{"version":"one","entries":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := LoadStore(path, box)
			if err == nil {
				t.Fatalf("LoadStore accepted a corrupt keystore and returned %d entries", len(st.List()))
			}
			if st != nil {
				t.Error("LoadStore returned both a store and an error")
			}
		})
	}
}

// TestLoadStoreRejectsAFutureVersion guards the version gate. A keystore
// written by a newer tallowctl may hold fields this binary would silently drop
// on the next Set, so it must be refused rather than downgraded.
func TestLoadStoreRejectsAFutureVersion(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(path, box); err == nil {
		t.Fatal("LoadStore accepted a keystore from a future version")
	}
}

// TestLoadStoreAcceptsAnEmptyEntriesMap covers the legitimate state where a
// keystore exists but holds no keys. That is what `tallowctl key add` produces
// before the first add, and it must load cleanly rather than being treated as
// corrupt.
func TestLoadStoreAcceptsAnEmptyEntriesMap(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if got := len(st.List()); got != 0 {
		t.Errorf("List = %d refs, want 0", got)
	}
	if err := st.Set("a", "b"); err != nil {
		t.Fatalf("an empty keystore must still be writable: %v", err)
	}
}

// TestKeystoreFileIsNotWorldReadable checks the permission of the file the
// keystore is written to. These are plaintext-equivalent provider credentials;
// 0o600 is the only acceptable mode, and a umask or a pre-existing file must
// not widen it.
func TestKeystoreFileIsNotWorldReadable(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("POSIX file modes do not apply on Windows")
	}
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	st, err := LoadStore(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("keystore mode = %04o, want no group or world access", perm)
	}

	// A pre-existing file with loose permissions must be tightened, not left
	// alone. os.OpenFile ignores the mode for an existing file.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("k2", "v2"); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("after rewriting a pre-existing 0644 file, mode = %04o: the save "+
			"did not tighten permissions on a file holding provider credentials", perm)
	}
}

// TestNoTemporaryFileSurvivesASuccessfulSave checks the atomic-write path
// leaves no .tmp behind. A leftover temp file is a full plaintext-shaped copy
// of the keystore sitting next to the real one, unpruned.
func TestNoTemporaryFileSurvivesASuccessfulSave(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	st, err := LoadStore(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a temp keystore file survived a successful save: %v", err)
	}
}

// TestGetReportsAMissingRef checks the plain miss path, which the gateway hits
// on every reload for a key the operator has not added yet.
func TestGetReportsAMissingRef(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := LoadStore(filepath.Join(dir, "ks.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("nope"); err == nil {
		t.Fatal("Get succeeded for a ref that was never set")
	}
	if err := st.Delete("nope"); err == nil {
		t.Fatal("Delete succeeded for a ref that was never set")
	}
}

// TestGetFailsClosedOnAWrongMasterKey is the tamper case at the keystore
// level. A keystore written under one master key must not decrypt under
// another, and must not return the ciphertext as if it were plaintext.
func TestGetFailsClosedOnAWrongMasterKey(t *testing.T) {
	dir := t.TempDir()
	boxA, err := NewBox([]byte("master-A"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	st, err := LoadStore(path, boxA)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("k", "SECRET"); err != nil {
		t.Fatal(err)
	}

	boxB, err := NewBox([]byte("master-B"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadStore(path, boxB)
	if err != nil {
		t.Fatal(err)
	}
	got, err := other.Get("k")
	if err == nil {
		t.Fatalf("decrypting under the wrong master key returned %q", got)
	}
	if got != "" {
		t.Errorf("a failed decrypt returned %q rather than an empty string", got)
	}
}

// TestLoadStoreReturnsNoStoreOnAReadError covers the failure that is neither a
// missing file nor a parse error: the keystore exists but cannot be read. The
// important property is that LoadStore returns a nil store. Handing back a
// usable-looking store alongside an error invites a caller to carry on with an
// empty key set, and the gateway would then start normally and fail every
// upstream call with an authentication error.
func TestLoadStoreReturnsNoStoreOnAReadError(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		t.Skip("running as root: the file mode is not enforced")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	// If the mode was not actually enforced, the read succeeds and there is
	// nothing to assert.
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("the file is still readable; this environment cannot exercise the read error")
	}

	st, err := LoadStore(path, box)
	if err == nil {
		t.Fatal("LoadStore succeeded on an unreadable keystore")
	}
	if st != nil {
		t.Errorf("LoadStore returned a non-nil store alongside an error: %d entries",
			len(st.List()))
	}
}

// TestLoadStoreRejectsAnOlderKeystoreVersion closes the other side of the
// version gate. Version 0 is what a pre-versioning build, or a hand-written
// file with the field omitted, would produce. Accepting it would read the
// entries under a schema this binary has never validated.
func TestLoadStoreRejectsAnOlderKeystoreVersion(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	if err := os.WriteFile(path, []byte(`{"version":0,"entries":{"a":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(path, box); err == nil {
		t.Fatal("LoadStore accepted a version 0 keystore")
	}
}

// TestSetOverwriteRestoresThePriorValueOnFailure is the overwrite variant of
// the rollback. Replacing an existing credential is the common case, and a
// failed write must leave the working credential in place rather than a
// half-applied mixture.
func TestSetOverwriteRestoresThePriorValueOnFailure(t *testing.T) {
	dir := t.TempDir()
	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	st, err := LoadStore(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set("k", "ORIGINAL"); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if os.Getuid() == 0 {
		t.Skip("running as root: the read-only directory mode is not enforced")
	}

	if err := st.Set("k", "REPLACEMENT"); err == nil {
		t.Fatal("expected the overwrite to fail on a read-only directory")
	}
	got, err := st.Get("k")
	if err != nil {
		t.Fatalf("the original credential is unreadable after a failed overwrite: %v", err)
	}
	if got != "ORIGINAL" {
		t.Errorf("credential = %q after a failed overwrite, want %q", got, "ORIGINAL")
	}
}
