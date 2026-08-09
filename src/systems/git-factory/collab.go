package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Collaborators.
//
// The authorization itself lives in gatekeeper: a per-repo permission on the
// owner-first resource "<namespace>/<service>/repos/<id>", held by a role the owner
// owns and assigns. This file is the repo-shaped front door for that — "share this
// repo with bob" instead of "create a permission, wrap it in a role, assign it".
//
// git_factory deliberately holds NO access-control state of its own. It records which
// gatekeeper role backs which (repo, level) so it can find it again, and nothing more:
// duplicating the grants here would create a second source of truth that could
// disagree with the one actually enforced.

// RepoShare maps a repo and access level to the gatekeeper role backing it. Not an
// ACL — the grant lives in gatekeeper; this is a pointer so we can find it again.
type RepoShare struct {
	RepoID    string    `gorm:"primaryKey" json:"repo_id"`
	Level     string    `gorm:"primaryKey" json:"level"` // "read" | "write"
	RoleID    string    `json:"role_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Access levels and the actions each carries. read is everything needed to browse,
// clone and take part in review; write adds pushing and landing changes. Managing
// collaborators is NOT included at any level — re-sharing someone else's repo stays
// with the owner, as do webhooks and transfer.
//
// REVIEWING IS A READ-LEVEL ACTION, deliberately. The merge gate refuses a
// self-approval, so if reviewPull needed write access then a repo requiring one
// approval could only be approved by someone who could already merge it unreviewed —
// and a two-person repo where the other person has read access would be permanently
// unmergeable. Being able to see a change is what qualifies you to comment on it.
//
// Keep this in step with the actions a repo's routes declare: an action added to the
// manifest but missing here is grantable to the OWNER and to nobody else, which shows
// up as a collaborator getting 404s on a repo they can otherwise use.
var shareActions = map[string][]string{
	"read": {
		"getRepo", "listCommit", "getReadme", "listBranch", "listTag",
		"getTree", "getBlob", "getArchive", "readRepo",
		"listPull", "getPull", "createPull",
		// Review and discussion: see above.
		"reviewPull", "commentPull",
		// Check results are part of reading a pull request — a reviewer has to be able
		// to see whether CI passed.
		"listStatus",
		// Forking needs only the right to read the source; the copy lands in the
		// forker's own namespace under their own createRepo grant.
		"forkRepo",
	},
	"write": {
		"getRepo", "listCommit", "getReadme", "listBranch", "listTag",
		"getTree", "getBlob", "getArchive", "readRepo", "writeRepo", "updateRepo",
		"setDefaultBranch",
		"listPull", "getPull", "createPull", "mergePull", "updatePull",
		"reviewPull", "commentPull", "listStatus", "forkRepo",
		// Writers land releases and report build results.
		"createTag", "deleteTag", "setStatus",
	},
}

func validShareLevel(l string) bool { _, ok := shareActions[l]; return ok }

// gatekeeperCall performs a request against gatekeeper AS THE CALLER, forwarding their
// bearer token. Acting as the user rather than as the service is the point: gatekeeper
// then applies its own confinement and attenuation rules, so git_factory cannot be
// tricked into granting something the caller could not grant themselves.
func gatekeeperCall(ctx context.Context, bearer, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, gatekeeperURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("gatekeeper %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// shareRoleFor returns the gatekeeper role backing (repo, level), creating it on first
// use. The role is created by the CALLER, so they end up owning it — which is what
// lets them assign and revoke it later without an admin.
func shareRoleFor(ctx context.Context, bearer string, re Repo, level string) (string, error) {
	var share RepoShare
	err := connectRead().WithContext(ctx).Where("repo_id = ? AND level = ?", re.ID, level).First(&share).Error
	if err == nil && share.RoleID != "" {
		return share.RoleID, nil
	}

	perms := make([]map[string]string, 0, len(shareActions[level]))
	for _, action := range shareActions[level] {
		perms = append(perms, map[string]string{
			"service":  serviceName,
			"action":   action,
			"resource": resRepoOf(re),
		})
	}
	var created struct {
		RoleID string `json:"role_id"`
	}
	body := map[string]any{
		"name":        "repo:" + re.Namespace + "/" + re.Name + ":" + level,
		"permissions": perms,
	}
	if err := gatekeeperCall(ctx, bearer, http.MethodPost, "/roles/namespace", body, &created); err != nil {
		return "", err
	}
	share = RepoShare{RepoID: re.ID, Level: level, RoleID: created.RoleID, CreatedAt: time.Now().UTC()}
	if err := connect().WithContext(ctx).Create(&share).Error; err != nil {
		// The role exists in gatekeeper but we failed to remember it. Report rather
		// than orphan it silently — a retry would create a second identical role.
		slog.ErrorContext(ctx, "share: role created but not recorded", "Repo_id", re.ID, "role_id", created.RoleID, "error", err)
		return "", err
	}
	return created.RoleID, nil
}

// handleAddCollaborator shares a repo with a user at read or write level.
func handleAddCollaborator(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleAddCollaborator")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "shareRepo")
	if !ok {
		return
	}
	var req struct {
		User  string `json:"user"`  // user id
		Level string `json:"level"` // read | write
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Level == "" {
		req.Level = "read"
	}
	if !validShareLevel(req.Level) {
		http.Error(w, "level must be read or write", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.User) == "" {
		http.Error(w, "user is required", http.StatusBadRequest)
		return
	}

	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	roleID, err := shareRoleFor(ctx, bearer, re, req.Level)
	if err != nil {
		slog.ErrorContext(ctx, "share: ensure role", "Repo_id", re.ID, "error", err)
		http.Error(w, "could not create the share: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := gatekeeperCall(ctx, bearer, http.MethodPut, "/roles/"+roleID+"/members/"+req.User, nil, nil); err != nil {
		slog.ErrorContext(ctx, "share: assign", "Repo_id", re.ID, "error", err)
		http.Error(w, "could not assign the share: "+err.Error(), http.StatusBadGateway)
		return
	}
	slog.InfoContext(ctx, "repo shared", "Repo_id", re.ID, "user_id", req.User, "level", req.Level)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{"repo_id": re.ID, "user": req.User, "level": req.Level})
}

// handleRemoveCollaborator revokes a user's access at every level they hold.
func handleRemoveCollaborator(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleRemoveCollaborator")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "shareRepo")
	if !ok {
		return
	}
	target := r.PathValue("user")
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")

	var shares []RepoShare
	if err := connectRead().WithContext(ctx).Where("repo_id = ?", re.ID).Find(&shares).Error; err != nil {
		slog.ErrorContext(ctx, "share: load", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Revoke at every level: a user granted read and later write holds two roles, and
	// removing "the" collaborator must not leave one of them behind.
	for _, s := range shares {
		if err := gatekeeperCall(ctx, bearer, http.MethodDelete, "/roles/"+s.RoleID+"/members/"+target, nil, nil); err != nil {
			slog.WarnContext(ctx, "share: revoke failed", "Repo_id", re.ID, "role_id", s.RoleID, "error", err)
		}
	}
	slog.InfoContext(ctx, "repo unshared", "Repo_id", re.ID, "user_id", target)
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleListCollaborators reports who the repo is shared with, per level. The
// membership lists come from gatekeeper, which is the only place they exist.
func handleListCollaborators(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListCollaborators")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "shareRepo")
	if !ok {
		return
	}
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")

	var shares []RepoShare
	if err := connectRead().WithContext(ctx).Where("repo_id = ?", re.ID).Find(&shares).Error; err != nil {
		slog.ErrorContext(ctx, "share: load", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	out := []map[string]string{}
	for _, s := range shares {
		var members []struct {
			UserID string `json:"user_id"`
		}
		if err := gatekeeperCall(ctx, bearer, http.MethodGet, "/roles/"+s.RoleID+"/members", nil, &members); err != nil {
			slog.WarnContext(ctx, "share: list members failed", "role_id", s.RoleID, "error", err)
			continue
		}
		for _, m := range members {
			out = append(out, map[string]string{"user_id": m.UserID, "level": s.Level})
		}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{"owner": re.Owner, "collaborators": out})
}
