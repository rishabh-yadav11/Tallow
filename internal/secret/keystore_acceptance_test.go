package secret

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// This file verifies the rollback contract the way an operator meets it: across
// a restart, using LoadStore on the real file, and by reading the bytes on disk
// rather than trusting the in-memory view.
//
// keystore_failure_test.go injects a write failure and checks the in-memory map.
// That is the right unit test, but it stops one process lifetime short of the
// question that matters. When `tallowctl key add` prints an error and the
// operator restarts Tallow, what does the gateway actually load? The contract
// Set and Delete now promise is that the in-memory map always describes what is
// on disk, so the answers here must agree across a reload: a failed add leaves
// no trace on disk, a failed delete leaves the old credential usable, and a
// failed overwrite leaves the previous credential intact. Every case also pins
// the on-disk bytes, so a store that merely failed to read back correctly
// cannot pass.

// loadFresh returns a keystore on disk seeded with the given entries, loaded
// through the real LoadStore, plus the box and path needed to drive it.
func loadFresh(t *testing.T, path string, box *Box, seed map[string]string) *Store {
	t.Helper()
	s, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	for ref, plaintext := range seed {
		if err := s.Set(ref, plaintext); err != nil {
			t.Fatalf("seed %s: %v", ref, err)
		}
	}
	return s
}

func newTestBox(t *testing.T) *Box {
	t.Helper()
	box, err := NewBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

// onDiskRefs reads the keystore file directly and returns its refs, without
// going through a Store. This is the ground truth an operator would find on
// disk after a crash or restart.
func onDiskRefs(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read keystore: %v", err)
	}
	var f keystoreFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("the keystore on disk is not valid JSON: %v", err)
	}
	refs := make([]string, 0, len(f.Entries))
	for ref := range f.Entries {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFailedAddIsInvisibleAfterRestart is the operator-visible half of the
// rollback. The add reports an error, so the operator believes nothing was
// stored. After a restart the gateway must load a keystore with no such entry -
// not merely an in-memory map that happens to agree.
func TestFailedAddIsInvisibleAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keys.json")
	box := newTestBox(t)

	// Seed a working store while the directory is still writable.
	s, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := s.Set("existing", "kept"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now make the directory unwritable, which is the real-world shape of the
	// failure: disk full, read-only mount, or a permissions mistake.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if isWritable(t, dir) {
		t.Skip("the filesystem ignores directory permissions; cannot inject a write failure here")
	}

	if err := s.Set("stolen", "must-never-persist"); err == nil {
		t.Fatal("expected the save to fail on a read-only filesystem")
	}

	// The live store still serves the pre-existing credential, and the rejected
	// credential is not live.
	if got, err := s.Get("existing"); err != nil || got != "kept" {
		t.Fatalf("after the failed add, Get(existing) = %q, %v; want %q", got, err, "kept")
	}
	if _, err := s.Get("stolen"); err == nil {
		t.Error("the failed credential is live in memory: the gateway would send a " +
			"key the operator was told was not stored")
	}

	// Now the decisive check: a fresh LoadStore, as a restarting Tallow would
	// do, must not see the rejected credential, and the on-disk file must never
	// have mentioned it.
	if got := onDiskRefs(t, path); !equalStrings(got, []string{"existing"}) {
		t.Errorf("on-disk refs are %v, want [existing]: the rejected credential "+
			"reached disk", got)
	}
	reopened, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := reopened.Get("stolen"); err == nil {
		t.Error("after a restart the rejected credential is live: the failed add " +
			"was persisted by a later save")
	}
	if got, err := reopened.Get("existing"); err != nil || got != "kept" {
		t.Errorf("after a restart Get(existing) = %q, %v; want %q", got, err, "kept")
	}
}

// TestFailedOverwriteKeepsTheOldCredentialAfterRestart pins the overwrite case,
// which is the one an operator is most likely to hit: rotating a key whose save
// fails must leave the OLD key working, not no key and not the new one.
func TestFailedOverwriteKeepsTheOldCredentialAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keys.json")
	box := newTestBox(t)

	s, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := s.Set("api", "old-secret"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Make the directory unwritable so the rotation's save fails for real.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if isWritable(t, dir) {
		t.Skip("the filesystem ignores directory permissions; cannot inject a write failure here")
	}

	if err := s.Set("api", "new-secret"); err == nil {
		t.Fatal("expected the overwrite to fail on a read-only filesystem")
	}

	// The in-memory view must still be the old value.
	if got, err := s.Get("api"); err != nil || got != "old-secret" {
		t.Errorf("after the failed overwrite Get(api) = %q, %v; want the previous "+
			"value %q", got, err, "old-secret")
	}
	// And the previous value must still be on disk and still loadable.
	if got := onDiskRefs(t, path); !equalStrings(got, []string{"api"}) {
		t.Errorf("on-disk refs are %v, want [api]", got)
	}
	reopened, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, err := reopened.Get("api"); err != nil || got != "old-secret" {
		t.Errorf("after a restart Get(api) = %q, %v; want %q: a failed rotation "+
			"left the gateway with no working credential", got, err, "old-secret")
	}
}

