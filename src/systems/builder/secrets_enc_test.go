package main

import (
	"strings"
	"testing"
)

const testSecretsKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

// withSecretsKey initialises builder's AES cipher for a test and restores it.
func withSecretsKey(t *testing.T) {
	t.Helper()
	orig := builderSecretsAEAD
	t.Setenv("BUILDER_SECRETS_KEY", testSecretsKey)
	initSecretsEncryption()
	t.Cleanup(func() {
		builderSecretsMu.Lock()
		builderSecretsAEAD = orig
		builderSecretsMu.Unlock()
	})
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	withSecretsKey(t)
	if !secretsEncryptionEnabled() {
		t.Fatal("expected encryption enabled")
	}
	plain := "postgres://forge:pw@db:5432/forge"
	ct, err := encryptSecret(plain, "forge")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if string(ct) == plain {
		t.Fatal("ciphertext must not equal plaintext")
	}
	got, err := decryptSecret(ct, "forge")
	if err != nil || got != plain {
		t.Fatalf("decrypt = (%q,%v), want (%q,nil)", got, err, plain)
	}
}

func TestDecrypt_AADMismatchFails(t *testing.T) {
	withSecretsKey(t)
	ct, _ := encryptSecret("secret-url", "forge")
	// A URL stored for "forge" must not decrypt under another service's name —
	// this is what stops a DB-write attacker swapping URLs between services.
	if _, err := decryptSecret(ct, "workflows"); err == nil {
		t.Fatal("expected decrypt to fail when the service (AAD) differs")
	}
}

func TestEncrypt_DisabledWhenNoKey(t *testing.T) {
	builderSecretsMu.Lock()
	orig := builderSecretsAEAD
	builderSecretsAEAD = nil
	builderSecretsMu.Unlock()
	t.Cleanup(func() {
		builderSecretsMu.Lock()
		builderSecretsAEAD = orig
		builderSecretsMu.Unlock()
	})
	if secretsEncryptionEnabled() {
		t.Fatal("expected disabled")
	}
	if _, err := encryptSecret("x", "forge"); err == nil {
		t.Fatal("expected error when encryption is not initialised")
	}
}

func TestRedactedDBHost(t *testing.T) {
	got, err := redactedDBHost("postgres://user:secret@db.example.com:5432/forge")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "db.example.com:5432/forge" {
		t.Fatalf("got %q, want db.example.com:5432/forge", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Fatalf("redacted host must not contain credentials: %q", got)
	}
	if _, err := redactedDBHost("mysql://h/db"); err == nil {
		t.Error("non-postgres scheme should error")
	}
	if _, err := redactedDBHost("postgres:///db"); err == nil {
		t.Error("missing host should error")
	}
}
