package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildMux_Routes(t *testing.T) {
	h := buildMux()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz got %d, want 200", w.Code)
	}
	// A representative authenticated route must be registered (no-token → not 404).
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/users", nil))
	if w2.Code == http.StatusNotFound {
		t.Error("/users route not registered")
	}
}

func TestBuildClientTLSConfig(t *testing.T) {
	t.Setenv("TLS_CLIENT_AUTH", "request")
	if cfg, err := buildClientTLSConfig(); err != nil || cfg.ClientAuth != tls.RequestClientCert {
		t.Fatalf("request: %v / %v", cfg, err)
	}
	t.Setenv("TLS_CLIENT_AUTH", "require")
	t.Setenv("TLS_CLIENT_CA_FILE", "")
	if _, err := buildClientTLSConfig(); err == nil {
		t.Error("require without CA should error")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("nope"), 0o600) //nolint:errcheck
	t.Setenv("TLS_CLIENT_CA_FILE", bad)
	if _, err := buildClientTLSConfig(); err == nil {
		t.Error("invalid PEM should error")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, IsCA: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600) //nolint:errcheck
	t.Setenv("TLS_CLIENT_CA_FILE", caPath)
	if cfg, err := buildClientTLSConfig(); err != nil || cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("valid CA: %v / %v", cfg, err)
	}
}