// TestFailedDeleteKeepsTheKeyUsableAfterRestart is the mirror image. A delete
// that reports an error must not have taken the credential away: the gateway
// should keep working, and a restart must still find the key.
func TestFailedDeleteKeepsTheKeyUsableAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keys.json")
	box := newTestBox(t)

	s, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := s.Set("api", "still-needed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if isWritable(t, dir) {
		t.Skip("the filesystem ignores directory permissions; cannot inject a write failure here")
	}

	if err := s.Delete("api"); err == nil {
		t.Fatal("expected the delete to fail on a read-only filesystem")
	}

	// Still live in memory, and still on disk after a restart.
	if got, err := s.Get("api"); err != nil || got != "still-needed" {
		t.Errorf("after the failed delete Get(api) = %q, %v; want %q: a delete "+
			"that reported an error took the credential away anyway", got, err, "still-needed")
	}
	if got := onDiskRefs(t, path); !equalStrings(got, []string{"api"}) {
		t.Errorf("on-disk refs are %v, want [api]", got)
	}
	reopened, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, err := reopened.Get("api"); err != nil || got != "still-needed" {
		t.Errorf("after a restart Get(api) = %q, %v; want %q", got, err, "still-needed")
	}
	if refs := reopened.List(); !equalStrings(refs, []string{"api"}) {
		t.Errorf("List after restart is %v, want [api]", refs)
	}
}

// TestSuccessfulOperationsSurviveRestart is the positive control for the whole
// file. If these do not round-trip, the failures above would prove nothing,
// because a store that never wrote anything would also "keep" the old value.
func TestSuccessfulOperationsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	box := newTestBox(t)

	s := loadFresh(t, path, box, map[string]string{"a": "one", "b": "two"})
	if err := s.Set("c", "three"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete("b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := []string{"a", "c"}
	if got := onDiskRefs(t, path); !equalStrings(got, want) {
		t.Errorf("on-disk refs are %v, want %v", got, want)
	}

	// List returns refs in Go map iteration order, which is deliberately
	// randomised. The CLI sorts before printing (cmd/tallowctl/key.go), and so
	// does the on-disk helper above, so this is the one assertion that has to
	// sort for itself. Asserting a fixed order here made the test fail
	// intermittently, which is worse than not testing it: it turns a real
	// regression signal into noise a maintainer learns to re-run away from.
	reopened, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := reopened.List()
	sort.Strings(got)
	if !equalStrings(got, want) {
		t.Errorf("List after restart is %v, want %v", got, want)
	}
	for ref, want := range map[string]string{"a": "one", "c": "three"} {
		if got, err := reopened.Get(ref); err != nil || got != want {
			t.Errorf("Get(%s) = %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := reopened.Get("b"); err == nil {
		t.Error("the deleted ref came back after a restart")
	}
	// A deleted credential must not be recoverable from the file either.
	if refs := onDiskRefs(t, path); !equalStrings(refs, want) {
		t.Errorf("after a restart the on-disk refs are %v, want %v", refs, want)
	}
}

// TestNoTemporaryFileSurvivesAFailedSave checks the failure path leaves no debris.
// A leftover keys.json.tmp holding a sealed credential is a plaintext-at-rest
// leak of the rejected write, and the next save would rename it into place.
func TestNoTemporaryFileSurvivesAFailedSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keys.json")
	box := newTestBox(t)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if isWritable(t, dir) {
		t.Skip("the filesystem ignores directory permissions; cannot inject a write failure here")
	}

	s, err := LoadStore(path, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := s.Set("api", "value"); err == nil {
		t.Fatal("expected the save to fail on a read-only filesystem")
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a temp keystore survived a failed save (stat err = %v): it can "+
			"leak the rejected credential or be renamed into place by a later save", err)
	}
	// The keystore itself must not have been created either.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a failed save created the keystore file (stat err = %v)", err)
	}
}
