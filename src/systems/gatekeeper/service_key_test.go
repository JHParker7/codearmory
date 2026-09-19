package main

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// A service key is 256 bits from crypto/rand, so it needs no key-stretching —
// and stretching it cost a CPU core. Measured on the local plane: ~4 requests a
// second against ~250ms of bcrypt each held gatekeeper at 0.99 cores while the
// only traffic was a status window polling.
func TestServiceKeyRoundTrips(t *testing.T) {
	const key = "b8f1c0de9a7e4f2b8c1d0e9f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"
	stored := hashServiceKey(key)
	if !strings.HasPrefix(stored, serviceKeyScheme) {
		t.Fatalf("stored hash %q does not name its scheme; a migration cannot tell the formats apart", stored)
	}
	ok, upgrade := verifyServiceKey(stored, key)
	if !ok {
		t.Error("a key did not verify against its own hash")
	}
	if upgrade {
		t.Error("a hash already in the new format was reported as needing an upgrade")
	}
	if ok, _ := verifyServiceKey(stored, key+"x"); ok {
		t.Error("a wrong key verified")
	}
	if ok, _ := verifyServiceKey("", key); ok {
		t.Error("an empty stored hash verified")
	}
	if ok, _ := verifyServiceKey(stored, ""); ok {
		t.Error("an empty key verified")
	}
}

// EVERY EXISTING DEPLOYMENT HAS BCRYPT HASHES. Rejecting them would lock every
// service out of the platform at the moment this is deployed, so they must still
// verify — and say that they should be rewritten.
func TestLegacyBcryptHashesStillVerifyAndAskToBeUpgraded(t *testing.T) {
	const key = "a-service-key"
	legacy, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	ok, upgrade := verifyServiceKey(string(legacy), key)
	if !ok {
		t.Fatal("a legacy bcrypt hash stopped verifying; every service would be locked out")
	}
	if !upgrade {
		t.Error("a legacy hash was not flagged for upgrade; it would pay bcrypt on every request forever")
	}
	if ok, _ := verifyServiceKey(string(legacy), "wrong"); ok {
		t.Error("a wrong key verified against a legacy hash")
	}
}

// A hash that names the new scheme but holds rubbish must fail closed rather
// than panic or, worse, match.
func TestMalformedNewHashFailsClosed(t *testing.T) {
	if ok, _ := verifyServiceKey(serviceKeyScheme+"not-hex", "anything"); ok {
		t.Error("a malformed hash verified")
	}
}
