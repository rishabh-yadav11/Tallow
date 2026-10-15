package secret

import (
	"os"
	"path/filepath"
	"testing"
)

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
