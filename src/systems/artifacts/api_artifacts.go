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
	out, err := listArtifacts(ctx, userID)
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
	a, err := getArtifact(ctx, userID, name)
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
	a, err := getArtifact(ctx, userID, name)
	if errors.Is(err, errNoSuch) {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to get artifact", http.StatusInternalServerError)
		return
	}
	f, err := store.Open(ctx, userID, name)
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
	if prev, err := getArtifact(ctx, userID, name); err == nil {
		prevSize = prev.SizeBytes
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
	}
	if err := upsertArtifact(ctx, a); err != nil {
		// The blob landed but the row did not: remove it rather than leaking bytes
		// that count against nothing and can never be listed or deleted.
		store.Remove(ctx, userID, name) //nolint:errcheck
		slog.ErrorContext(ctx, "record artifact", "user_id", userID, "name", name, "error", err)
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
	n, err := deleteArtifact(ctx, userID, name)
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
	if err := store.Remove(ctx, userID, name); err != nil {
		slog.WarnContext(ctx, "artifact blob remove failed", "user_id", userID, "name", name, "error", err)
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
