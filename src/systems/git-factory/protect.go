package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// Branch protection.
//
// receive-pack accepts whatever a writer sends, including a force-push that rewrites
// history and a deletion of the default branch. Both destroy work irrecoverably from
// the client's point of view, and neither is distinguishable from an ordinary push
// until it has already happened.
//
// Enforcement is a git pre-receive HOOK rather than a check in the HTTP handler. The
// ref updates a push carries live in the request body, which is streamed straight into
// git — reading them in the handler would mean buffering the whole packfile to inspect
// a few lines. git already parses them and hands them to the hook, and a non-zero exit
// rejects the push atomically, before any ref moves.
//
// The rules are written next to the hook as a flat file so the hook stays a small
// POSIX script with no JSON parser and no callback into this service: a push must not
// depend on the control plane being reachable.

type BranchProtection struct {
	RepoID   string `gorm:"primaryKey" json:"repo_id"`
	Pattern  string `gorm:"primaryKey" json:"pattern"` // branch name or glob, e.g. "main" or "release/*"
	NoForce  bool   `json:"block_force_push"`
	NoDelete bool   `json:"block_deletion"`
	// RequireReview gates the MERGE api (not the push hook — a merge is the only write
	// that lands on a protected branch through a review): a PR into a matching branch
	// needs an approving review and no outstanding changes-requested. Off by default,
	// so an existing repo's merges are unchanged until it is turned on.
	RequireReview bool      `json:"require_review"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// preReceiveHook enforces the rules file. Kept deliberately small and dependency-free.
//
// git feeds it "<old-sha> <new-sha> <ref>" per updated ref on stdin. A deletion has an
// all-zero new sha; a non-fast-forward is one where old is NOT an ancestor of new,
// which is exactly what makes a force-push destructive.
const preReceiveHook = `#!/bin/sh
# Managed by codearmory git_factory. Rules live in codearmory-protection.
# Format: <pattern> <noforce:0|1> <nodelete:0|1>
RULES="$(dirname "$0")/../codearmory-protection"
[ -f "$RULES" ] || exit 0
ZERO="0000000000000000000000000000000000000000"
status=0
while read -r old new ref; do
  case "$ref" in refs/heads/*) ;; *) continue ;; esac
  branch=${ref#refs/heads/}
  while read -r pattern noforce nodelete; do
    [ -n "$pattern" ] || continue
    # shellcheck disable=SC2254 -- the pattern is a glob on purpose
    case "$branch" in
      $pattern) ;;
      *) continue ;;
    esac
    if [ "$new" = "$ZERO" ] && [ "$nodelete" = "1" ]; then
      echo "remote: branch '$branch' is protected: deletion is not allowed" >&2
      status=1
      continue
    fi
    if [ "$old" != "$ZERO" ] && [ "$new" != "$ZERO" ] && [ "$noforce" = "1" ]; then
      if ! git merge-base --is-ancestor "$old" "$new" 2>/dev/null; then
        echo "remote: branch '$branch' is protected: force-push would discard commits" >&2
        status=1
      fi
    fi
  done < "$RULES"
done
exit $status
`

// writeProtection materialises the hook and the rules file into the bare repo. Called
// whenever rules change, and on repo creation so the hook is always present (an empty
// rules file means "nothing protected", which the hook exits on immediately).
func writeProtection(ctx context.Context, repoID string, rules []BranchProtection) error {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range rules {
		b.WriteString(r.Pattern + " " + boolBit(r.NoForce) + " " + boolBit(r.NoDelete) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "codearmory-protection"), []byte(b.String()), 0o640); err != nil {
		return err
	}
	hookPath := filepath.Join(dir, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o750); err != nil {
		return err
	}
	// 0750: git runs it, nobody else needs to.
	return os.WriteFile(hookPath, []byte(preReceiveHook), 0o750)
}

func boolBit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func protectionsFor(ctx context.Context, repoID string) ([]BranchProtection, error) {
	var rules []BranchProtection
	err := connectRead().WithContext(ctx).Where("repo_id = ?", repoID).Order("pattern").Find(&rules).Error
	if rules == nil {
		rules = []BranchProtection{}
	}
	return rules, err
}

func handleListProtections(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListProtections")
	defer span.End()
	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listProtection")
	if !ok {
		return
	}
	rules, err := protectionsFor(ctx, re.ID)
	if err != nil {
		slog.ErrorContext(ctx, "list protections", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

// handleSetProtection creates or updates a rule, then rewrites the hook so the change
// takes effect on the very next push.
func handleSetProtection(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleSetProtection")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "setProtection")
	if !ok {
		return
	}
	var req struct {
		Pattern       string `json:"pattern"`
		NoForce       *bool  `json:"block_force_push"`
		NoDelete      *bool  `json:"block_deletion"`
		RequireReview *bool  `json:"require_review"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Pattern = strings.TrimSpace(req.Pattern)
	// The pattern reaches a shell `case` as a glob, so it is held to the branch
	// allowlist plus "*" — no spaces, quotes or expansions can survive that.
	if req.Pattern == "" || !protectionPatternRe.MatchString(req.Pattern) {
		http.Error(w, "pattern must be a branch name or glob", http.StatusBadRequest)
		return
	}
	rule := BranchProtection{
		RepoID: re.ID, Pattern: req.Pattern,
		NoForce:       req.NoForce == nil || *req.NoForce, // protecting means blocking force by default
		NoDelete:      req.NoDelete == nil || *req.NoDelete,
		RequireReview: req.RequireReview != nil && *req.RequireReview, // off unless asked
		CreatedAt:     time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).
		Where("repo_id = ? AND pattern = ?", re.ID, req.Pattern).
		Assign(map[string]any{"no_force": rule.NoForce, "no_delete": rule.NoDelete, "require_review": rule.RequireReview, "updated_at": rule.UpdatedAt}).
		FirstOrCreate(&rule).Error; err != nil {
		slog.ErrorContext(ctx, "set protection", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := syncProtection(ctx, re.ID); err != nil {
		slog.ErrorContext(ctx, "set protection: sync hook", "Repo_id", re.ID, "error", err)
		http.Error(w, "the rule was saved but could not be applied", http.StatusInternalServerError)
		return
	}
	slog.InfoContext(ctx, "branch protection set", "Repo_id", re.ID, "pattern", req.Pattern)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, rule)
}

func handleDeleteProtection(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleDeleteProtection")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "setProtection")
	if !ok {
		return
	}
	pattern := r.PathValue("pattern")
	if err := connect().WithContext(ctx).
		Where("repo_id = ? AND pattern = ?", re.ID, pattern).
		Delete(&BranchProtection{}).Error; err != nil {
		slog.ErrorContext(ctx, "delete protection", "Repo_id", re.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := syncProtection(ctx, re.ID); err != nil {
		slog.ErrorContext(ctx, "delete protection: sync hook", "Repo_id", re.ID, "error", err)
		http.Error(w, "the rule was removed but the hook could not be updated", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// syncProtection rewrites the on-disk hook from the stored rules. The database is the
// source of truth; the file is a projection of it, rebuilt whole rather than patched.
func syncProtection(ctx context.Context, repoID string) error {
	rules, err := protectionsFor(ctx, repoID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Pattern < rules[j].Pattern })
	return writeProtection(ctx, repoID, rules)
}
