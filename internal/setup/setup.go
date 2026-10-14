package setup

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/secret"
)

const (
	keyfilePath  = "~/.tallow/master.key"
	keystorePath = "~/.tallow/keys.json"
	storePath    = "~/.tallow/tallow.db"
	adminSocket  = "~/.tallow/admin.sock"
)

func Run(cfgPath string, in io.Reader, out io.Writer) error {
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("setup: config already exists: %s", cfgPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("setup: stat %s: %w", cfgPath, err)
	}

	sc := bufio.NewScanner(in)

	name, err := required(sc, out, "provider name: ")
	if err != nil {
		return errors.New("setup aborted: no config written")
	}
	baseURL, err := baseURL(sc, out, name)
	if err != nil {
		return errors.New("setup aborted: no config written")
	}
	apiKey, err := optional(sc, out, "provider api key (empty to add later): ")
	if err != nil {
		return errors.New("setup aborted: no config written")
	}
	model, err := required(sc, out, "model name (exposed to clients; sent upstream as-is): ")
	if err != nil {
		return errors.New("setup aborted: no config written")
	}

	masterKey, err := resolveMasterKey(expandHome(keyfilePath))
	if err != nil {
		return err
	}
	box, err := secret.NewBox(masterKey)
	if err != nil {
		return fmt.Errorf("setup: master key box: %w", err)
	}

	clientKey := randomHex(16)
	ref := name + ":default"
	stored := false
	if apiKey != "" {
		store, err := secret.LoadStore(expandHome(keystorePath), box)
		if err != nil {
			return fmt.Errorf("setup: keystore: %w", err)
		}
		if err := store.Set(ref, apiKey); err != nil {
			return fmt.Errorf("setup: store provider key %s: %w", ref, err)
		}
		stored = true
	}

	cfg := buildConfig(name, baseURL, model, clientKey, stored)
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("setup: encode config: %w", err)
	}
	if err := writeFileAtomic(cfgPath, buf.Bytes(), 0o600); err != nil {
		return err
	}

	if privateNote(baseURL) {
		fmt.Fprintln(out, "private base_url needs -allow-private-upstream on tallow")
	}
	fmt.Fprintf(out, "config written: %s\n", cfgPath)
	fmt.Fprintf(out, "provider: %s\n", name)
	fmt.Fprintf(out, "model: %s\n", model)
	fmt.Fprintf(out, "client api key: %s\n", clientKey)
	if stored {
		fmt.Fprintf(out, "provider api key: stored in keystore\n")
	} else {
		fmt.Fprintf(out, "no provider api key stored — add the key and a [[provider.key]] block with id default to %s\n", cfgPath)
	}
	return nil
}

func required(sc *bufio.Scanner, out io.Writer, label string) (string, error) {
	for {
		line, err := readLine(sc, out, label)
		if err != nil {
			return "", err
		}
		if line != "" {
			return line, nil
		}
	}
}

func optional(sc *bufio.Scanner, out io.Writer, label string) (string, error) {
	return readLine(sc, out, label)
}

func baseURL(sc *bufio.Scanner, out io.Writer, provider string) (string, error) {
	for {
		line, err := readLine(sc, out, "provider base url: ")
		if err != nil {
			return "", err
		}
		if err := config.ValidateBaseURL(provider, line); err != nil {
			fmt.Fprintln(out, err)
			continue
		}
		return line, nil
	}
}

func readLine(sc *bufio.Scanner, out io.Writer, label string) (string, error) {
	fmt.Fprint(out, label)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return strings.TrimSpace(sc.Text()), nil
}

func resolveMasterKey(path string) ([]byte, error) {
	if v := os.Getenv(secret.EnvMasterKey); v != "" {
		return []byte(v), nil
	}
	if _, err := os.Stat(path); err == nil {
		return secret.LoadMasterKey(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("setup: stat keyfile %s: %w", path, err)
	}
	key := randomHex(32)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("setup: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return nil, fmt.Errorf("setup: write keyfile %s: %w", path, err)
	}
	return []byte(key), nil
}

func buildConfig(name, baseURL, model, clientKey string, withKey bool) *config.Config {
	t, f := true, false
	d5, d30, d90 := 5, 30, 90
	p := config.Provider{
		Name:           name,
		BaseURL:        baseURL,
		Timeout:        "120s",
		HealthCheck:    true,
		HealthInterval: "60s",
	}
	if withKey {
		p.Key = []config.Key{{ID: "default", Ref: name + ":default"}}
	}
	return &config.Config{
		Version: config.SchemaVersion,
		Server: config.Server{
			Listen:          "127.0.0.1:8080",
			AdminSocket:     adminSocket,
			MaxConcurrent:   16,
			QueueTimeout:    "30s",
			ReadTimeout:     "30s",
			WriteTimeout:    "0s",
			IdleTimeout:     "120s",
			MaxRequestBytes: 8 << 20,
			StickyTTL:       "15m",
		},
		Auth: config.Auth{
			APIKeys: []string{clientKey},
			Open:    f,
		},
		Secret: config.SecretCfg{
			Keyfile:  keyfilePath,
			Keystore: keystorePath,
		},
		Store: config.StoreCfg{
			Path:      storePath,
			RawBodies: &t,
		},
		Cache: config.CacheCfg{
			Enabled:    &t,
			MaxEntries: 1024,
			TTL:        "10m",
			MaxBytes:   256 * 1024 * 1024,
		},
		Retention: config.Retention{
			RawBodiesDays: &d5,
			MetadataDays:  &d30,
			ErrorsDays:    &d90,
			RollupCron:    "1h",
			VacuumCron:    "6h",
		},
		Compaction: config.Compaction{
			Enabled:            &t,
			MaxToolOutputChars: 8000,
		},
		Observability: config.Observability{Enabled: true},
		Provider:      []config.Provider{p},
		Alias: []config.Alias{{
			Name: model,
			Target: []config.Target{{
				Provider:        name,
				Model:           model,
				ContextWindow:   32768,
				SupportsTools:   true,
				SupportsStream:  true,
				PriceInputPerM:  0,
				PriceOutputPerM: 0,
			}},
		}},
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func privateNote(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return !config.IsPublicAddr(ip)
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	for _, p := range []string{"127.", "10.", "192.168.", "169.254."} {
		if strings.HasPrefix(host, p) {
			return true
		}
	}
	if parts := strings.Split(host, "."); len(parts) >= 2 && parts[0] == "172" {
		var v int
		if _, err := fmt.Sscanf(parts[1], "%d", &v); err == nil && v >= 16 && v <= 31 {
			return true
		}
	}
	return false
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("setup: mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("setup: create %s: %w", tmp, err)
	}
	if _, err := fh.Write(b); err != nil {
		fh.Close()
		os.Remove(tmp)
		return fmt.Errorf("setup: write %s: %w", tmp, err)
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		os.Remove(tmp)
		return fmt.Errorf("setup: sync %s: %w", tmp, err)
	}
	if err := fh.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("setup: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("setup: rename %s: %w", tmp, err)
	}
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}
