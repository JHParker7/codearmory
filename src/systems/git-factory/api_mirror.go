package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// handleEnsureMirror is the pull-through-cache entry point (DESIGN-read-replicas.md §7),
// called by git-connector — NOT routed through Conductor's RBAC and NOT a user surface;
// it is authenticated solely by the shared X-Internal-Key, exactly like the git-connector
// broker's own /internal/clone-token.
//
// It is idempotent by design: creating the mirror if absent, then always fetching
// upstream. That single verb covers both "mirror this repo on connection" and "refresh
// it right before a CI run" — the latter by passing `ref`, which must exist after the
// fetch or the call fails, guaranteeing the clone that follows sees the commit it wants.
//
// Opt-in: nothing is mirrored unless this is called for it, and a mirror is READ-ONLY
// over the git wire (see authorizeGitRepo) and scoped to the CI service identity in
// `owner` — it is clonable only by that identity, independent of upstream's ACLs.
func handleEnsureMirror(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleEnsureMirror")
	defer span.End()

	if !internalKeyOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req mirrorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.UpstreamURL = strings.TrimSpace(req.UpstreamURL)
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.Name = strings.TrimSpace(req.Name)
	req.Owner = strings.TrimSpace(req.Owner)
	switch {
	case req.UpstreamURL == "":
		http.Error(w, "upstream_url is required", http.StatusBadRequest)
		return
	case validateFetchURL(req.UpstreamURL) != nil:
		// The URL becomes an argv element of `git fetch`, where a value starting with
		// a dash is parsed as an OPTION — `--upload-pack=<cmd>` being one git executes.
		// See validateFetchURL.
		http.Error(w, "upstream_url is not a valid remote", http.StatusBadRequest)
		return
	case req.Owner == "":
		http.Error(w, "owner is required", http.StatusBadRequest)
		return
	case !validRepoName(req.Name):
		http.Error(w, "name is invalid", http.StatusBadRequest)
		return
	case req.Namespace == "" || !validRepoName(req.Namespace):
		// A namespace is also a URL path segment and a routing key, so it is held to
		// the same allowlist as a repo name (§6 — never build a path from an
		// unvalidated string).
		http.Error(w, "namespace is invalid", http.StatusBadRequest)
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = defaultBranchName
	}

	// Find an existing repo at this clone path, or create the mirror row. Uniqueness is
	// (namespace, name); if that path is already taken by a NATIVE repo or a mirror
	// owned by someone else, refuse rather than hijack it.
	re, err := getRepoByPath(ctx, req.Namespace, req.Name)
	switch {
	case errors.Is(err, errRepoNotFound):
		re = Repo{
			ID:            uuid.New().String(),
			Owner:         req.Owner,
			Namespace:     req.Namespace,
			Name:          req.Name,
			DefaultBranch: req.DefaultBranch,
			Kind:          kindMirror,
			Visibility:    visibilityPrivate,
			CreatedAt:     time.Now().UTC(),
			UpdatedAt:     time.Now().UTC(),
		}
		if err := re.Add(ctx); err != nil {
			if isUniqueViolation(err) { // lost a create race — fall through by re-reading
				re, err = getRepoByPath(ctx, req.Namespace, req.Name)
			}
			if err != nil {
				slog.ErrorContext(ctx, "mirror: create row", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		}
	case err != nil:
		slog.ErrorContext(ctx, "mirror: lookup", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if re.Kind != kindMirror || re.Owner != req.Owner {
		// The path exists but isn't a mirror we may write, so fetching into it would
		// either clobber a native repo's bytes or serve one user's data to another.
		http.Error(w, "a repository already exists at this path", http.StatusConflict)
		return
	}

	if err := mirrorFetch(ctx, re, req.UpstreamURL); err != nil {
		// Upstream unreachable / auth failed / no such repo — a bad gateway, not our bug.
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	if req.Ref != "" && !refExists(ctx, re.ID, req.Ref) {
		// Fetched successfully but the requested commit/branch still isn't here: the
		// caller must not go on to clone expecting it. 404 the ref, not the mirror.
		http.Error(w, "ref not found after fetch: "+req.Ref, http.StatusNotFound)
		return
	}
	now := time.Now().UTC()
	if err := markMirrored(ctx, re.ID, req.UpstreamURL, now); err != nil {
		slog.ErrorContext(ctx, "mirror: mark", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Re-read so the response carries the derived http_url (the clone URL the caller
	// hands to the runner) and the freshly-stamped mirror_at.
	re, err = getRepoByID(ctx, re.ID)
	if err != nil {
		slog.ErrorContext(ctx, "mirror: reread", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "mirror ensured", "Repo_id", re.ID, "namespace", re.Namespace, "name", re.Name)
	writeJSON(w, http.StatusOK, re)
}

// cloneTokenTTL bounds. A CI clone is a one-shot; a short life keeps a leaked URL nearly
// worthless, and the cap stops a caller minting a long-lived credential.
const (
	defaultCloneTokenTTL = 15 * time.Minute
	maxCloneTokenTTL     = time.Hour
)

// handleMintCloneToken issues a runner-usable, read-only clone URL for one mirror
// (DESIGN §7). Internal-key gated like the mirror endpoint. Scoped to Kind=mirror repos:
// the whole point is cloning a cache by the CI identity — native repos keep going
// through gatekeeper, which is where their real ownership lives.
func handleMintCloneToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleMintCloneToken")
	defer span.End()

	if !internalKeyOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if cloneTokenKey() == "" {
		http.Error(w, "clone tokens are not configured", http.StatusServiceUnavailable)
		return
	}

	var req cloneTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var (
		re  Repo
		err error
	)
	switch {
	case strings.TrimSpace(req.RepoID) != "":
		re, err = getRepoByID(ctx, req.RepoID)
	case req.Namespace != "" && req.Name != "":
		re, err = getRepoByPath(ctx, req.Namespace, req.Name)
	default:
		http.Error(w, "repo_id or namespace+name is required", http.StatusBadRequest)
		return
	}
	if errors.Is(err, errRepoNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "clone-token: lookup", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if re.Kind != kindMirror {
		http.Error(w, "clone tokens are issued only for mirrors", http.StatusConflict)
		return
	}

	ttl := defaultCloneTokenTTL
	if req.TTLSeconds > 0 {
		if ttl = time.Duration(req.TTLSeconds) * time.Second; ttl > maxCloneTokenTTL {
			ttl = maxCloneTokenTTL
		}
	}
	token, expiresAt := mintCloneToken(re.ID, ttl)
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "clone token minted", "Repo_id", re.ID, "expires_at", expiresAt)
	writeJSON(w, http.StatusOK, cloneTokenResponse{
		CloneURL:  authenticatedCloneURL(re, token),
		ExpiresAt: expiresAt,
	})
}
