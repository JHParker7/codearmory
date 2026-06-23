package main

import (
	"encoding/hex"
	"testing"
)

// testSecretsKey (a valid 64-hex / 32-byte BUILDER_SECRETS_KEY) is declared in
// secrets_enc_test.go and shared across the package's secret tests.

func resetDerivation() {
	derivationMu.Lock()
	derivationRoot = nil
	derivationMu.Unlock()
}

func TestDeriveSharedKey_DeterministicAnd32ByteHex(t *testing.T) {
	resetDerivation()
	t.Setenv("BUILDER_SECRETS_KEY", testSecretsKey)
	initSecretDerivation()
	if !secretDerivationEnabled() {
		t.Fatal("derivation should be enabled with a valid key")
	}

	got := deriveSharedKey("hooks-trigger-key")
	// Deterministic: same name → same value on every call.
	if again := deriveSharedKey("hooks-trigger-key"); again != got {
		t.Fatalf("not deterministic: %q vs %q", got, again)
	}
	// 64 hex chars decoding to exactly 32 bytes — the AES-256 key format blueprints
	// and workflows require.
	if len(got) != 64 {
		t.Fatalf("len = %d, want 64 hex chars", len(got))
	}
	raw, err := hex.DecodeString(got)
	if err != nil || len(raw) != 32 {
		t.Fatalf("not 32-byte hex: err=%v len=%d", err, len(raw))
	}
}

func TestDerive_DistinctPerNameAndService(t *testing.T) {
	resetDerivation()
	t.Setenv("BUILDER_SECRETS_KEY", testSecretsKey)
	initSecretDerivation()

	// Distinct shared-secret names → distinct keys.
	if deriveSharedKey("hooks-trigger-key") == deriveSharedKey("outpost-internal-key") {
		t.Error("different shared names produced the same key")
	}
	// Same private-key name, different services → distinct keys.
	if derivePrivateKey("blueprints", "encryption-key") == derivePrivateKey("workflows", "encryption-key") {
		t.Error("same private key name across services collided")
	}
	// Shared and private domains are separated even for the same string.
	if deriveSharedKey("encryption-key") == derivePrivateKey("blueprints", "encryption-key") {
		t.Error("shared/private domain separation failed")
	}
}

func TestDerive_DisabledWithoutKey(t *testing.T) {
	resetDerivation()
	t.Setenv("BUILDER_SECRETS_KEY", "")
	initSecretDerivation()
	if secretDerivationEnabled() {
		t.Fatal("derivation should be disabled without a key")
	}
	if deriveSharedKey("hooks-trigger-key") != "" {
		t.Error("disabled derivation should return empty string")
	}
	if derivePrivateKey("blueprints", "encryption-key") != "" {
		t.Error("disabled derivation should return empty string")
	}
}

func TestDerive_MalformedKeyDisables(t *testing.T) {
	resetDerivation()
	t.Setenv("BUILDER_SECRETS_KEY", "not-hex-and-too-short")
	initSecretDerivation()
	if secretDerivationEnabled() {
		t.Fatal("derivation should be disabled with a malformed key")
	}
}
