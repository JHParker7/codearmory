package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// handleListRepos returns the repo selector's options: every repo discoverable by
// enumerating the caller's linked backends, plus any repos they pinned manually.
// Enumeration is best-effort per backend so one unreachable host or an
// un-enumerable generic backend never blanks the list.
func handleListRepos(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleListRepos")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listRepo", "git_connector/repos")
	if !ok {
		return
	}
	backends, err := listBackends(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "list backends", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var enumerated []repoView
	for _, b := range backends {
		repos, err := enumerateRepos(ctx, b)
		if err != nil {
			slog.WarnContext(ctx, "repo enumeration failed", "backend", b.Name, "type", b.Type, "error", err)
			continue
		}
		enumerated = append(enumerated, repos...)
	}
	manual, err := listRepos(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "list repos", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, mergeRepos(backends, enumerated, manual))
}

// handleListBranches returns the branch selector's options for a repo: the
// branches of the clone URL in the `url` query param, enumerated via the owning
// backend's API and feeding forge's checkout.ref. Reuses the listRepo permission
// since branch listing is part of choosing a clone target. Generic (un-enumerable)
// backends return an empty list so the UI falls back to a free-text ref.
func handleListBranches(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleListBranches")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listRepo", "git_connector/repos")
	if !ok {
		return
	}
	repoURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if repoURL == "" {
		http.Error(w, "url query parameter is required", http.StatusBadRequest)
		return
	}
	host, err := deriveHost(repoURL)
	if err != nil {
		http.Error(w, "url: "+err.Error(), http.StatusBadRequest)
		return
	}
	b, err := getBackendByHost(ctx, userID, host)
	if errors.Is(err, errBackendNotFound) {
		http.Error(w, "no git backend linked for that repository host", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "list branches: get backend", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	branches, err := enumerateBranches(ctx, b, repoURL)
	if err != nil {
		slog.WarnContext(ctx, "branch enumeration failed", "backend", b.Name, "type", b.Type, "error", err)
		http.Error(w, "failed to list branches: "+err.Error(), http.StatusBadGateway)
		return
	}
	if branches == nil {
		branches = []branchView{}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, branches)
}

// handleCreateRepo pins a repo to the caller's selector. Useful for generic
// backends (which can't be enumerated) or to surface a repo the enumeration page
// limit dropped. The URL's host need not match a linked backend at registration
// time, but minting a credential for it later still requires one.
func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleCreateRepo")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "createRepo", "git_connector/repos")
	if !ok {
		return
	}
	var req createRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	host, err := deriveHost(req.URL)
	if err != nil {
		http.Error(w, "url: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = repoNameFromURL(req.URL)
	}
	rp := GitRepo{
		ID:        uuid.New().String(),
		Owner:     userID,
		Name:      name,
		URL:       req.URL,
		Host:      host,
		CreatedAt: time.Now().UTC(),
	}
	if err := rp.Add(ctx); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "a repo with this url is already registered", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "create repo", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	view := repoView{ID: rp.ID, Name: rp.Name, URL: rp.URL, Source: repoSourceManual}
	if b, err := getBackendByHost(ctx, userID, host); err == nil {
		view.Backend, view.BackendType = b.Name, b.Type
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repo registered", "user_id", userID, "repo_id", rp.ID, "host", host)
	writeJSON(w, http.StatusCreated, view)
}

// handleDeleteRepo removes a manually-registered repo. Enumerated repos have no id
// and cannot be deleted (they vanish on their own when the backend stops listing them).
func handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("git").Start(r.Context(), "handleDeleteRepo")
	defer span.End()

	id := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "deleteRepo", "git_connector/repos/"+id)
	if !ok {
		return
	}
	rp := GitRepo{ID: id, Owner: userID}
	if err := rp.Remove(ctx); err != nil {
		if errors.Is(err, errRepoNotFound) {
			http.Error(w, "repo not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "delete repo", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repo deleted", "user_id", userID, "repo_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// mergeRepos overlays manual repos (which carry an id and a delete affordance) onto
// the enumerated set, deduping by clone URL so a pinned repo that also enumerates
// shows once. Manual repos lead so their id-bearing entry wins the dedupe.
func mergeRepos(backends []GitBackend, enumerated []repoView, manual []GitRepo) []repoView {
	byHost := make(map[string]GitBackend, len(backends))
	for _, b := range backends {
		byHost[b.Host] = b
	}
	seen := make(map[string]bool)
	out := make([]repoView, 0, len(manual)+len(enumerated))
	for _, m := range manual {
		key := strings.ToLower(m.URL)
		if seen[key] {
			continue
		}
		seen[key] = true
		v := repoView{ID: m.ID, Name: m.Name, URL: m.URL, Source: repoSourceManual}
		if b, ok := byHost[m.Host]; ok {
			v.Backend, v.BackendType = b.Name, b.Type
		}
		out = append(out, v)
	}
	for _, e := range enumerated {
		key := strings.ToLower(e.URL)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// repoNameFromURL derives a human-friendly "owner/repo" display name from a clone
// URL, falling back to the last path segment or the host.
func repoNameFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if p == "" {
		return u.Host
	}
	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return parts[len(parts)-1]
}
