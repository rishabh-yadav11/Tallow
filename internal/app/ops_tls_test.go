package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rishabh-yadav11/tallow/internal/config"
)

// writeSelfSignedCert writes a throwaway cert/key pair to temp files and returns
// their paths, so a test can exercise the TLS wiring without shipping a cert.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return certPath, keyPath
}

// TestServerTLSBuildsConfigWhenBothSet proves the cert is loaded and a usable
// TLS config results when both tls_cert and tls_key are set.
func TestServerTLSBuildsConfigWhenBothSet(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)
	cfg, err := serverTLS(config.Server{TLSCert: certPath, TLSKey: keyPath})
	if err != nil {
		t.Fatalf("serverTLS: %v", err)
	}
	if cfg == nil || len(cfg.Certificates) == 0 {
		t.Fatalf("expected a configured TLS server config, got %#v", cfg)
	}
	srv := newProxyServer("127.0.0.1:0", nil, config.Params{}, cfg)
	if srv.TLSConfig != cfg {
		t.Fatalf("newProxyServer did not attach the TLS config")
	}
}

// TestServerTLSPlainWhenUnset proves TLS stays off (nil config) when neither
// field is set.
func TestServerTLSPlainWhenUnset(t *testing.T) {
	cfg, err := serverTLS(config.Server{})
	if err != nil {
		t.Fatalf("serverTLS with no cert/key should not error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil TLS config when neither cert nor key is set, got %#v", cfg)
	}
}

// TestServerTLSRequiresBoth fails fast when only one of the pair is set.
func TestServerTLSRequiresBoth(t *testing.T) {
	if _, err := serverTLS(config.Server{TLSCert: "/nonexistent/x.pem"}); err == nil {
		t.Fatalf("setting only tls_cert should fail")
	}
	if _, err := serverTLS(config.Server{TLSKey: "/nonexistent/x.key"}); err == nil {
		t.Fatalf("setting only tls_key should fail")
	}
	// A bad cert path must be caught at startup, not on the first connection.
	if _, err := serverTLS(config.Server{TLSCert: "/nonexistent/x.pem", TLSKey: "/nonexistent/x.key"}); err == nil {
		t.Fatalf("unreadable cert should fail fast")
	}
}

// TestServeProxyServesHTTPS proves end-to-end that a configured TLS listener
// actually answers HTTPS rather than plain HTTP (activation, not just wiring).
func TestServeProxyServesHTTPS(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)
	cfg, err := serverTLS(config.Server{TLSCert: certPath, TLSKey: keyPath})
	if err != nil {
		t.Fatalf("serverTLS: %v", err)
	}
	srv := newProxyServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), config.Params{}, cfg)
	if srv.TLSConfig != cfg {
		t.Fatalf("newProxyServer did not attach the TLS config")
	}

	// Bind the listener ourselves so the real address (not :0) is known, then
	// serve over TLS via the server's attached config.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	addr := ln.Addr().String()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("https get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("https status = %d, want 204", resp.StatusCode)
	}

	cctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(cctx)
}
