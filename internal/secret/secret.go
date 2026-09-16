// Package secret handles the master key and encrypted side-storage keystore.
// Provider API keys are never written to disk in plaintext: they are AES-GCM
// sealed under a key derived (SHA-256) from the master key.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// EnvMasterKey is the environment variable holding the master key.
	EnvMasterKey    = "TALLOW_MASTER_KEY"
	keystoreVersion = 1
)

// Box encrypts/decrypts opaque byte payloads under the master key.
type Box struct{ aead cipher.AEAD }

// NewBox derives an AES-256-GCM key from the master key bytes.
func NewBox(masterKey []byte) (*Box, error) {
	h := sha256.Sum256(masterKey)
	block, err := aes.NewCipher(h[:])
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: g}, nil
}

// Seal encrypts plaintext, returning base64(nonce || ciphertext).
func (b *Box) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nil, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// Open decrypts a Seal result.
func (b *Box) Open(enc string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("secret: decode: %w", err)
	}
	if len(raw) < b.aead.NonceSize() {
		return nil, errors.New("secret: ciphertext too short")
	}
	return b.aead.Open(nil, raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():], nil)
}

// LoadMasterKey resolves the master key: TALLOW_MASTER_KEY env first, then the
// keyfile path (trimmed). Hard-fails if neither is available.
func LoadMasterKey(keyfile string) ([]byte, error) {
	if v := os.Getenv(EnvMasterKey); v != "" {
		return []byte(v), nil
	}
	if keyfile != "" {
		b, err := os.ReadFile(keyfile)
		if err != nil {
			return nil, fmt.Errorf("secret: read keyfile %s: %w", keyfile, err)
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return nil, fmt.Errorf("secret: keyfile %s is empty", keyfile)
		}
		return []byte(s), nil
	}
	return nil, fmt.Errorf("secret: no master key (set %s or secret.keyfile)", EnvMasterKey)
}

type keystoreFile struct {
	Version int               `json:"version"`
	Entries map[string]string `json:"entries"`
}

// Store is a small encrypted side-storage file mapping ref -> ciphertext.
// Managed by tallowctl (key add/rm); the gateway only reads it.
type Store struct {
	path    string
	box     *Box
	mu      sync.Mutex
	entries map[string]string
}

// LoadStore opens (creating if absent) the keystore at path.
func LoadStore(path string, box *Box) (*Store, error) {
	s := &Store{path: path, box: box, entries: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("secret: read keystore: %w", err)
	}
	var f keystoreFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("secret: parse keystore %s: %w", path, err)
	}
	if f.Version != keystoreVersion {
		return nil, fmt.Errorf("secret: unsupported keystore version %d", f.Version)
	}
	if f.Entries != nil {
		s.entries = f.Entries
	}
	return s, nil
}

// Get decrypts the plaintext secret for ref.
func (s *Store) Get(ref string) (string, error) {
	s.mu.Lock()
	c, ok := s.entries[ref]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("secret: no keystore entry for %q", ref)
	}
	b, err := s.box.Open(c)
	if err != nil {
		return "", fmt.Errorf("secret: decrypt %q: %w", ref, err)
	}
	return string(b), nil
}

// Set encrypts and stores plaintext under ref, persisting atomically.
func (s *Store) Set(ref, plaintext string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.box.Seal([]byte(plaintext))
	if err != nil {
		return err
	}
	s.entries[ref] = c
	return s.saveLocked()
}

// Delete removes ref from the keystore.
func (s *Store) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[ref]; !ok {
		return fmt.Errorf("secret: no keystore entry for %q", ref)
	}
	delete(s.entries, ref)
	return s.saveLocked()
}

// List returns the set of stored refs.
func (s *Store) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for r := range s.entries {
		out = append(out, r)
	}
	return out
}

func (s *Store) saveLocked() error {
	f := keystoreFile{Version: keystoreVersion, Entries: s.entries}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// fsync before rename so a crash between write and rename cannot lose the
	// last key add/rm. Clean up the temp file on any failure.
	cleanup := func() {
		fh.Close()
		os.Remove(tmp)
	}
	if _, err := fh.Write(b); err != nil {
		cleanup()
		return err
	}
	if err := fh.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := fh.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path)
}
