package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Builder encrypts admin-supplied per-service DB URLs at rest with AES-256-GCM,
// keyed by its OWN BUILDER_SECRETS_KEY (separate from gatekeeper's key, so a
// builder compromise can't decrypt gatekeeper's org secrets and vice-versa). Every
// value is bound to its service name as additional authenticated data, so a stored
// URL can't be swapped between services even by a database-write attacker.
//
// This is the local baseline; a Vault Transit backend (key never leaves Vault,
// built-in rotation + audit) is the planned hardening and would slot in behind the
// same encrypt/decrypt calls.
var (
	builderSecretsMu   sync.RWMutex
	builderSecretsAEAD cipher.AEAD
)

// initSecretsEncryption reads BUILDER_SECRETS_KEY (64 hex chars = 32 bytes) and
// initialises the AES-256-GCM cipher. Absent key => provisioning of DB-backed
// services is disabled (the admin cannot store a DB URL), but the rest of builder
// runs normally.
func initSecretsEncryption() {
	raw := strings.TrimSpace(secret("BUILDER_SECRETS_KEY"))
	if raw == "" {
		slog.Warn("BUILDER_SECRETS_KEY not set — per-service DB URL storage disabled")
		return
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		slog.Error("BUILDER_SECRETS_KEY must be 64 hex characters (32 bytes)")
		os.Exit(1)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		slog.Error("failed to create AES cipher", "error", err)
		os.Exit(1)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		slog.Error("failed to create GCM", "error", err)
		os.Exit(1)
	}
	builderSecretsMu.Lock()
	builderSecretsAEAD = aead
	builderSecretsMu.Unlock()
	slog.Info("secrets encryption initialised")
}

func secretsEncryptionEnabled() bool {
	builderSecretsMu.RLock()
	defer builderSecretsMu.RUnlock()
	return builderSecretsAEAD != nil
}

// encryptSecret encrypts plaintext, binding it to aad (the service name). The
// nonce is prepended to the ciphertext.
func encryptSecret(plaintext, aad string) ([]byte, error) {
	builderSecretsMu.RLock()
	aead := builderSecretsAEAD
	builderSecretsMu.RUnlock()
	if aead == nil {
		return nil, errors.New("secrets encryption not initialised")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad)), nil
}

// decryptSecret reverses encryptSecret; aad must match what was used to encrypt.
func decryptSecret(ciphertext []byte, aad string) (string, error) {
	builderSecretsMu.RLock()
	aead := builderSecretsAEAD
	builderSecretsMu.RUnlock()
	if aead == nil {
		return "", errors.New("secrets encryption not initialised")
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	pt, err := aead.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(pt), nil
}
