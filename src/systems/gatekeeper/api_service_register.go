package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Runtime service-account registration lets the builder service bring a non-core
// service online with no Helm change: builder generates a key, registers the
// identity here, and bakes the same key into the service's Secret. Gatekeeper's
// permission check falls back to the live ServiceAccount table (isServicePermitted),
// so a service registered this way authenticates without being in GATEKEEPER_SERVICES.
//
// Guarded by BUILDER_INTERNAL_KEY — the same shared secret gatekeeper already uses
// for the org-service disable-gate, so no new secret is introduced. Inert (401)
// when that key is unset.

// requireBuilderInternal authorises a builder→gatekeeper internal call via the
// shared BUILDER_INTERNAL_KEY bearer token.
func requireBuilderInternal(w http.ResponseWriter, r *http.Request) bool {
	if builderInternalKey == "" {
		http.Error(w, "internal endpoint disabled", http.StatusUnauthorized)
		return false
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(builderInternalKey)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// handleRegisterServiceAccount creates (or refreshes the bootstrap key of) a
// service account so a freshly deployed service can authenticate. Idempotent: the
// underlying upsert refreshes the bootstrap key for an existing account, which is
// exactly what a redeploy with a new key needs.
func handleRegisterServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !requireBuilderInternal(w, r) {
		return
	}
	var req struct {
		ServiceName string `json:"service_name"`
		Key         string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.ServiceName = strings.TrimSpace(req.ServiceName)
	if req.ServiceName == "" || req.Key == "" {
		http.Error(w, "service_name and key are required", http.StatusBadRequest)
		return
	}
	// Never let a runtime caller re-key a statically-configured core service: that
	// would overwrite its bootstrap key and let the caller authenticate AS that
	// service. Builder only ever provisions non-core services.
	if isCoreServiceName(req.ServiceName) {
		slog.WarnContext(r.Context(), "service account register: refused to overwrite core service", "service", req.ServiceName)
		http.Error(w, "cannot register a core service name", http.StatusForbidden)
		return
	}
	// Re-registration with an unchanged key is the COMMON case, not the exception:
	// builder's reconciler calls this for every managed service on every tick. Writing
	// a row and an audit entry each time turns the audit trail into a record of
	// polling — the register entries drown out everything a human is looking for. So
	// an unchanged, still-active account is a no-op: same 204, nothing written.
	// Inactive is deliberately excluded, since re-registering is how a torn-down
	// service comes back and that genuinely is a change.
	if existing, err := (ServiceAccount{ServiceName: req.ServiceName}).Get(r.Context()); err == nil {
		acct := existing.(ServiceAccount)
		if acct.Active && acct.HashedBootstrapKey != "" &&
			bcrypt.CompareHashAndPassword([]byte(acct.HashedBootstrapKey), []byte(req.Key)) == nil {
			slog.DebugContext(r.Context(), "service account already registered with this key", "service", req.ServiceName)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Key), 12)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	upsertServiceAccountDB(r.Context(), req.ServiceName, string(hash))
	slog.InfoContext(r.Context(), "service account registered at runtime", "service", req.ServiceName)
	writeAudit(r.Context(), "builder", "service", "service_account.register", req.ServiceName, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleDeregisterServiceAccount deactivates a service account when its service is
// torn down. Best-effort: a missing account is treated as already gone.
func handleDeregisterServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !requireBuilderInternal(w, r) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	row, err := (ServiceAccount{ServiceName: name}).Get(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := row.(ServiceAccount).Remove(r.Context()); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(r.Context(), "service account deregistered", "service", name)
	writeAudit(r.Context(), "builder", "service", "service_account.deregister", name, "")
	w.WriteHeader(http.StatusNoContent)
}
