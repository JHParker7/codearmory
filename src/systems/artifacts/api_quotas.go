package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// The quota admin surface, shaped like forge's concurrency limits: a service-wide
// default from config, plus per-scope overrides an admin can set, read and clear.
//
// These endpoints are admin-only by RBAC — the registry manifest grants
// setArtifactQuota/deleteArtifactQuota to no one by default, so only a role an admin
// explicitly hands out can call them. A user reads their OWN allowance through
// GET /usage instead, which needs no admin right.
//
// Every resource below lives in the "codearmory/" PLATFORM namespace, which is what
// makes the admin-only property hold. Gatekeeper prefixes the CALLER's username to an
// unscoped resource (scopeResource), so the natural-looking "artifacts/quotas/<id>"
// is evaluated as "<caller>/artifacts/quotas/<id>" — caller-prefixed on the grant side
// too, so it matches by SYMMETRY rather than by ownership. A user holding
// setArtifactQuota over their own namespace would then pass the check for ANY
// scope_id, i.e. edit everyone's quota. Naming the platform namespace explicitly
// removes the caller from the resource entirely, so only a grant that genuinely names
// "codearmory/..." can satisfy it — the same shape forge uses for its
// concurrency-limits and containers for its registries.
const resQuotas = "codearmory/artifacts/quotas"

// resQuotaOf is the per-scope resource. The scope is part of it: a user-scoped and an
// org-scoped quota for the same id are different objects, and collapsing them to the
// bare id (as the resource once did) would let a grant over one authorize the other.
func resQuotaOf(scope, scopeID string) string { return resQuotas + "/" + scope + "/" + scopeID }

type quotaRequest struct {
	// MaxBytes is the cap for this scope. Exactly one of MaxBytes/MaxMB is required;
	// MaxMB exists because an admin thinks in megabytes and 5368709120 is a typo
	// waiting to happen.
	MaxBytes *int64 `json:"max_bytes,omitempty"`
	MaxMB    *int64 `json:"max_mb,omitempty"`
}

// quotaView resolves a scope's effective allowance and current usage.
func quotaView(ctx contextT, userID string) (QuotaView, error) {
	maxBytes, isDefault, setBy, err := effectiveQuota(ctx, userID)
	if err != nil {
		return QuotaView{}, err
	}
	used, count, err := usage(ctx, userID)
	if err != nil {
		return QuotaView{}, err
	}
	return QuotaView{
		Scope: ScopeUser, ScopeID: userID, MaxBytes: maxBytes, Default: isDefault,
		UsedBytes: used, Artifacts: count, SetBy: setBy,
	}, nil
}

// handleListQuotas returns every override, plus the service default, so an admin can
// see what has been configured without probing user by user.
func handleListQuotas(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listArtifactQuota", resQuotas); !ok {
		return
	}
	rows, err := listQuotas(ctx)
	if err != nil {
		http.Error(w, "failed to list quotas", http.StatusInternalServerError)
		return
	}
	views := make([]QuotaView, 0, len(rows))
	for _, q := range rows {
		used, count, err := usage(ctx, q.ScopeID)
		if err != nil {
			http.Error(w, "failed to resolve usage", http.StatusInternalServerError)
			return
		}
		views = append(views, QuotaView{
			Scope: q.Scope, ScopeID: q.ScopeID, MaxBytes: q.MaxBytes,
			Default: false, UsedBytes: used, Artifacts: count, SetBy: q.SetBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default_max_bytes": defaultQuotaBytes(),
		"overrides":         views,
	})
}

// handleGetQuota returns one scope's effective allowance — override or default.
func handleGetQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, scopeID := r.PathValue("scope"), r.PathValue("scope_id")
	// Scope is validated BEFORE it is interpolated into the RBAC resource. Conductor
	// does not constrain this path param, so an unchecked value would become part of
	// the string the permission check is evaluated against.
	if scope != ScopeUser {
		http.Error(w, "only the user scope is supported", http.StatusBadRequest)
		return
	}
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getArtifactQuota", resQuotaOf(scope, scopeID)); !ok {
		return
	}
	view, err := quotaView(ctx, scopeID)
	if err != nil {
		http.Error(w, "failed to resolve quota", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleSetQuota sets a scope's override.
func handleSetQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, scopeID := r.PathValue("scope"), r.PathValue("scope_id")
	if scope != ScopeUser {
		http.Error(w, "only the user scope is supported", http.StatusBadRequest)
		return
	}
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "setArtifactQuota", resQuotaOf(scope, scopeID))
	if !ok {
		return
	}
	var req quotaRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	max, msg := resolveMax(req)
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := setQuota(ctx, Quota{Scope: scope, ScopeID: scopeID, MaxBytes: max, SetBy: userID}); err != nil {
		slog.ErrorContext(ctx, "set artifact quota", "scope_id", scopeID, "error", err)
		http.Error(w, "failed to set quota", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "artifact quota set", "scope", scope, "scope_id", scopeID, "max_bytes", max, "set_by", userID)
	view, err := quotaView(ctx, scopeID)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleDeleteQuota clears an override, returning the scope to the service default.
func handleDeleteQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, scopeID := r.PathValue("scope"), r.PathValue("scope_id")
	// The same scope guard the get/set handlers apply. It was missing here, so a
	// delete could name a scope the other two reject.
	if scope != ScopeUser {
		http.Error(w, "only the user scope is supported", http.StatusBadRequest)
		return
	}
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteArtifactQuota", resQuotaOf(scope, scopeID)); !ok {
		return
	}
	n, err := deleteQuota(ctx, scope, scopeID)
	if err != nil {
		http.Error(w, "failed to delete quota", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "no override set for that scope", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveMax turns a request into a byte cap, accepting either unit but not both.
// Returns a user-facing message when invalid.
func resolveMax(req quotaRequest) (int64, string) {
	switch {
	case req.MaxBytes != nil && req.MaxMB != nil:
		return 0, "set exactly one of max_bytes or max_mb"
	case req.MaxBytes != nil:
		if *req.MaxBytes < 0 {
			return 0, "max_bytes must not be negative"
		}
		return *req.MaxBytes, ""
	case req.MaxMB != nil:
		if *req.MaxMB < 0 {
			return 0, "max_mb must not be negative"
		}
		return *req.MaxMB * 1024 * 1024, ""
	default:
		return 0, "set exactly one of max_bytes or max_mb"
	}
}
