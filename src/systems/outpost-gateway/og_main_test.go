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

	"github.com/google/uuid"
)

// TestRun_ServesAndShutsDown starts the full server lifecycle on an ephemeral
// port, confirms it serves, then cancels the context and asserts a clean
// shutdown — covering run()'s migrate/serve/Shutdown bootstrap path.
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
		t.Fatal("server did not come up on /healthz")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not shut down after ctx cancel")
	}
}

func writeTestCA(t *testing.T) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

func TestBuildClientTLSConfig(t *testing.T) {
	// Default: no client auth required.
	t.Setenv("TLS_CLIENT_AUTH", "")
	cfg, err := buildClientTLSConfig()
	if err != nil || cfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("default: cfg=%v err=%v", cfg, err)
	}

	// request → RequestClientCert.
	t.Setenv("TLS_CLIENT_AUTH", "request")
	cfg, err = buildClientTLSConfig()
	if err != nil || cfg.ClientAuth != tls.RequestClientCert {
		t.Fatalf("request: %v / %v", cfg, err)
	}

	// require without CA file → error.
	t.Setenv("TLS_CLIENT_AUTH", "require")
	t.Setenv("TLS_CLIENT_CA_FILE", "")
	if _, err = buildClientTLSConfig(); err == nil {
		t.Error("require without CA should error")
	}

	// require with a non-existent file → error.
	t.Setenv("TLS_CLIENT_CA_FILE", filepath.Join(t.TempDir(), "nope.pem"))
	if _, err = buildClientTLSConfig(); err == nil {
		t.Error("require with missing CA file should error")
	}

	// require with a garbage (non-PEM) file → error.
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("not a pem"), 0o600) //nolint:errcheck
	t.Setenv("TLS_CLIENT_CA_FILE", bad)
	if _, err = buildClientTLSConfig(); err == nil {
		t.Error("require with invalid PEM should error")
	}

	// require with a valid CA → RequireAndVerifyClientCert.
	t.Setenv("TLS_CLIENT_CA_FILE", writeTestCA(t))
	cfg, err = buildClientTLSConfig()
	if err != nil || cfg.ClientAuth != tls.RequireAndVerifyClientCert || cfg.ClientCAs == nil {
		t.Fatalf("require valid CA: %v / %v", cfg, err)
	}
}

func TestSecret_FilePath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	os.WriteFile(f, []byte("file-secret\n"), 0o600) //nolint:errcheck
	t.Setenv("OG_SEC_FILE", f)
	if got := secret("OG_SEC"); got != "file-secret" {
		t.Errorf("secret from *_FILE = %q, want file-secret (trimmed)", got)
	}
	// Falls back to the plain env var when no _FILE is set.
	t.Setenv("OG_SEC2", "env-secret")
	if got := secret("OG_SEC2"); got != "env-secret" {
		t.Errorf("secret from env = %q", got)
	}
}

func TestBuildMux_Routes(t *testing.T) {
	h := buildMux()

	// Health route returns 200.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz got %d, want 200", w.Code)
	}

	// OpenAPI route is wired.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("/openapi.yaml got %d, want 200", w2.Code)
	}

	// A user-facing route exists (unauthenticated → not 404; gatekeeper denies).
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/outposts", nil))
	if w3.Code == http.StatusNotFound {
		t.Error("/outposts route not registered")
	}
}

func TestHandleAckCommand_Success(t *testing.T) {
	requireDB(t)
	o, key := seedOutpost(t, "u-ack", "", "chaos")
	// Acking an unknown command id is a no-op that still succeeds (idempotent).
	r := outpostReq(http.MethodPost, "/outpost/commands/"+uuid.New().String()+"/ack", o.OutpostID, key, nil)
	r.SetPathValue("id", uuid.New().String())
	w := httptest.NewRecorder()
	handleAckCommand(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("ack got %d, want 204: %s", w.Code, w.Body.String())
	}
}
