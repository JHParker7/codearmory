package main

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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildMux_Routes(t *testing.T) {
	h := buildMux()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz got %d", w.Code)
	}
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/services", nil))
	if w2.Code == http.StatusNotFound {
		t.Error("/services route not registered")
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

func TestRun_ServesAndShutsDown(t *testing.T) {
	requireDB(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]
	ln.Close()
	t.Setenv("PORT", port)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	up := false
	for range 100 {
		resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				up = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if !up {
		t.Fatal("server did not come up")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not shut down")
	}
}
