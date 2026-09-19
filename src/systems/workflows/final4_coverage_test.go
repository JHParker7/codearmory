package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInitHTTPClient_TLS exercises initHTTPClient's client-cert + CA load branches
// with a generated self-signed cert/key (the os.Exit error branches stay uncovered).
func TestInitHTTPClient_TLS(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	caPath := filepath.Join(dir, "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	os.WriteFile(certPath, certPEM, 0o600)                                                              //nolint:errcheck
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600) //nolint:errcheck
	os.WriteFile(caPath, certPEM, 0o600)                                                                //nolint:errcheck

	t.Setenv("TLS_CLIENT_CERT_FILE", certPath)
	t.Setenv("TLS_CLIENT_KEY_FILE", keyPath)
	t.Setenv("TLS_CA_FILE", caPath)

	if c := initHTTPClient(); c == nil {
		t.Fatal("initHTTPClient returned nil with TLS configured")
	}
}
