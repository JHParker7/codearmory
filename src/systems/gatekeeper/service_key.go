package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Service-key hashing.
//
// A SERVICE KEY IS NOT A PASSWORD, and hashing it like one was costing a CPU
// core. bcrypt is deliberately slow — that is its whole purpose, and the cost
// factor is chosen so that brute-forcing a stolen password database is
// uneconomic. Passwords need that because people choose them: they are short,
// low-entropy, and reused, so an attacker who steals the hashes can guess.
//
// A service key is none of those things. It is 32 bytes from crypto/rand, hex
// encoded — 256 bits of entropy. There is no dictionary to try and no guessing
// strategy better than enumerating the whole space, so key-stretching buys
// exactly nothing against it. What it does buy is a quarter of a second of CPU
// on EVERY authenticated request.
//
// Measured on the local plane, 2026-08-15: gatekeeper held a full core steady
// while the only traffic was a status window polling every two seconds. ~4
// requests per second times ~250ms of bcrypt each is ~1.0 core, and that matched
// the measurement to two decimal places. The cost is paid per request and scales
// with call volume, so the busier the platform gets the worse it becomes — for a
// check that protects a value nobody can guess.
//
// SHA-256 over a high-entropy secret, compared in constant time, is the right
// primitive here and is roughly ten thousand times cheaper.
//
// WHAT THIS DELIBERATELY DOES NOT CHANGE: user passwords. Those are exactly the
// case bcrypt exists for and they stay on it — see api_users.go and api_oauth.go.

// serviceKeyScheme prefixes the new format so a stored hash names its own
// algorithm. Without a marker the only way to tell the formats apart is to guess
// from their shape, which is how a migration turns into a silent lockout.
const serviceKeyScheme = "sha256:"

// hashServiceKey renders a key for storage.
func hashServiceKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return serviceKeyScheme + hex.EncodeToString(sum[:])
}

// verifyServiceKey reports whether key matches stored, and whether the stored
// hash is in the old format and should be rewritten.
//
// LEGACY HASHES STILL VERIFY. Every existing deployment has bcrypt hashes in
// hashed_key and hashed_bootstrap_key, and a change that rejected them would
// lock every service out of the platform at the moment it was deployed. So the
// old format is accepted, and `upgrade` tells the caller to store the cheap hash
// on the way past — the expensive comparison then happens once per key rather
// than once per request.
func verifyServiceKey(stored, key string) (ok bool, upgrade bool) {
	if stored == "" || key == "" {
		return false, false
	}
	if rest, found := strings.CutPrefix(stored, serviceKeyScheme); found {
		sum := sha256.Sum256([]byte(key))
		want, err := hex.DecodeString(rest)
		if err != nil {
			return false, false
		}
		// Constant time: a byte-by-byte comparison leaks how much of the hash
		// matched through its timing, which over enough attempts recovers it.
		return subtle.ConstantTimeCompare(sum[:], want) == 1, false
	}
	// Anything else is a bcrypt hash from before this change.
	if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(key)); err != nil {
		return false, false
	}
	return true, true
}
