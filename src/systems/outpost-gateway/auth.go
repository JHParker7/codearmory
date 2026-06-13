package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// outpostInternalKey is the shared HMAC secret protecting the internal
// command/event plane between control-plane services and the gateway. The same
// scheme is implemented by the chaos/argo consumers (see their gateway.go).
var outpostInternalKey = secret("OUTPOST_INTERNAL_KEY")

// randToken returns a URL-safe random secret with n bytes of entropy.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable; surface loudly.
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// mintEnrollmentToken returns the opaque token handed to the user and its bcrypt
// hash stored on the outpost row. The token embeds the outpost id so register
// needs no lookup-by-token: "<outpost_id>.<secret>".
func mintEnrollmentToken(outpostID string) (token, hash string, err error) {
	secretPart := randToken(24)
	token = outpostID + "." + secretPart
	h, err := bcrypt.GenerateFromPassword([]byte(secretPart), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	return token, string(h), nil
}

// splitEnrollmentToken parses "<outpost_id>.<secret>".
func splitEnrollmentToken(token string) (outpostID, secretPart string, ok bool) {
	i := strings.IndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return "", "", false
	}
	return token[:i], token[i+1:], true
}

// mintOutpostKey returns the long-lived outpost key and its bcrypt hash.
func mintOutpostKey() (key, hash string, err error) {
	key = randToken(32)
	h, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	return key, string(h), nil
}

// authenticateOutpost verifies an outpost-facing request. The outpost sends its
// id in X-Outpost-ID and the long-lived key as a bearer token; the gateway
// bcrypt-compares against the stored hash. Returns the outpost on success, else
// writes a 401 and returns ok=false.
func authenticateOutpost(ctx context.Context, w http.ResponseWriter, r *http.Request) (Outpost, bool) {
	id := r.Header.Get("X-Outpost-ID")
	key := bearerToken(r)
	if id == "" || key == "" {
		http.Error(w, "missing outpost credentials", http.StatusUnauthorized)
		return Outpost{}, false
	}
	o, err := getOutpost(ctx, id)
	if err != nil || o.KeyHash == "" || o.Status == OutpostPending {
		http.Error(w, "unknown or unenrolled outpost", http.StatusUnauthorized)
		return Outpost{}, false
	}
	if bcrypt.CompareHashAndPassword([]byte(o.KeyHash), []byte(key)) != nil {
		http.Error(w, "invalid outpost key", http.StatusUnauthorized)
		return Outpost{}, false
	}
	return o, true
}

// checkBcrypt reports whether plaintext matches the bcrypt hash.
func checkBcrypt(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// signInternal builds the HMAC token authenticating an internal request. It
// signs the domain ("command"/"event"), a unix timestamp the verifier checks
// against a 30s window, and the FULL request body — so tampering with any field
// (the payload, org_id, user_id, …) invalidates the signature. The domain binds
// the token to one plane so a command token cannot be replayed as an event.
func signInternal(domain string, body []byte) (token, timestamp string) {
	return signInternalAt(domain, body, time.Now().Unix())
}

func signInternalAt(domain string, body []byte, unixTime int64) (token, timestamp string) {
	ts := strconv.FormatInt(unixTime, 10)
	return internalMAC(domain, ts, body), ts
}

// verifyInternal validates an inbound HMAC token against the exact bytes received.
func verifyInternal(domain string, body []byte, token, timestamp string) bool {
	if outpostInternalKey == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Now().Unix() - ts; d > 30 || d < -30 {
		return false
	}
	return hmac.Equal([]byte(token), []byte(internalMAC(domain, timestamp, body)))
}

// internalMAC computes hex(HMAC-SHA256(key, "domain:timestamp:" || body)).
func internalMAC(domain, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(outpostInternalKey))
	fmt.Fprintf(mac, "%s:%s:", domain, timestamp)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
