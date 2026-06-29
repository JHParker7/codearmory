package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// encKey is the 32-byte AES-256 key used to encrypt backend credential material
// at rest. It is derived (SHA-256) from GIT_ENCRYPTION_KEY so any passphrase
// length works. The service refuses to start without it — credentials are never
// stored in plaintext.
var encKey []byte

// initEncryption derives encKey from GIT_ENCRYPTION_KEY. Returns an error when the
// key is unset so main() can refuse to start.
func initEncryption() error {
	raw := secret("GIT_ENCRYPTION_KEY")
	if raw == "" {
		return errors.New("GIT_ENCRYPTION_KEY is required (credentials are encrypted at rest)")
	}
	sum := sha256.Sum256([]byte(raw))
	encKey = sum[:]
	return nil
}

// sealAuth marshals an authConfig and returns nonce||ciphertext (AES-256-GCM).
func sealAuth(a authConfig) ([]byte, error) {
	plain, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("marshal auth: %w", err)
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
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// openAuth reverses sealAuth.
func openAuth(ct []byte) (authConfig, error) {
	var a authConfig
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return a, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return a, err
	}
	if len(ct) < gcm.NonceSize() {
		return a, errors.New("ciphertext too short")
	}
	nonce, body := ct[:gcm.NonceSize()], ct[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return a, fmt.Errorf("decrypt auth: %w", err)
	}
	if err := json.Unmarshal(plain, &a); err != nil {
		return a, fmt.Errorf("unmarshal auth: %w", err)
	}
	return a, nil
}
