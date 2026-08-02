package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// GitOps workflow-sync settings on a pinned repo, plus the internal endpoint the
// workflows service consults before syncing `.armory/workflows/*` from a push.
//
// The branch allowlist is the trust boundary: configs are synced ONLY from branches a
// user explicitly listed (default "main"), so a push to a feature branch — or a PR
// from someone without merge rights — can never register or run a workflow. The check
// is enforced server-side (workflows re-consults this endpoint), never only in the UI.

// effectiveSyncBranches is the allowlist a sync-enabled repo uses: the branches the
// user listed, or "main" when they enabled sync without naming any.
func effectiveSyncBranches(r GitRepo) []string {
	if len(r.WorkflowSyncBranches) > 0 {
		return r.WorkflowSyncBranches
	}
	return []string{"main"}
}

// updateRepoRequest is the body of PUT /repos/{id}. BOTH fields are pointers so an
// omitted one leaves the current value: this is a partial update, and the difference
// matters — sending only branches must not turn sync off, and sending only the enabled
// flag must not wipe the allowlist (which would fall back to "main" and sync from a
// branch the user deliberately excluded). Branches are replaced wholesale when present,
// and an explicit `[]` clears the allowlist.
type updateRepoRequest struct {
	WorkflowSyncEnabled  *bool     `json:"workflow_sync_enabled"`
	WorkflowSyncBranches *[]string `json:"workflow_sync_branches"`
}

// handleUpdateRepo sets the GitOps sync settings on a pinned repo the caller owns.
func handleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleUpdateRepo")
	defer span.End()

	id := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "updateRepo", "git_connector/repos/"+id)
	if !ok {
		return
	}
	var req updateRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// Only the fields the body actually carried are written; cols is what tells the
	// store which those are, so an omitted field keeps its stored value instead of
	// being force-written back as its zero value.
	rp := GitRepo{ID: id, Owner: userID}
	var cols []string
	if req.WorkflowSyncEnabled != nil {
		rp.WorkflowSyncEnabled = *req.WorkflowSyncEnabled
		cols = append(cols, "workflow_sync_enabled")
	}
	if req.WorkflowSyncBranches != nil {
		// Non-nil even when every entry is filtered out: an explicit empty (or
		// all-blank) list is a deliberate "clear the allowlist", not an omission.
		branches := make([]string, 0, len(*req.WorkflowSyncBranches))
		for _, b := range *req.WorkflowSyncBranches {
			if b = strings.TrimSpace(b); b != "" {
				branches = append(branches, b)
			}
		}
		rp.WorkflowSyncBranches = branches
		cols = append(cols, "workflow_sync_branches")
	}
	stored, err := rp.UpdateSync(ctx, cols)
	if err != nil {
		if errors.Is(err, errRepoNotFound) {
			http.Error(w, "repo not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "update repo sync", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repo sync settings updated", "user_id", userID, "repo_id", id, "enabled", stored.WorkflowSyncEnabled)
	writeJSON(w, http.StatusOK, repoView{
		ID: id, Source: repoSourceManual,
		WorkflowSyncEnabled:  stored.WorkflowSyncEnabled,
		WorkflowSyncBranches: stored.WorkflowSyncBranches,
	})
}

// syncOwnerPolicy is one owner's sync policy for a repo: whom to sync as, and the
// branch allowlist the workflows service must enforce.
type syncOwnerPolicy struct {
	Owner    string   `json:"owner"`
	Branches []string `json:"branches"`
}

// handleInternalSyncConfig returns the sync policy for a repo URL: every owner that
// pinned it with sync enabled, and each one's branch allowlist. Called by the
// workflows service (X-Internal-Key auth) when a push arrives, so the trust decision
// lives here rather than in the caller. A repo with sync off returns no policy.
func handleInternalSyncConfig(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleInternalSyncConfig")
	defer span.End()

	if internalKey == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Key")), []byte(internalKey)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	repos, err := reposByURL(ctx, strings.TrimSpace(req.URL))
	if err != nil {
		slog.ErrorContext(ctx, "sync-config lookup", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	policies := make([]syncOwnerPolicy, 0, len(repos))
	for _, rp := range repos {
		if !rp.WorkflowSyncEnabled {
			continue
		}
		policies = append(policies, syncOwnerPolicy{Owner: rp.Owner, Branches: effectiveSyncBranches(rp)})
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
}
