package secret

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

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

func TestFailedAddIsInvisibleAfterRestart(t *testing.T) {
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
	if err := s.Set("existing", "kept"); err != nil {
		t.Fatalf("seed: %v", err)
	}

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

	if got, err := s.Get("existing"); err != nil || got != "kept" {
		t.Fatalf("after the failed add, Get(existing) = %q, %v; want %q", got, err, "kept")
	}
	if _, err := s.Get("stolen"); err == nil {
		t.Error("the failed credential is live in memory: the gateway would send a " +
			"key the operator was told was not stored")
	}

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

	if got, err := s.Get("api"); err != nil || got != "old-secret" {
		t.Errorf("after the failed overwrite Get(api) = %q, %v; want the previous "+
			"value %q", got, err, "old-secret")
	}

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

	if refs := onDiskRefs(t, path); !equalStrings(refs, want) {
		t.Errorf("after a restart the on-disk refs are %v, want %v", refs, want)
	}
}

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

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a failed save created the keystore file (stat err = %v)", err)
	}
}
