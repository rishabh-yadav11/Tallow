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
	EnvMasterKey    = "TALLOW_MASTER_KEY"
	keystoreVersion = 1
)

type Box struct{ aead cipher.AEAD }

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

func (b *Box) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nil, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

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

type Store struct {
	path    string
	box     *Box
	mu      sync.Mutex
	entries map[string]string
}

func LoadStore(path string, box *Box) (*Store, error) {
	if box == nil {
		return nil, fmt.Errorf("secret: LoadStore requires a non-nil box")
	}
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

func (s *Store) Set(ref, plaintext string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.box.Seal([]byte(plaintext))
	if err != nil {
		return err
	}
	prev, hadPrev := s.entries[ref]
	s.entries[ref] = c
	if err := s.saveLocked(); err != nil {

		if hadPrev {
			s.entries[ref] = prev
		} else {
			delete(s.entries, ref)
		}
		return err
	}
	return nil
}

func (s *Store) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.entries[ref]
	if !ok {
		return fmt.Errorf("secret: no keystore entry for %q", ref)
	}
	delete(s.entries, ref)
	if err := s.saveLocked(); err != nil {
		s.entries[ref] = prev
		return err
	}
	return nil
}

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
