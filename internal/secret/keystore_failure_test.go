package secret

import (
	"os"
	"path/filepath"
	"testing"
)

// readOnlyDir returns a directory that cannot be written to, and restores its
// permissions on cleanup. It skips the test if the filesystem ignores the mode.
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

// Set and Delete both mutated the in-memory map before attempting the write, so a
// failed write left the keystore describing something that was not on disk.
//
// The Set half is the sharper one. `tallowctl key add` would print an error, so
// the operator reasonably believed nothing had been stored - but the secret was
// live in memory, and the next unrelated key operation wrote the whole map out,
// silently persisting a credential the operator had been told did not exist. A
// failed add must leave no trace that a later save can resurrect.
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

	// Repair the filesystem, then add an unrelated key. If the failed Set left
	// its entry in the map, the ghost is written out in full here.
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

// The Delete half runs the other way: the entry was removed from memory before
// the write, so a failed delete reported an error while having already stopped
// the gateway from using that credential. The caller cannot tell from the
// return value whether the key is live or gone, so on failure the key has to
// stay exactly as it was.
func TestFailedDeleteKeepsTheKeyInMemory(t *testing.T) {
	dir := readOnlyDir(t, t.TempDir())
	if isWritable(t, dir) {
		t.Skip("filesystem allows writes despite the read-only directory mode")
	}

	box, err := NewBox([]byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	// Seed the entry while the directory is still writable.
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
