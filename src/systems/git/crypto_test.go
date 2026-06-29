package main

import "testing"

func TestSealOpenRoundTrip(t *testing.T) {
	in := authConfig{Mode: modePAT, Token: "ghp_secret", Username: "octocat"}
	ct, err := sealAuth(in)
	if err != nil {
		t.Fatalf("sealAuth: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("empty ciphertext")
	}
	out, err := openAuth(ct)
	if err != nil {
		t.Fatalf("openAuth: %v", err)
	}
	if out.Token != in.Token || out.Username != in.Username || out.Mode != in.Mode {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestOpenTamperFails(t *testing.T) {
	ct, err := sealAuth(authConfig{Mode: modeBasic, Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("sealAuth: %v", err)
	}
	ct[len(ct)-1] ^= 0xFF // flip a ciphertext byte
	if _, err := openAuth(ct); err == nil {
		t.Fatal("expected decryption failure on tampered ciphertext")
	}
}

func TestOpenTooShort(t *testing.T) {
	if _, err := openAuth([]byte{0x01, 0x02}); err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}
