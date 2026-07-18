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
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listArtifactQuota", "artifacts/quotas"); !ok {
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
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getArtifactQuota", "artifacts/quotas/"+scopeID); !ok {
		return
	}
	if scope != ScopeUser {
		http.Error(w, "only the user scope is supported", http.StatusBadRequest)
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
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "setArtifactQuota", "artifacts/quotas/"+scopeID)
	if !ok {
		return
	}
	if scope != ScopeUser {
		http.Error(w, "only the user scope is supported", http.StatusBadRequest)
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
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteArtifactQuota", "artifacts/quotas/"+scopeID); !ok {
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
