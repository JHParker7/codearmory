package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"
)

// TestMain initialises the no-op OTel metrics so counter calls in handlers
// don't panic during tests that exercise code paths touching the globals.
func TestMain(m *testing.M) {
	initMetrics()
	os.Exit(m.Run())
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	setTestKey(t)
	plaintext := []byte(`{"version":4,"resources":[]}`)

	ct, err := encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext must differ from plaintext")
	}

	got, err := decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestEncryptNonDeterministic(t *testing.T) {
	setTestKey(t)
	plaintext := []byte("same plaintext")

	ct1, _ := encrypt(plaintext)
	ct2, _ := encrypt(plaintext)
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two encryptions of the same plaintext must produce different ciphertext (nonce reuse)")
	}
}

func TestEncryptDecryptNoKey(t *testing.T) {
	encKey = nil

	plaintext := []byte("hello world")

	ct, err := encrypt(plaintext)
	if err != nil || !bytes.Equal(ct, plaintext) {
		t.Fatalf("no-key encrypt: expected passthrough, got err=%v ct=%q", err, ct)
	}

	pt, err := decrypt(plaintext)
	if err != nil || !bytes.Equal(pt, plaintext) {
		t.Fatalf("no-key decrypt: expected passthrough, got err=%v pt=%q", err, pt)
	}
}

func TestDecryptTruncated(t *testing.T) {
	setTestKey(t)
	if _, err := decrypt([]byte("tooshort")); err == nil {
		t.Fatal("expected error decrypting truncated ciphertext")
	}
}

func TestDecryptTampered(t *testing.T) {
	setTestKey(t)
	ct, _ := encrypt([]byte("secret state"))
	ct[len(ct)-1] ^= 0xff // flip a byte in the GCM tag
	if _, err := decrypt(ct); err == nil {
		t.Fatal("expected authentication failure for tampered ciphertext")
	}
}

func TestInitEncryptionInvalidHex(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "not-valid-hex!")
	encKey = nil
	t.Cleanup(func() { encKey = nil })

	if err := initEncryption(); err == nil {
		t.Fatal("expected error for invalid hex key")
	}
}

func TestInitEncryptionWrongLength(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", hex.EncodeToString([]byte("only16bytes!!!!!")))
	encKey = nil
	t.Cleanup(func() { encKey = nil })

	if err := initEncryption(); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}

func TestInitEncryptionValid(t *testing.T) {
	key := make([]byte, 32)
	t.Setenv("ENCRYPTION_KEY", hex.EncodeToString(key))
	encKey = nil
	t.Cleanup(func() { encKey = nil })

	if err := initEncryption(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if encKey == nil {
		t.Fatal("encKey must be set after successful init")
	}
}

// setTestKey loads a deterministic 32-byte key and restores nil on cleanup.
func setTestKey(t *testing.T) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	encKey = key
	t.Cleanup(func() { encKey = nil })
}
