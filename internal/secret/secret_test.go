package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	b, err := NewBox([]byte("e2e-master-key"))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return b
}

// TestBoxSealOpenRoundTrip verifies the AES-256-GCM seal/open cycle and that
// distinct Seal calls produce distinct ciphertexts (fresh random nonce each
// time, so ciphertext equality cannot leak plaintext equality).
func TestBoxSealOpenRoundTrip(t *testing.T) {
	box := testBox(t)
	plain := "sk-provider-secret-123"

	enc1, err := box.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	enc2, err := box.Seal([]byte(plain))
	if err != nil {
		t.Fatalf("Seal (2nd): %v", err)
	}
	if enc1 == enc2 {
		t.Fatal("two Seals of the same plaintext produced identical ciphertext; nonce reuse?")
	}
	if strings.Contains(enc1, plain) {
		t.Fatal("ciphertext contains plaintext")
	}

	got, err := box.Open(enc1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("round trip mismatch: got %q want %q", got, plain)
	}
}

// TestBoxOpenRejectsTampering verifies GCM authentication rejects modified
// ciphertext and malformed inputs.
func TestBoxOpenRejectsTampering(t *testing.T) {
	box := testBox(t)
	enc, err := box.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := box.Open(enc + "AAAA"); err == nil {
		t.Fatal("expected error for corrupted ciphertext")
	}
	if _, err := box.Open("not base64 !!!"); err == nil {
		t.Fatal("expected error for non-base64 input")
	}
	if _, err := box.Open(""); err == nil {
		t.Fatal("expected error for empty ciphertext")
	}

	// Cross-box open must fail: different master key = different derived key.
	other, err := NewBox([]byte("another-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(enc); err == nil {
		t.Fatal("expected decryption failure under a different master key")
	}
}

// TestKeystoreRoundTripAndPersistence covers Set/Get/List/Delete plus the
// on-disk format: ciphertext only (no plaintext on disk), 0600 permissions,
// and durability across a reopen.
func TestKeystoreRoundTripAndPersistence(t *testing.T) {
	dir := t.TempDir()
	ksPath := filepath.Join(dir, "keys.json")
	box := testBox(t)

	s, err := LoadStore(ksPath, box)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := s.Set("p1:k1", "sk-live-key-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("p1:k2", "sk-live-key-2"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := s.Get("p1:k1")
	if err != nil || got != "sk-live-key-1" {
		t.Fatalf("Get: got %q, %v", got, err)
	}
	if refs := s.List(); len(refs) != 2 {
		t.Fatalf("List: got %d refs, want 2", len(refs))
	}

	// On disk: no plaintext anywhere in the raw file, and 0600 perms.
	raw, err := os.ReadFile(ksPath)
	if err != nil {
		t.Fatalf("read keystore: %v", err)
	}
	if strings.Contains(string(raw), "sk-live-key") {
		t.Fatal("keystore file contains plaintext secret")
	}
	if !strings.Contains(string(raw), `"version": 1`) {
		t.Fatalf("keystore file missing version header: %s", raw)
	}
	st, err := os.Stat(ksPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("keystore file mode = %o, want 600", got)
	}

	// Delete removes the entry and persists.
	if err := s.Delete("p1:k1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("p1:k1"); err == nil {
		t.Fatal("expected error after Delete")
	}

	// Durability: reopen from disk and confirm the remaining entry decrypts.
	s2, err := LoadStore(ksPath, box)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got2, err := s2.Get("p1:k2")
	if err != nil || got2 != "sk-live-key-2" {
		t.Fatalf("reopen Get: got %q, %v", got2, err)
	}
}

// TestKeystoreRejectsWrongVersion verifies version guarding on the keystore
// file so a future format change cannot be silently misread.
func TestKeystoreRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	ksPath := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(ksPath, []byte(`{"version": 99, "entries": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(ksPath, testBox(t)); err == nil || !strings.Contains(err.Error(), "unsupported keystore version") {
		t.Fatalf("expected version error, got %v", err)
	}
}

// TestLoadMasterKeyPrecedence verifies env wins over keyfile, keyfile content
// is trimmed, and missing sources produce a hard error.
func TestLoadMasterKeyPrecedence(t *testing.T) {
	t.Setenv(EnvMasterKey, "env-key-wins")
	got, err := LoadMasterKey("no/such/file")
	if err != nil || string(got) != "env-key-wins" {
		t.Fatalf("env must win over keyfile: got %q, %v", got, err)
	}

	// Empty env falls back to the keyfile; content must be trimmed.
	t.Setenv(EnvMasterKey, "")
	dir := t.TempDir()
	kf := filepath.Join(dir, "master.key")
	if err := os.WriteFile(kf, []byte("  file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadMasterKey(kf)
	if err != nil || string(got) != "file-key" {
		t.Fatalf("keyfile fallback: got %q, %v", got, err)
	}

	// Neither source: hard error.
	if _, err := LoadMasterKey(""); err == nil {
		t.Fatal("expected error when no master key source is available")
	}

	// Empty keyfile is rejected.
	t.Setenv(EnvMasterKey, "")
	empty := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-keyfile error, got %v", err)
	}
}
