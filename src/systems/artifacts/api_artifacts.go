package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

var errOverQuota = errors.New("artifact exceeds the remaining quota")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// handleListArtifacts returns the caller's artifacts.
func handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listArtifact", "artifacts/artifacts")
	if !ok {
		return
	}
	out, err := listArtifacts(ctx, userID, r.URL.Query().Get("project"), accessibleProjectIDs(ctx, r.Header.Get("Authorization")))
	if err != nil {
		slog.ErrorContext(ctx, "list artifacts", "user_id", userID, "error", err)
		http.Error(w, "failed to list artifacts", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetArtifact returns one artifact's metadata.
func handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getArtifact", "artifacts/artifacts/"+name)
	if !ok {
		return
	}
	// Owner OR a member of the project (?project=) the artifact is filed into. A
	// non-owner without a project grant is indistinguishable from a missing row (404).
	a, err := loadAuthorizedArtifact(ctx, r.Header.Get("Authorization"), userID, name, "getArtifact", r.URL.Query().Get("project"))
	if errors.Is(err, errNoSuch) {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to get artifact", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleDownloadArtifact streams an artifact's bytes.
func handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getArtifact", "artifacts/artifacts/"+name)
	if !ok {
		return
	}
	// Owner OR a project member (?project=). The blob store is keyed by the OWNER's id,
	// so read it under a.UserID — which is the caller for their own, and the filing
	// owner for a project artifact reached as a member.
	a, err := loadAuthorizedArtifact(ctx, r.Header.Get("Authorization"), userID, name, "getArtifact", r.URL.Query().Get("project"))
	if errors.Is(err, errNoSuch) {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to get artifact", http.StatusInternalServerError)
		return
	}
	f, err := store.Open(ctx, a.UserID, a.Name)
	if err != nil {
		// The row exists but the blob does not — the store and the DB have diverged.
		// Report it rather than serving an empty body that looks like a valid cache.
		slog.ErrorContext(ctx, "artifact blob missing", "user_id", userID, "name", name, "error", err)
		http.Error(w, "artifact content unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	if a.SHA256 != "" {
		w.Header().Set("X-Artifact-SHA256", a.SHA256)
	}
	io.Copy(w, f) //nolint:errcheck — the client hanging up mid-download is not our error
}

// handleUploadArtifact stores (or replaces) an artifact, enforcing the caller's quota.
func handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createArtifact", "artifacts/artifacts")
	if !ok {
		return
	}
	if msg := validateName(name); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Project scoping. The project slug rides on a query param (?project=) rather than
	// the request body, because on this route the body IS the blob. If it names a real
	// gatekeeper project the caller can reach, file the artifact into it — but only if
	// the caller may create within it. A slug that resolves to nothing stays a free-text
	// label (unchanged behaviour); a slug the caller may only view is refused rather than
	// silently downgraded to a label. Resolved BEFORE the blob is streamed so a refused
	// upload spends no bandwidth. Mirrors forge's submit handler.
	projectSlug := r.URL.Query().Get("project")
	var projectID, projectNamespace string
	if projectSlug != "" {
		bearer := r.Header.Get("Authorization")
		if p := resolveProjectSlug(ctx, bearer, projectSlug); p != nil {
			if !checkProjectPermission(ctx, bearer, "createArtifact", "artifacts", p.Slug, "") {
				http.Error(w, "you cannot create artifacts in project "+p.Slug, http.StatusForbidden)
				return
			}
			projectID = p.ProjectID
			projectNamespace = p.Namespace
		}
	}

	maxBytes, _, _, err := effectiveQuota(ctx, userID)
	if err != nil {
		http.Error(w, "failed to resolve quota", http.StatusInternalServerError)
		return
	}
	used, _, err := usage(ctx, userID)
	if err != nil {
		http.Error(w, "failed to resolve usage", http.StatusInternalServerError)
		return
	}
	// Replacing an artifact frees its current size, so the headroom is the quota
	// minus everything EXCEPT the one being overwritten. Without this, re-saving a
	// cache that already fills most of the quota would always fail.
	var prevSize int64
	replacing := false
	if prev, err := getArtifact(ctx, userID, name); err == nil {
		prevSize = prev.SizeBytes
		replacing = true
	}
	headroom := maxBytes - (used - prevSize)
	if headroom <= 0 {
		http.Error(w, quotaMsg(0, maxBytes, used-prevSize), http.StatusRequestEntityTooLarge)
		return
	}
	// A declared Content-Length over the headroom is rejected before reading the
	// body — no point streaming gigabytes we will refuse. It is only a shortcut:
	// writeBlob enforces the real limit as it copies, since the header is a claim.
	if r.ContentLength > 0 && r.ContentLength > headroom {
		http.Error(w, quotaMsg(r.ContentLength, maxBytes, used-prevSize), http.StatusRequestEntityTooLarge)
		return
	}

	size, digest, err := store.Write(ctx, userID, name, r.Body, headroom)
	if errors.Is(err, errOverQuota) {
		http.Error(w, quotaMsg(size, maxBytes, used-prevSize), http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "write artifact", "user_id", userID, "name", name, "error", err)
		http.Error(w, "failed to store artifact", http.StatusInternalServerError)
		return
	}

	ct := r.Header.Get("Content-Type")
	if len(ct) > maxContentTypeLen {
		ct = ct[:maxContentTypeLen]
	}
	a := Artifact{
		ArtifactID: uuid.New().String(), UserID: userID, OrgID: orgID,
		Name: name, SizeBytes: size, ContentType: ct, SHA256: digest,
		Project: projectSlug, ProjectID: projectID, ProjectNamespace: projectNamespace,
	}
	if err := upsertArtifact(ctx, a); err != nil {
		// The blob landed but the row did not. What to do about it depends on whether
		// this was a first save or a REPLACE, and the difference is the whole point:
		//
		//   - First save (no row existed): the blob is orphaned — nothing can list,
		//     download or delete it, and it counts against nothing. Remove it.
		//   - Replace: store.Write already renamed the new blob over the old one, so
		//     the old bytes are gone. The pre-existing ROW survived, and removing the
		//     blob would leave it pointing at nothing — destroying a previously-good
		//     artifact for good, permanently 500ing GET .../content, and charging its
		//     size against the quota forever. Keep the blob: the row's size/digest lag
		//     by one save until the client retries (which is exactly what a failed
		//     step does), and until then the artifact still downloads.
		//
		// Ordering note: the row cannot simply be committed first — a failed write
		// would then leave a row advertising a size and digest no blob has. Holding a
		// transaction open across the upload instead would pin a connection (and a row
		// lock) for the length of a multi-gigabyte transfer. So the blob write stays
		// first and the compensation is scoped to the case where it is safe.
		if !replacing {
			store.Remove(ctx, userID, name) //nolint:errcheck
		}
		slog.ErrorContext(ctx, "record artifact", "user_id", userID, "name", name,
			"replacing", replacing, "orphan_blob_removed", !replacing, "error", err)
		http.Error(w, "failed to store artifact", http.StatusInternalServerError)
		return
	}
	stored, err := getArtifact(ctx, userID, name)
	if err != nil {
		writeJSON(w, http.StatusCreated, a)
		return
	}
	slog.InfoContext(ctx, "artifact stored", "user_id", userID, "name", name, "size_bytes", size)
	writeJSON(w, http.StatusCreated, stored)
}

// handleDeleteArtifact removes an artifact and frees its quota.
func handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteArtifact", "artifacts/artifacts/"+name)
	if !ok {
		return
	}
	// Owner OR a project member (?project=) with the delete grant. The row and blob are
	// keyed by the OWNER's id, so operate on a.UserID — the caller for their own, the
	// filing owner for a project artifact.
	a, err := loadAuthorizedArtifact(ctx, r.Header.Get("Authorization"), userID, name, "deleteArtifact", r.URL.Query().Get("project"))
	if errors.Is(err, errNoSuch) {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to delete artifact", http.StatusInternalServerError)
		return
	}
	n, err := deleteArtifact(ctx, a.UserID, a.Name)
	if err != nil {
		http.Error(w, "failed to delete artifact", http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	// Drop the row first: an orphaned blob wastes disk, but an orphaned ROW would
	// keep charging the user's quota for bytes they can no longer reach.
	if err := store.Remove(ctx, a.UserID, a.Name); err != nil {
		slog.WarnContext(ctx, "artifact blob remove failed", "user_id", a.UserID, "name", a.Name, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetUsage reports the caller's own quota and usage, so a pipeline (or a user)
// can see the headroom without admin rights.
func handleGetUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listArtifact", "artifacts/artifacts")
	if !ok {
		return
	}
	view, err := quotaView(ctx, userID)
	if err != nil {
		http.Error(w, "failed to resolve quota", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func quotaMsg(want, maxBytes, used int64) string {
	return "artifact quota exceeded: " + humanMB(used) + " used + " + humanMB(want) +
		" requested > " + humanMB(maxBytes) + " cap"
}

func humanMB(b int64) string {
	return strconv.FormatFloat(float64(b)/(1024*1024), 'f', 1, 64) + " MB"
}
