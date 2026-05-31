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

var (
	secretsEncMu   sync.RWMutex
	secretsAEAD    cipher.AEAD
	secretsEnabled bool
)

// initSecretsEncryption reads GATEKEEPER_SECRETS_KEY (64 hex chars = 32 bytes),
// initialises an AES-256-GCM cipher, and marks secrets as enabled.
// If the env var is absent, secrets endpoints return 503.
func initSecretsEncryption() {
	raw := strings.TrimSpace(secret("GATEKEEPER_SECRETS_KEY"))
	if raw == "" {
		slog.Warn("GATEKEEPER_SECRETS_KEY not set — secrets endpoints will return 503")
		return
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		slog.Error("GATEKEEPER_SECRETS_KEY must be 64 hex characters (32 bytes)", "error", err)
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
	secretsEncMu.Lock()
	secretsAEAD = aead
	secretsEnabled = true
	secretsEncMu.Unlock()
	slog.Info("secrets encryption initialised")
}

// encryptSecret encrypts plaintext with AES-256-GCM. The nonce is prepended.
func encryptSecret(plaintext string) ([]byte, error) {
	secretsEncMu.RLock()
	aead := secretsAEAD
	secretsEncMu.RUnlock()
	if aead == nil {
		return nil, errors.New("secrets encryption not initialised")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	ct := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return ct, nil
}

// decryptSecret reverses encryptSecret.
func decryptSecret(ciphertext []byte) (string, error) {
	secretsEncMu.RLock()
	aead := secretsAEAD
	secretsEncMu.RUnlock()
	if aead == nil {
		return "", errors.New("secrets encryption not initialised")
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(pt), nil
}
