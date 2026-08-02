package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Resource/action naming convention (must match the registry manifest):
//
//	action   = camelCase verb + singular noun: createRepo, listRepo, getRepo…
//	resource = "<service>/<collection>" for the collection, "<service>/<collection>/<id>"
//	           for a specific record. gatekeeper auto-scopes the resource by the
//	           caller's username, so grants are templated as {username}/<service>/…
const (
	// resRepos is the COLLECTION: "in my space". It stays unscoped so gatekeeper
	// applies the caller's own name — listing and creating are always your own.
	resRepos = serviceName + "/repos"
)

// resRepoOf is the PER-RECORD resource, and it leads with the repo's OWNER rather
// than the caller: "<namespace>/<service>/repos/<id>". That is what lets a permission
// name someone else's repository — with a caller-scoped resource, alice asking about
// bob's repo and about her own produce the identical string, so no grant can tell them
// apart and sharing is inexpressible (gatekeeper commit cdb492e2).
//
// The id, not the name, is the last segment: a rename is metadata-only (ARCHITECTURE
// §4), so grants must survive one.
func resRepoOf(re Repo) string {
	return re.Namespace + "/" + serviceName + "/repos/" + re.ID
}

// publicReadActions is the closed set of actions a public repo grants to everyone, no
// grant required. An allowlist rather than a "not a write" test, so an action added
// later is private until someone decides otherwise. Note what is NOT here: createPull
// and shareRepo read as reads but write, listProtection exposes a repo's rules, and
// writeRepo (push) is never public whatever the visibility.
//
// Every handler taking one of these actions discards the caller id (`re, _, ok :=`),
// which is what makes it safe for the public path to authorize without establishing
// one — see authorizeRepo.
var publicReadActions = map[string]bool{
	"readRepo":   true, // the git wire fetch/clone
	"getRepo":    true,
	"getReadme":  true,
	"listCommit": true,
	"listBranch": true,
	"listTag":    true,
	"getTree":    true,
	"getBlob":    true,
	"getArchive": true,
	"listPull":   true,
	"getPull":    true,
}

// allowedByVisibility reports whether the repo's own visibility authorizes action,
// with no grant and no identity. This is the single place public access is decided:
// the management API (authorizeRepo) and the git wire (authorizeGitRepo) both defer
// to it, so "what may an anonymous caller do to a public repo" has one answer rather
// than two that drift.
func allowedByVisibility(re Repo, action string) bool {
	return re.Visibility == visibilityPublic && publicReadActions[action]
}

// validVisibility reports whether v is a visibility the API accepts. Empty is handled
// by the caller (create defaults it, update leaves it unchanged), never here.
func validVisibility(v string) bool {
	return v == visibilityPrivate || v == visibilityPublic
}

// repoNameRe is the allowlist ARCHITECTURE §6 mandates for any string that becomes
// a URL path segment or a filesystem name. Anchored, so a partial match can't slip
// a separator or space through.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// protectionPatternRe allows a branch name or a glob of one. The value reaches a shell
// `case` statement in the pre-receive hook, so anything that could carry a quote, a
// space or a substitution is refused here rather than escaped there.
var protectionPatternRe = regexp.MustCompile(`^[A-Za-z0-9._*/-]+$`)

