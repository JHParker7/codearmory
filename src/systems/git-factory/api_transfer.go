package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Repo transfer — moving a repo to a different owner or namespace.
//
// Rename was already possible; transfer was not, which meant a repo created under the
// wrong account, or belonging to someone leaving, could only be moved by cloning it into
// a new repo and losing its id — and with the id goes every grant written against
// `<ns>/codearmory_git_factory/repos/<id>`, every PR, and every clone URL in someone's
// CI.
//
// So transfer changes Owner and Namespace and NOTHING else. The id is stable, which
// keeps the bytes where they are (the path derives from the id alone — ARCHITECTURE §4),
// keeps PRs and protections attached, and means the only thing a client must update is
// the clone URL, which is derived on read anyway.
//
// The authorization is deliberately two-sided: the CALLER must be able to administer the
// repo today, and the recipient must be a real principal. What this cannot do is ask the
// recipient's permission — there is no invitation model here — so a transfer is
// immediate and the audit trail is what makes it accountable.

func handleTransferRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleTransferRepo")
	defer span.End()

	// deleteRepo rather than updateRepo: giving a repo away is closer to destroying it
	// from the current owner's point of view than to editing its description, and it
	// should not ride on a grant handed out for metadata edits.
	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "deleteRepo")
	if !ok {
		return
	}
	var req struct {
		// Username of the new owner. The namespace follows from it, since a namespace
		// is the owner's handle.
		ToUsername string `json:"to_username"`
		// Optional rename as part of the move, for when the name is already taken in
		// the destination namespace.
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.ToUsername = strings.TrimSpace(req.ToUsername)
	if req.ToUsername == "" {
		http.Error(w, "to_username is required", http.StatusBadRequest)
		return
	}
	if !validRepoName(req.ToUsername) {
		// A namespace is a URL path segment, held to the same allowlist as a repo name.
		http.Error(w, "to_username is invalid", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = re.Name
	}
	if !validRepoName(name) {
		http.Error(w, "repo name is invalid", http.StatusBadRequest)
		return
	}

	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	// Resolve the recipient to a real gatekeeper user. Without this the repo would be
	// transferred to a namespace nobody owns — unreachable by its new "owner" and
	// invisible to its old one, which is a way to lose a repo rather than move it.
	target, err := getUserByName(ctx, token, req.ToUsername)
	if err != nil || target.UserID == "" {
		http.Error(w, "no such user", http.StatusBadRequest)
		return
	}
	if target.UserID == re.Owner && name == re.Name {
		http.Error(w, "the repo already belongs to that user", http.StatusConflict)
		return
	}

	prevOwner, prevNS := re.Owner, re.Namespace
	res := connect().WithContext(ctx).Model(&Repo{}).
		Where("id = ?", re.ID).
		Updates(map[string]any{
			"owner": target.UserID, "namespace": target.Username,
			"name": name, "updated_at": time.Now().UTC(),
			// Filing is namespace-scoped, so a project reference from the OLD namespace
			// cannot survive the move. Cleared rather than carried, which would leave the
			// repo claiming membership of a project its new owner may not even see.
			"project": "", "project_id": "", "project_namespace": "",
		})
	if res.Error != nil {
		if isUniqueViolation(res.Error) {
			http.Error(w, "a repo with this name already exists in the destination namespace", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "transfer repo", "Repo_id", re.ID, "error", res.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	out, err := getRepoByID(ctx, re.ID)
	if err != nil {
		slog.ErrorContext(ctx, "transfer repo: read back", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "repo transferred", "Repo_id", re.ID,
		"from_owner", prevOwner, "from_namespace", prevNS,
		"to_owner", target.UserID, "to_namespace", target.Username, "by", userID)
	auditEvent(ctx, userID, auditActionRepoUpdate, re.ID,
		auditDetail(out, "transferred from "+prevNS+" to "+target.Username))
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, out)
}
