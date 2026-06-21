package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// builderInternalKey guards the internal effective-state endpoint that the
// gatekeeper disable-gate calls. It is a simple shared bearer secret — the data
// (which services an org has disabled) is low-sensitivity, and the gate fails
// open if this is unset, so the platform never locks up on a misconfiguration.
var builderInternalKey string

// requireInternalKey checks the shared bearer token. Returns false (and writes
// 401) when the key is unset or the token does not match.
func requireInternalKey(w http.ResponseWriter, r *http.Request) bool {
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

// handleInternalEffective returns the set of services explicitly disabled for an
// org (merging the default baseline with the org's overrides). The gatekeeper
// gate caches this per org and denies requests to any service in the set.
func handleInternalEffective(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("builder").Start(r.Context(), "handleInternalEffective")
	defer span.End()

	if !requireInternalKey(w, r) {
		span.SetStatus(codes.Ok, "")
		return
	}
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		orgID = defaultOrgID
	}

	disabled, err := effectiveDisabledSet(ctx, orgID)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to resolve effective state", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(effectiveResponse{OrgID: orgID, Disabled: disabled}) //nolint:errcheck
}
