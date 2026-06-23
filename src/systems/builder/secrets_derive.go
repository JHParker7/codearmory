package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
)

// Builder derives the stable secrets that several non-core services must AGREE on —
// the cross-service shared HMAC keys (hooks-trigger-key shared by hooks/workflows/
// tickets/chaos; outpost-internal-key by outpost-gateway/chaos/argo; gitea-internal-key
// by gitea_integration/forge) and the per-service crypto keys (blueprints encryption-key,
// workflows token-key). They are HMAC-derived from a dedicated root, so every consumer
// computes the same value with no ordering dependency between enables — there is no
// "generate once and distribute" coordination to get wrong.
//
// The root is HMAC(BUILDER_SECRETS_KEY, derivationLabel), namespaced away from the
// AES-256-GCM at-rest cipher that also uses BUILDER_SECRETS_KEY (see secrets_enc.go),
// so the two uses are cryptographically independent. Output is hex-encoded: 64 hex
// chars / 32 bytes — which is BOTH a valid opaque HMAC key AND the exact AES-256 key
// format blueprints (crypto.go) and workflows (token_enc.go) require; they reject any
// key that is not 64 hex characters, so emitting raw or base64 bytes would crash-loop
// them. A future Vault backend can replace these calls with Vault-issued material.
const derivationLabel = "shared-secret-derivation-v1"

var (
	derivationMu   sync.RWMutex
	derivationRoot []byte
)

// initSecretDerivation computes the derivation root from BUILDER_SECRETS_KEY. An absent
// or malformed key leaves derivation disabled (deriveSharedKey/derivePrivateKey return
// ""); initSecretsEncryption already os.Exits on a malformed key, so a running builder
// in practice has both enabled or neither.
func initSecretDerivation() {
	raw := strings.TrimSpace(secret("BUILDER_SECRETS_KEY"))
	if raw == "" {
		return
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		slog.Warn("BUILDER_SECRETS_KEY malformed — shared-secret derivation disabled")
		return
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(derivationLabel))
	root := mac.Sum(nil)
	derivationMu.Lock()
	derivationRoot = root
	derivationMu.Unlock()
}

func secretDerivationEnabled() bool {
	derivationMu.RLock()
	defer derivationMu.RUnlock()
	return derivationRoot != nil
}

// deriveSharedKey returns the stable key for a cross-service shared secret, keyed by
// its canonical name (e.g. "hooks-trigger-key"). The same name yields the same 64-hex
// value on every call and in every service builder provisions.
func deriveSharedKey(name string) string {
	return deriveKey("shared:" + name)
}

// derivePrivateKey returns a stable per-service crypto key (e.g. blueprints
// "encryption-key"), bound to the service so two services never collide on one.
func derivePrivateKey(service, name string) string {
	return deriveKey("service:" + service + ":" + name)
}

func deriveKey(label string) string {
	derivationMu.RLock()
	root := derivationRoot
	derivationMu.RUnlock()
	if root == nil {
		return ""
	}
	mac := hmac.New(sha256.New, root)
	mac.Write([]byte(label))
	return hex.EncodeToString(mac.Sum(nil))
}
