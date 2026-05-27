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
)

var encKey []byte

// initEncryption loads ENCRYPTION_KEY (or ENCRYPTION_KEY_FILE) from the
// environment. The key must be a 64-character hex string (32 bytes / AES-256).
// If neither variable is set, state is stored and retrieved as plaintext.
func initEncryption() error {
	raw := secret("ENCRYPTION_KEY")
	if raw == "" {
		slog.Error("ENCRYPTION_KEY / ENCRYPTION_KEY_FILE is not set; refusing to start without encryption to protect state secrets")
		os.Exit(1)
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return fmt.Errorf("ENCRYPTION_KEY: invalid hex: %w", err)
	}
	if len(key) != 32 {
		return fmt.Errorf("ENCRYPTION_KEY: must be 32 bytes (64 hex chars), got %d", len(key))
	}
	encKey = key
	return nil
}

// encrypt seals plaintext with AES-256-GCM. The returned ciphertext is
// [12-byte nonce || GCM ciphertext+tag]. Returns plaintext unchanged when no
// key is configured.
func encrypt(plaintext []byte) ([]byte, error) {
	if encKey == nil {
		return plaintext, nil
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt opens an AES-256-GCM ciphertext produced by encrypt. Returns the
// ciphertext unchanged when no key is configured.
func decrypt(ciphertext []byte) ([]byte, error) {
	if encKey == nil {
		return ciphertext, nil
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}
