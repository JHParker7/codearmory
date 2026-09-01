package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// Service-minted clone tokens (DESIGN-read-replicas.md §7, "runner credential").
//
// A workflow runner cloning a mirror has no platform JWT — Forge injects only a clone
// URL into an env var. So git-factory issues its OWN credential: a short-lived,
// read-only, REPO-SCOPED token that its wire auth verifies directly (no gatekeeper
// round-trip), exactly the shape git-connector's Forgejo-admin mode already mints. The
// token authorizes a clone of ONE repo and nothing else; it can never push (upload-pack
// only) and expires on its own.
//
// It is HMAC-signed with GIT_FACTORY_CLONE_TOKEN_KEY. Minting is reachable only behind
// the internal key (git-connector), so the blast radius of the surface is one service.

const cloneTokenPrefix = "cgfct1." // versioned so the scheme can change without ambiguity

// cloneTokenKey is the HMAC key. Distinct from GIT_FACTORY_INTERNAL_KEY so the
// credential a runner carries is never the key that authorizes minting. Empty disables
// verification (every token is rejected), which keeps the feature off until configured.
func cloneTokenKey() string { return secret("GIT_FACTORY_CLONE_TOKEN_KEY") }

type cloneTokenPayload struct {
	R string `json:"r"` // repo id the token is scoped to
	E int64  `json:"e"` // expiry, unix seconds
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func sign(msg string) []byte {
	m := hmac.New(sha256.New, []byte(cloneTokenKey()))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// mintCloneToken issues a read-only token for repoID valid for ttl. Returns the token
// and its expiry. An empty key yields an empty token (caller should treat that as
// "feature not configured").
func mintCloneToken(repoID string, ttl time.Duration) (token string, expiresAt time.Time) {
	if cloneTokenKey() == "" {
		return "", time.Time{}
	}
	expiresAt = time.Now().Add(ttl).UTC()
	body, _ := json.Marshal(cloneTokenPayload{R: repoID, E: expiresAt.Unix()})
	head := cloneTokenPrefix + b64(body)
	return head + "." + b64(sign(head)), expiresAt
}

// verifyCloneToken checks a presented token's prefix, signature and expiry in constant
// time and returns the repo id it authorizes. ok is false for anything that is not a
// valid, unexpired git-factory clone token — the caller then falls through to the normal
// auth path, so a JWT or PAT is unaffected.
func verifyCloneToken(token string) (repoID string, ok bool) {
	if cloneTokenKey() == "" || !strings.HasPrefix(token, cloneTokenPrefix) {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 { // "cgfct1", payload, sig
		return "", false
	}
	head := parts[0] + "." + parts[1]
	want := sign(head)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, want) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var p cloneTokenPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.R == "" {
		return "", false
	}
	if time.Now().Unix() >= p.E {
		return "", false // expired
	}
	return p.R, true
}

// authenticatedCloneURL is the repo's clone URL with the token embedded as basic-auth
// userinfo (git://git:<token>@host/ns/name.git), ready for a runner to `git clone`. The
// username is a fixed placeholder — git-factory reads the token from the password field.
func authenticatedCloneURL(re Repo, token string) string {
	u, err := url.Parse(re.HttpUrl)
	if err != nil || re.HttpUrl == "" {
		return re.HttpUrl
	}
	u.User = url.UserPassword("git", token)
	return u.String()
}