// validRepoName reports whether name is safe to use as a repo name. Beyond the
// allowlist it rejects two cases the pattern alone would admit:
//   - "." and ".." — path-traversal names made only of permitted characters
//   - a ".git" suffix — which would yield a doubled "/{name}.git.git" clone URL
//
// Dots are otherwise legal, so "my.repo" is a valid name.
func validRepoName(name string) bool {
	if !repoNameRe.MatchString(name) {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	return !strings.HasSuffix(name, ".git")
}

// authorizeRepo loads the repo addressed by id and authorizes the caller for action
// against the repo's OWN namespace, writing the response and returning ok=false when
// it cannot.
//
// Load-then-check, not check-then-load: the resource names the owner, which is a
// property of the record. A missing repo and a repo the caller may not touch are both
// 404 — a 403 would confirm that someone else's repo exists.
func authorizeRepo(ctx context.Context, w http.ResponseWriter, r *http.Request, id, action string) (Repo, string, bool) {
	// Answer "who are you?" before "does it exist?". Loading first would 404 an
	// unauthenticated caller on a missing id, when the honest answer is 401.
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Repo{}, "", false
	}
	re, err := getRepoByID(ctx, id)
	if err != nil {
		if errors.Is(err, errRepoNotFound) {
			http.Error(w, "Repo not found", http.StatusNotFound)
			return Repo{}, "", false
		}
		slog.ErrorContext(ctx, "load repo", "Repo_id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return Repo{}, "", false
	}
	// A denial must read as "not found", not "forbidden": 403 on someone else's repo
	// confirms it exists and who owns it. The service used to own that translation via
	// an owner-equality check; now that the decision is gatekeeper's, the 403 it writes
	// is intercepted and rewritten here so the property survives the move.
	aw := &notFoundOnDeny{ResponseWriter: w}
	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, aw, r, action, resRepoOf(re))
	if !ok {
		// Only a denial is overridden below: a 401 (no credential at all) or a 5xx has
		// already answered the caller and must pass through untouched.
		if !aw.swallowed {
			return Repo{}, "", false
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		// The record's own owner, for the namespaces no grant can name (see
		// callerOwnsRepo). Tried first: it is the narrowest of the three fallbacks.
		if uid, own := callerOwnsRepo(ctx, token, action, re); own {
			return re, uid, true
		}
		// A repo filed into a project is reachable by anyone holding the matching
		// project role, even without a per-repo grant — tried after the per-repo check.
		if token != "" {
			if uid, pok := repoProjectAuthorizes(ctx, token, action, re); pok {
				return re, uid, true
			}
		}
		// A public repo authorizes its own reads. The per-record grant is absent by
		// design — the resource lives in the OWNER's namespace, so no other user holds
		// it — and visibility is the fact that stands in for it.
		if allowedByVisibility(re, action) {
			return re, "", true
		}
		http.Error(w, "Repo not found", http.StatusNotFound)
		return Repo{}, "", false
	}
	return re, userID, true
}

// callerOwnsRepo is the second half of the owner-first decision, and it runs only
// after gatekeeper has already DENIED the per-record resource.
//
// resRepoOf names the repo's namespace, which is the owner's username for a personal
// repo — but an ORG repo's namespace is the org's name, and a default grant cannot be
// templated for it (gatekeeper substitutes {username}/{user_id}/{org_id}, never the org
// NAME), so an org repo's own creator would be locked out of it. A user rename opens
// the same gap, since the issued grant still carries the old handle.
//
// The fallback closes that without reopening the caller-scoped hole the owner-first
// resource exists to shut. It grants exactly one thing — "this record is yours" — and
// it is not a query filter that a later handler can forget: the caller's identity comes
// back from gatekeeper (never from a client-supplied header), the same action must be
// held in the caller's OWN namespace, and the decision then turns on the Owner recorded
// on the loaded record. A caller probing someone else's id clears the first half and
// fails the equality, so nothing is authorized that ownership does not justify.
func callerOwnsRepo(ctx context.Context, bearer, action string, re Repo) (string, bool) {
	if bearer == "" || re.Owner == "" {
		return "", false
	}
	// Both grant shapes are tried, because gatekeeper's two forms are DISJOINT and
	// which one a deployment uses is its own choice. matchPermission treats "…/repos/*"
	// as a prefix that requires the slash, so it never covers the bare "…/repos"; and a
	// plain "…/repos" is an exact string that never covers "…/repos/<id>". A manifest
	// that models per-record verbs the natural way — getRepo/updateRepo/readRepo on
	// "{username}/<service>/repos/*", leaving only createRepo/listRepo on the
	// collection — is therefore entirely reasonable, and asking solely about the
	// collection would refuse the owner of an ORG repo under it. The failure is silent:
	// the caller sees the 404 this fallback exists to prevent, so a control-plane
	// upgrade that ships ahead of its registry manifest loses org repos with nothing
	// in the logs pointing at a grant shape.
	//
	// Neither form decides anything on its own. Both are caller-scoped ("do you hold
	// this verb over your OWN repos at all"), and a caller holding a wildcard passes
	// either one for any id — which is precisely why the id is not the authorization.
	// The decision below is the equality against the loaded record's Owner.
	for _, resource := range []string{serviceName + "/repos/" + re.ID, resRepos} {
		body := map[string]string{"service": serviceName, "action": action, "resource": resource}
		var res struct {
			Authorized bool   `json:"authorized"`
			UserID     string `json:"user_id"`
		}
		if err := gatekeeperCall(ctx, bearer, http.MethodPost, "/check_permissions", body, &res); err != nil || !res.Authorized {
			continue
		}
		if res.UserID == "" || res.UserID != re.Owner {
			return "", false
		}
		return res.UserID, true
	}
	return "", false
}

// notFoundOnDeny swallows a 403 written by the gatekeeper SDK so the caller can answer
// 404 instead. Every other status — 401 above all — passes through untouched.
type notFoundOnDeny struct {
	http.ResponseWriter
	swallowed bool
	written   bool
}

func (n *notFoundOnDeny) WriteHeader(status int) {
	if status == http.StatusForbidden {
		n.swallowed = true
		return
	}
	n.written = true
	n.ResponseWriter.WriteHeader(status)
}

func (n *notFoundOnDeny) Write(b []byte) (int, error) {
	if n.swallowed {
		return len(b), nil // drop the "forbidden" body along with its status
	}
	return n.ResponseWriter.Write(b)
}

// handleListRepos returns every Repo owned by the caller.
func handleListRepos(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListRepos")
	defer span.End()

	userID, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRepo", resRepos)
	if !ok {
		return
	}
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	Repos, err := listRepos(ctx, userID, accessibleProjectIDs(ctx, bearer))
	if err != nil {
		slog.ErrorContext(ctx, "list Repos", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if Repos == nil {
		Repos = []Repo{}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, Repos)
}

// handleCreateRepo creates a Repo owned by the caller.
func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCreateRepo")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createRepo", resRepos)
	if !ok {
		return
	}
	var req createRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// Validate the name BEFORE any gatekeeper round-trip: it becomes a URL path
	// segment and a filesystem name, so it must be rejected up front (§6), and a
	// bad request shouldn't cost an identity lookup.
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if !validRepoName(req.Name) {
		http.Error(w, "repo name is invalid", http.StatusBadRequest)
		return
	}
	// Absent means private. An unrecognised value is refused rather than coerced: a
	// typo'd "publik" silently creating a private repo is recoverable, but the reverse
	// coercion would not be, and a caller that meant to publish deserves to be told.
	if req.Visibility == "" {
		req.Visibility = visibilityPrivate
	}
	if !validVisibility(req.Visibility) {
		http.Error(w, "visibility must be 'private' or 'public'", http.StatusBadRequest)
		return
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, "failed to get the jwt", http.StatusBadRequest)
		return
	}

	// namespace is the human-readable handle that appears in the clone URL: the
	// org's name for an org repo, otherwise the caller's username. It is display
	// and routing only — ownership is always the gatekeeper user_id below.
	var namespace string
	if req.AssignToOrg {
		org, err := getOrg(ctx, token, orgID)
		if err != nil {
			http.Error(w, "failed to get the org name", http.StatusBadRequest)
			return
		}
		namespace = org.OrgName
	} else {
		user, err := getUser(ctx, token, userID)
		if err != nil {
			http.Error(w, "failed to get the user name", http.StatusBadRequest)
			return
		}
		namespace = user.Username
	}

	re := Repo{
		ID:          uuid.New().String(),
		Owner:       userID, // the gatekeeper user_id — the authorization filter every other handler scopes by
		Namespace:   namespace,
		Name:        req.Name,
		Description: req.Description,
		// Set explicitly rather than left to the column default: CreateGitRepo reads it
		// to point HEAD at the branch this response advertises, and it runs before the
		// DB default would ever be read back into the struct.
		DefaultBranch: defaultBranchName,
		Visibility:    req.Visibility,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	// File into a project when asked and permitted (developer/admin/owner). A slug that
	// resolves to no accessible project is kept as a plain label; a project the caller
	// may only view is refused rather than silently downgraded.
	if req.Project != "" {
		re.Project = req.Project
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if p := resolveProjectSlug(ctx, bearer, req.Project); p != nil {
			if !projectAllowsRepoAction(ctx, bearer, "createRepo", p.Slug) {
				http.Error(w, "you cannot create repos in project "+p.Slug, http.StatusForbidden)
				return
			}
			re.ProjectID, re.ProjectNamespace = p.ProjectID, p.Namespace
		}
	}
	if err := re.Add(ctx); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "a Repo with this name already exists", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "create Repo", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	re, err := getRepo(ctx, re.Owner, re.ID)
	if err != nil {
		slog.ErrorContext(ctx, "create Repo check created", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	meterReposCreated.Add(ctx, 1)
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "Repo created", "user_id", userID, "Repo_id", re.ID)
	auditEvent(ctx, userID, auditActionRepoCreate, re.ID, auditDetail(re, "visibility="+re.Visibility))
	writeJSON(w, http.StatusCreated, re)
}

// handleGetRepo returns a single Repo owned by the caller.
func handleGetRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGetRepo")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getRepo")
	if !ok {
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, re)
}

