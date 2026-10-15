package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func readOnlyDir(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "locked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return dir
}

func isWritable(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		_ = os.Remove(probe)
		return false
	}
	_ = os.Remove(probe)
	return true
}

func TestFailedSetDoesNotResurrectOnLaterSave(t *testing.T) {
	dir := readOnlyDir(t, t.TempDir())
	if isWritable(t, dir) {
		t.Skip("filesystem allows writes despite the read-only directory mode")
	}

	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ks.json")
	st, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	if err := st.Set("ghost", "SUPERSECRET"); err == nil {
		t.Fatal("expected the first Set to fail while the directory is read-only")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("real", "value"); err != nil {
		t.Fatalf("second Set should succeed: %v", err)
	}

	readBack, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	refs := readBack.List()
	t.Logf("refs persisted to disk = %v", refs)
	if _, err := readBack.Get("ghost"); err == nil {
		t.Errorf("a key whose add reported FAILURE is now persisted on disk: %v", refs)
	}
	if _, err := readBack.Get("real"); err != nil {
		t.Errorf("the key that was actually added is missing: %v", err)
	}
}

func TestFailedDeleteKeepsTheKeyInMemory(t *testing.T) {
	dir := readOnlyDir(t, t.TempDir())
	if isWritable(t, dir) {
		t.Skip("filesystem allows writes despite the read-only directory mode")
	}

	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}

	seed, err := LoadStore(filepath.Join(dir, "ks.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := seed.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	st, err := LoadStore(filepath.Join(dir, "ks.json"), box)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("k"); err == nil {
		t.Fatal("expected Delete to fail while the directory is read-only")
	}
	if _, err := st.Get("k"); err != nil {
		t.Errorf("Delete reported failure but the key is already gone from memory: %v", err)
	}
}
