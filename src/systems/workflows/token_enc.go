package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

var tokenEncKey []byte

// initTokenEncryption loads WORKFLOWS_TOKEN_KEY (hex-encoded 32-byte AES key).
// When the env var is absent the service logs a warning and stores tokens in
// plaintext — acceptable for local dev but must not be used in production.
func initTokenEncryption() {
	raw := secret("WORKFLOWS_TOKEN_KEY")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "WARNING: WORKFLOWS_TOKEN_KEY is not set; run tokens will be stored unencrypted in the database")
		return
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		fmt.Fprintf(os.Stderr, "WORKFLOWS_TOKEN_KEY must be a hex-encoded 32-byte (64-character) key, got %d bytes\n", len(key))
		os.Exit(1)
	}
	tokenEncKey = key
}

// encryptToken encrypts a plaintext JWT with AES-256-GCM and returns a
// base64-encoded ciphertext. Returns plaintext unchanged when no key is set.
func encryptToken(plaintext string) (string, error) {
	if len(tokenEncKey) == 0 {
		return plaintext, nil
	}
	block, err := aes.NewCipher(tokenEncKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptToken reverses encryptToken. Returns the value unchanged when no key
// is set (plaintext passthrough for dev environments without WORKFLOWS_TOKEN_KEY).
func decryptToken(ciphertext string) (string, error) {
	if len(tokenEncKey) == 0 {
		return ciphertext, nil
	}
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(tokenEncKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
