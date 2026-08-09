package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// The rules split by WHERE they can be enforced, and the split is not cosmetic:
//
//   - NoForce / NoDelete are wire rules. They describe pushes, so the pre-receive hook
//     is the only place that can see them, and they must hold with the control plane
//     unreachable.
//   - RequireApprovals / RequireChecks / DismissStale are MERGE rules. They are facts
//     about review and CI state that live in the database, which a POSIX hook with no
//     network cannot consult. They are enforced in the merge handler (mergeGate).
//
// Naming that boundary matters, because the natural assumption is that "protected"
// means one thing everywhere. It does not: a merge rule constrains the API merge path
// only. Someone with push rights can still land the same commits with `git push`, and
// SHOULD be able to — the wire rules are what stop that being destructive. If you need
// a branch that only ever changes by reviewed merge, set a merge rule AND withhold the
// write action for that repo.
type BranchProtection struct {
	RepoID   string `gorm:"primaryKey" json:"repo_id"`
	Pattern  string `gorm:"primaryKey" json:"pattern"` // branch name or glob, e.g. "main" or "release/*"
	NoForce  bool   `json:"block_force_push"`
	NoDelete bool   `json:"block_deletion"`
	// RequireApprovals is how many distinct reviewers must currently approve before the
	// API will merge into a matching branch. 0 disables the rule.
	RequireApprovals int `json:"require_approvals"`
	// RequireChecks is a comma-separated list of status contexts that must be green on
	// the source head. Empty disables the rule. Stored flat rather than as a side table
	// because it is read whole, on one code path, and never queried across repos.
	RequireChecks string `json:"require_checks,omitempty"`
	// DismissStale ignores approvals given against an older source head, so a push
	// after sign-off re-opens review rather than riding the previous approval.
	DismissStale bool      `json:"dismiss_stale_approvals"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// requiredChecks splits the stored list. Blank entries are dropped so a trailing comma
// cannot create a required check named "" that no status can ever satisfy.
func (b BranchProtection) requiredChecks() []string {
	var out []string
	for _, c := range strings.Split(b.RequireChecks, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
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
		Pattern          string  `json:"pattern"`
		NoForce          *bool   `json:"block_force_push"`
		NoDelete         *bool   `json:"block_deletion"`
		RequireApprovals *int    `json:"require_approvals"`
		RequireChecks    *string `json:"require_checks"`
		DismissStale     *bool   `json:"dismiss_stale_approvals"`
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
	// The merge rules default OFF, unlike the wire rules: blocking force-push is what
	// "protected" has always meant here, but silently requiring approvals on an existing
	// rule would start rejecting merges that used to work.
	approvals := 0
	if req.RequireApprovals != nil {
		approvals = *req.RequireApprovals
	}
	if approvals < 0 {
		http.Error(w, "require_approvals must not be negative", http.StatusBadRequest)
		return
	}
	checks := ""
	if req.RequireChecks != nil {
		checks = strings.TrimSpace(*req.RequireChecks)
	}
	// Each context is compared verbatim against a posted status, so a malformed one
	// could never match and would block the branch permanently. Reject it instead.
	for _, c := range strings.Split(checks, ",") {
		if c = strings.TrimSpace(c); c != "" && !statusContextRe.MatchString(c) {
			http.Error(w, "require_checks entry "+c+" is not a valid status context", http.StatusBadRequest)
			return
		}
	}
	rule := BranchProtection{
		RepoID: re.ID, Pattern: req.Pattern,
		NoForce:          req.NoForce == nil || *req.NoForce, // protecting means blocking force by default
		NoDelete:         req.NoDelete == nil || *req.NoDelete,
		RequireApprovals: approvals,
		RequireChecks:    checks,
		DismissStale:     req.DismissStale != nil && *req.DismissStale,
		CreatedAt:        time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := connect().WithContext(ctx).
		Where("repo_id = ? AND pattern = ?", re.ID, req.Pattern).
		Assign(map[string]any{
			"no_force": rule.NoForce, "no_delete": rule.NoDelete,
			"require_approvals": rule.RequireApprovals, "require_checks": rule.RequireChecks,
			"dismiss_stale": rule.DismissStale, "updated_at": rule.UpdatedAt,
		}).
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

// protectionsForBranch returns the rules whose pattern matches branch. Several may
// match ("main" and "*"), and they are combined by taking the STRICTEST of each rule
// rather than letting the first or last win — a broad rule must never weaken a specific
// one just by also matching.
func protectionsForBranch(ctx context.Context, repoID, branch string) (BranchProtection, error) {
	rules, err := protectionsFor(ctx, repoID)
	if err != nil {
		return BranchProtection{}, err
	}
	var eff BranchProtection
	for _, r := range rules {
		ok, mErr := filepath.Match(r.Pattern, branch)
		if mErr != nil || !ok {
			continue
		}
		eff.NoForce = eff.NoForce || r.NoForce
		eff.NoDelete = eff.NoDelete || r.NoDelete
		eff.DismissStale = eff.DismissStale || r.DismissStale
		if r.RequireApprovals > eff.RequireApprovals {
			eff.RequireApprovals = r.RequireApprovals
		}
		for _, c := range r.requiredChecks() {
			if !strings.Contains(","+eff.RequireChecks+",", ","+c+",") {
				if eff.RequireChecks != "" {
					eff.RequireChecks += ","
				}
				eff.RequireChecks += c
			}
		}
	}
	return eff, nil
}

// errMergeBlocked is a policy refusal, distinct from a git failure: the merge did not
// happen because it was not allowed to, not because it could not be computed.
type errMergeBlocked struct{ reason string }

func (e errMergeBlocked) Error() string { return e.reason }

// mergeGate decides whether pr may be merged, per the protections on its TARGET branch.
//
// This is the check that was missing. Protections were enforced only by the pre-receive
// hook, which the API merge path never touches — so a branch marked protected could be
// changed through /pulls/{n}/merge without consulting its rules at all. Every rule below
// therefore fails CLOSED: an error reading review or status state blocks the merge
// rather than waving it through, because a gate that opens when it cannot see is not a
// gate.
func mergeGate(ctx context.Context, re Repo, pr PullRequest) error {
	rule, err := protectionsForBranch(ctx, re.ID, pr.TargetRef)
	if err != nil {
		return errMergeBlocked{"could not read branch protection rules"}
	}
	if rule.RequireApprovals == 0 && rule.RequireChecks == "" {
		return nil
	}

	head := refSHA(ctx, re.ID, pr.localSourceRef())

	if rule.RequireApprovals > 0 {
		reviews, err := latestReviews(ctx, pr.ID)
		if err != nil {
			return errMergeBlocked{"could not read reviews"}
		}
		approvals := 0
		for _, rv := range reviews {
			// A standing "changes requested" blocks regardless of the approval count:
			// three approvals do not out-vote an unresolved objection.
			if rv.State == reviewChangesRequested {
				return errMergeBlocked{"a reviewer has requested changes"}
			}
			if rv.State != reviewApproved {
				continue
			}
			// A stale approval is one given against a head that has since moved. Only
			// discounted when the rule asks for it, since re-approving every push is a
			// real cost and not every repo wants to pay it.
			if rule.DismissStale && head != "" && rv.CommitSHA != head {
				continue
			}
			approvals++
		}
		if approvals < rule.RequireApprovals {
			return errMergeBlocked{fmt.Sprintf(
				"%d of %d required approvals", approvals, rule.RequireApprovals)}
		}
	}

	if req := rule.requiredChecks(); len(req) > 0 {
		if head == "" {
			return errMergeBlocked{"could not resolve the source branch head to check status"}
		}
		sts, err := statusesFor(ctx, re.ID, head)
		if err != nil {
			return errMergeBlocked{"could not read commit statuses"}
		}
		state := map[string]string{}
		for _, s := range sts {
			state[s.Context] = s.State
		}
		for _, c := range req {
			switch state[c] {
			case statusSuccess:
				// green
			case "":
				return errMergeBlocked{"required check " + c + " has not reported"}
			default:
				return errMergeBlocked{"required check " + c + " is " + state[c]}
			}
		}
	}
	return nil
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