// handleUpdateRepo updates a Repo owned by the caller.
func handleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleUpdateRepo")
	defer span.End()

	id := r.PathValue("id")
	_, userID, ok := authorizeRepo(ctx, w, r, id, "updateRepo")
	if !ok {
		return
	}
	// The DB write below is owner-scoped on purpose: a read grant must never become a
	// rename. Widening this to collaborators is a deliberate later step.
	var req updateRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// A file edit rides on this already-registered route (so it works through the
	// gateway), but is a content change, not metadata: it needs writeRepo, checked here
	// so an updateRepo-only grant can never rewrite files.
	if req.File != nil {
		writeRepoFileEdit(ctx, w, r, id, req.File)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		repo, err := getRepo(ctx, userID, id)
		if err != nil {
			if errors.Is(err, errRepoNotFound) {
				http.Error(w, "Repo not found", http.StatusNotFound)
				return
			}
		}
		req.Name = repo.Name

	}
	// Empty means "leave it alone" (Repo.Update skips the column); anything else has
	// to be a visibility we recognise, since this is how a repo gets published.
	if req.Visibility != "" && !validVisibility(req.Visibility) {
		http.Error(w, "visibility must be 'private' or 'public'", http.StatusBadRequest)
		return
	}
	re := Repo{ID: id, Owner: userID, Name: req.Name, Description: req.Description, Visibility: req.Visibility}
	// File (or re-file) into a project when named and permitted. Unresolved slugs stay
	// plain labels; a project the caller may only view is refused.
	if req.Project != "" {
		re.Project = req.Project
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if p := resolveProjectSlug(ctx, bearer, req.Project); p != nil {
			if !projectAllowsRepoAction(ctx, bearer, "updateRepo", p.Slug) {
				http.Error(w, "you cannot file repos into project "+p.Slug, http.StatusForbidden)
				return
			}
			re.ProjectID, re.ProjectNamespace = p.ProjectID, p.Namespace
		}
	}
	if err := re.Update(ctx); err != nil {
		if errors.Is(err, errRepoNotFound) {
			http.Error(w, "Repo not found", http.StatusNotFound)
			return
		}
		if isUniqueViolation(err) {
			http.Error(w, "a Repo with this name already exists", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "update Repo", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	updated, err := getRepo(ctx, userID, id)
	if err != nil {
		slog.ErrorContext(ctx, "reload Repo", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteRepo deletes a Repo owned by the caller.
func handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleDeleteRepo")
	defer span.End()

	id := r.PathValue("id")
	target, userID, ok := authorizeRepo(ctx, w, r, id, "deleteRepo")
	if !ok {
		return
	}
	// Owner-scoped below: deletion destroys the bytes, so it stays the owner's alone.
	re := Repo{ID: id, Owner: userID}
	if err := re.Remove(ctx); err != nil {
		if errors.Is(err, errRepoNotFound) {
			http.Error(w, "Repo not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "delete Repo", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "Repo deleted", "user_id", userID, "Repo_id", id)
	// Recorded AFTER the row is gone, so the trail only ever claims deletions that
	// happened. The clone path is in the detail because the id is about to stop
	// resolving to anything a reader can look up.
	auditEvent(ctx, userID, auditActionRepoDelete, id, auditDetail(target, "outcome=ok"))
	w.WriteHeader(http.StatusNoContent)
}

// handleListCommits returns a repo's commit history. Read-only, and owner-scoped by the
// same getRepo the rest of the management API uses — gatekeeper authorises the action,
// the owner filter authorises the record.
func handleListCommits(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListCommits")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listCommit")
	if !ok {
		return
	}

	// ref comes from the query string and becomes a git argument, so it is held to the
	// same allowlist as a branch name; anything else falls back to HEAD rather than
	// being passed through. grep/author are literal substrings (see commitQuery), so
	// they need no allowlist — they can never be read as options or patterns.
	qs := r.URL.Query()
	// ?sha= turns this into "one commit with its diff". Served from this already-
	// registered route (rather than a separate /commits/{sha}) so it works through the
	// gateway, which only forwards registered routes.
	if sha := strings.TrimSpace(qs.Get("sha")); sha != "" {
		if !branchNameRe.MatchString(sha) {
			http.Error(w, "invalid commit", http.StatusBadRequest)
			return
		}
		d, err := commitDetailFor(ctx, re.ID, sha)
		if err != nil {
			if errors.Is(err, errPathNotFound) {
				http.Error(w, "commit not found", http.StatusNotFound)
				return
			}
			slog.ErrorContext(ctx, "commit detail", "Repo_id", re.ID, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Ok, "")
		writeJSON(w, http.StatusOK, d)
		return
	}
	// ?base=&head= previews the changes between two branches (the new-PR compare view).
	// Served from this registered route so it works through the gateway.
	if base := strings.TrimSpace(qs.Get("base")); base != "" {
		head := strings.TrimSpace(qs.Get("head"))
		if !branchNameRe.MatchString(base) || !branchNameRe.MatchString(head) {
			http.Error(w, "invalid ref", http.StatusBadRequest)
			return
		}
		out := map[string]any{"commits": commitsBetween(ctx, re.ID, base, head)}
		if changes, err := diffStat(ctx, re.ID, base, head); err == nil {
			out["files"] = changes
		}
		if patch, err := diffPatch(ctx, re.ID, base, head); err == nil {
			out["diff"] = patch
		}
		span.SetStatus(codes.Ok, "")
		writeJSON(w, http.StatusOK, out)
		return
	}
	ref := qs.Get("ref")
	if ref != "" && !branchNameRe.MatchString(ref) {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(qs.Get("limit"))
	skip, _ := strconv.Atoi(qs.Get("skip"))
	q := commitQuery{
		Ref:    ref,
		Skip:   skip,
		Limit:  limit,
		Grep:   strings.TrimSpace(qs.Get("q")),
		Author: strings.TrimSpace(qs.Get("author")),
	}

	commits, err := listCommits(ctx, re.ID, q)
	if err != nil {
		slog.ErrorContext(ctx, "list commits", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// The total is computed with the SAME filters as the page, so the pager can never
	// promise pages the query cannot fill.
	total, err := countCommits(ctx, re.ID, q)
	if err != nil {
		slog.ErrorContext(ctx, "count commits", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"commits": commits,
		"total":   total,
		"skip":    q.Skip,
		"limit":   len(commits),
	})
}

// handleCommit returns one commit with its per-file summary and full unified diff.
// Authorized with listCommit (a public-read action): whoever may see the history may
// see any one commit's changes.
func handleCommit(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCommit")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listCommit")
	if !ok {
		return
	}
	rev := r.PathValue("sha")
	if !branchNameRe.MatchString(rev) { // becomes a git argv element
		http.Error(w, "invalid commit", http.StatusBadRequest)
		return
	}
	d, err := commitDetailFor(ctx, re.ID, rev)
	if err != nil {
		if errors.Is(err, errPathNotFound) {
			http.Error(w, "commit not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "commit detail", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, d)
}

// handleWriteBlob commits an edited (or new) file to a branch — the portal's file
// editor. Authorized with writeRepo (same as a push); a file edit is a fast-forward
// commit, so protections that only block force-push/delete do not apply.
func handleWriteBlob(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleWriteBlob")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "writeRepo")
	if !ok {
		return
	}
	var req struct {
		Ref     string `json:"ref"`
		Path    string `json:"path"`
		Content string `json:"content"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Ref = strings.TrimSpace(req.Ref)
	if req.Ref == "" {
		req.Ref = headBranch(ctx, re.ID)
	}
	if !branchNameRe.MatchString(req.Ref) {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	req.Path = strings.TrimPrefix(strings.TrimSpace(req.Path), "/")
	if req.Path == "" || strings.Contains(req.Path, "..") || req.Path == ".git" || strings.HasPrefix(req.Path, ".git/") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	span.SetStatus(codes.Ok, "")
	commitFileAndRespond(ctx, w, re.ID, userID, token, req.Ref, req.Path, req.Content, req.Message)
}

// writeRepoFileEdit is the file-edit path of PATCH /repos/{id} (the browser editor's
// route). updateRepo was already checked by the caller; a content write additionally
// requires writeRepo, checked here so a metadata-only grant can never rewrite files.
func writeRepoFileEdit(ctx context.Context, w http.ResponseWriter, r *http.Request, repoID string, f *fileEditRequest) {
	_, userID, ok := authorizeRepo(ctx, w, r, repoID, "writeRepo")
	if !ok {
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	commitFileAndRespond(ctx, w, repoID, userID, token, f.Ref, f.Path, f.Content, f.Message)
}

// commitFileAndRespond validates a file edit, commits it (attributed to the caller's
// username when resolvable), and writes the JSON result. Shared by the PUT /blob and
// PATCH /repos/{id} entry points.
func commitFileAndRespond(ctx context.Context, w http.ResponseWriter, repoID, userID, token, ref, rawPath, content, message string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = headBranch(ctx, repoID)
	}
	if !branchNameRe.MatchString(ref) {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	path := strings.TrimPrefix(strings.TrimSpace(rawPath), "/")
	if path == "" || strings.Contains(path, "..") || path == ".git" || strings.HasPrefix(path, ".git/") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		msg = "Update " + path
	}
	author := userID
	if u, err := getUser(ctx, token, userID); err == nil && u.Username != "" {
		author = u.Username
	}
	sha, err := commitFileChange(ctx, repoID, ref, path, content, msg, author)
	if err != nil {
		if errors.Is(err, errNoChange) {
			http.Error(w, "no change — the file already has that content", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "write file", "Repo_id", repoID, "error", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commit": sha, "ref": ref, "path": path})
}

// handleGetReadme returns the repo's rendered-on-the-home-page readme, as raw markdown
// for the client to render. 200 with ok=false rather than 404 when there is none: "this
// repo has no readme" is a normal answer the UI shows as a hint, not a failure.
func handleGetReadme(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGetReadme")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getReadme")
	if !ok {
		return
	}

	ref := r.URL.Query().Get("ref")
	if ref != "" && !branchNameRe.MatchString(ref) {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}

	path, content, found, err := readme(ctx, re.ID, ref)
	if err != nil {
		slog.ErrorContext(ctx, "readme", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": found, "path": path, "content": content})
}

// ── refs ────────────────────────────────────────────────────────────────────

func handleListBranches(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListBranches")
	defer span.End()
	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listBranch")
	if !ok {
		return
	}
	refs, err := listRefs(ctx, re.ID, "refs/heads/")
	if err != nil {
		slog.ErrorContext(ctx, "list branches", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Mark the default so a picker can show it without a second call. HEAD is the
	// authority, not Repo.DefaultBranch: HEAD is what a clone actually checks out.
	head := headBranch(ctx, re.ID)
	for i := range refs {
		refs[i].Default = refs[i].Name == head
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": refs, "default": head})
}

func handleListTags(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListTags")
	defer span.End()
	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listTag")
	if !ok {
		return
	}
	refs, err := listRefs(ctx, re.ID, "refs/tags/")
	if err != nil {
		slog.ErrorContext(ctx, "list tags", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, refs)
}

// handleSetDefaultBranch repoints HEAD and the stored default together — they must not
// drift, or the API advertises one branch while a clone checks out another.
func handleSetDefaultBranch(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleSetDefaultBranch")
	defer span.End()

	id := r.PathValue("id")
	re, userID, ok := authorizeRepo(ctx, w, r, id, "setDefaultBranch")
	if !ok {
		return
	}
	var req struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := setHEAD(ctx, re.ID, strings.TrimSpace(req.DefaultBranch)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := connect().WithContext(ctx).Model(&Repo{}).
		Where("id = ? AND owner = ?", re.ID, userID).
		Updates(map[string]any{"default_branch": req.DefaultBranch, "updated_at": time.Now().UTC()}).Error; err != nil {
		slog.ErrorContext(ctx, "set default branch", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	updated, _ := getRepoByID(ctx, re.ID)
	writeJSON(w, http.StatusOK, updated)
}

// ── code browsing ───────────────────────────────────────────────────────────

// refAndPath pulls the ref and path query parameters, holding ref to the branch-name
// allowlist. path needs no allowlist: it is resolved by git as part of "ref:path"
// inside the object database, never joined onto the filesystem.
func refAndPath(r *http.Request) (ref, path string, ok bool) {
	ref = r.URL.Query().Get("ref")
	if ref != "" && !branchNameRe.MatchString(ref) {
		return "", "", false
	}
	return ref, strings.TrimPrefix(r.URL.Query().Get("path"), "/"), true
}

func handleTree(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleTree")
	defer span.End()
	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getTree")
	if !ok {
		return
	}
	ref, path, ok := refAndPath(r)
	if !ok {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	entries, err := listTree(ctx, re.ID, ref, path)
	if err != nil {
		if errors.Is(err, errPathNotFound) {
			http.Error(w, "path not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "list tree", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ref": ref, "path": path, "entries": entries})
}

func handleBlob(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleBlob")
	defer span.End()
	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getBlob")
	if !ok {
		return
	}
	ref, path, ok := refAndPath(r)
	if !ok {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	content, size, binary, err := readBlob(ctx, re.ID, ref, path)
	if err != nil {
		if errors.Is(err, errPathNotFound) {
			http.Error(w, "path not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(ctx, "read blob", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "size": size, "binary": binary,
		"truncated": size > maxBlobBytes, "content": content,
	})
}

// handleArchive streams a tar.gz of a ref. Like the git wire routes, it clears the
// server's write deadline: an archive of a real repo takes longer than the JSON API's
// 60s, and being cut off mid-stream reads as a network fault.
func handleArchive(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleArchive")
	defer span.End()
	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getArchive")
	if !ok {
		return
	}
	ref, _, ok := refAndPath(r)
	if !ok {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	name := ref
	if name == "" {
		name = "HEAD"
	}
	unboundedStream(w, r)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+re.Name+`-`+name+`.tar.gz"`)
	if err := writeArchive(ctx, re.ID, ref, re.Name, w); err != nil {
		// Headers are already sent, so the only honest signal left is to cut the stream.
		slog.ErrorContext(ctx, "archive failed mid-stream", "Repo_id", re.ID, "error", err)
	}
}
