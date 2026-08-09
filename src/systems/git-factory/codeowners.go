package main

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// CODEOWNERS.
//
// A file in the repo mapping path patterns to the people responsible for them. It is
// read from git rather than stored in the database for the same reason a release is: it
// belongs to a commit, so it must travel with branches, diffs and history, and a copy in
// a table would immediately disagree with the branch being reviewed.
//
// What this deliberately does NOT do is enforce anything. Turning "these people own this
// path" into "these people must approve" needs an identity mapping from a CODEOWNERS
// entry (which is text — a username, an email, a team) to a gatekeeper user_id, and
// guessing that mapping wrong fails OPEN: a required owner who never resolves is a
// requirement silently satisfied by nobody. So this reports owners, and the merge gate
// continues to count approvals it can verify. See ownersFor.

// codeownersPaths are the locations checked, in order — the same set every other
// platform accepts, so a repo moved here keeps working.
var codeownersPaths = []string{"CODEOWNERS", ".codearmory/CODEOWNERS", "docs/CODEOWNERS"}

// codeownersRule is one line: a path pattern and the owners it assigns.
type codeownersRule struct {
	Pattern string   `json:"pattern"`
	Owners  []string `json:"owners"`
}

// parseCodeowners reads the file's text into rules. Comments and blank lines are
// skipped; everything else is "<pattern> <owner>...".
//
// Later rules win, which is the convention the format has everywhere: the file reads
// top-to-bottom from general to specific, so the LAST match is the most specific
// statement about a path.
func parseCodeowners(content string) []codeownersRule {
	var rules []codeownersRule
	for _, line := range strings.Split(content, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue // a pattern with no owners assigns nobody; skip rather than record
		}
		rules = append(rules, codeownersRule{Pattern: fields[0], Owners: fields[1:]})
	}
	return rules
}

// matchCodeowners returns the owners for a path — the LAST matching rule's, or none.
func matchCodeowners(rules []codeownersRule, filePath string) []string {
	var owners []string
	for _, r := range rules {
		if codeownersMatch(r.Pattern, filePath) {
			owners = r.Owners
		}
	}
	return owners
}

// codeownersMatch implements the subset of the pattern syntax that is unambiguous.
//
// Deliberately not a full gitignore implementation: the exotic corners of that syntax
// (negation, "**" in the middle, character classes) are where a subtly wrong match sends
// a review to the wrong people, which is worse than not matching. Supported: a leading
// "/" anchors to the repo root, a trailing "/" matches a directory and everything under
// it, "*" matches within one segment, and a bare name matches at any depth.
func codeownersMatch(pattern, filePath string) bool {
	filePath = strings.TrimPrefix(filePath, "/")
	if pattern == "*" || pattern == "**" {
		return true
	}
	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")

	if dirOnly {
		// "src/" covers everything beneath it.
		if strings.HasPrefix(filePath, pattern+"/") {
			return true
		}
		if !anchored {
			// Unanchored, so it may match at any depth: "docs/" matches "a/docs/x".
			return strings.Contains("/"+filePath, "/"+pattern+"/")
		}
		return false
	}
	if ok, _ := path.Match(pattern, filePath); ok {
		return true
	}
	// A directory prefix match: "src" also owns "src/main.go".
	if strings.HasPrefix(filePath, pattern+"/") {
		return true
	}
	if !anchored {
		// Unanchored patterns match on any segment boundary, so "*.go" matches
		// "pkg/a.go" and "Makefile" matches "sub/Makefile".
		base := filePath
		if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
			base = filePath[i+1:]
		}
		if ok, _ := path.Match(pattern, base); ok {
			return true
		}
		if strings.Contains("/"+filePath, "/"+pattern+"/") {
			return true
		}
	}
	return false
}

// codeownersFor loads and parses the repo's CODEOWNERS at a ref, if it has one.
func codeownersFor(ctx context.Context, repoID, ref string) ([]codeownersRule, string, bool) {
	for _, p := range codeownersPaths {
		content, _, binary, err := readBlob(ctx, repoID, ref, p)
		if err != nil || binary {
			continue
		}
		return parseCodeowners(content), p, true
	}
	return nil, "", false
}

// handleGetCodeowners returns the parsed rules, and — when asked about a specific PR —
// who owns the files it changes.
func handleGetCodeowners(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGetCodeowners")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getBlob")
	if !ok {
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = headBranch(ctx, re.ID)
	}
	if !branchNameRe.MatchString(ref) {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	rules, from, found := codeownersFor(ctx, re.ID, ref)
	out := map[string]any{"found": found, "rules": rules}
	if rules == nil {
		out["rules"] = []codeownersRule{}
	}
	if found {
		out["path"] = from
	}

	// ?pull=<number> answers the question actually worth asking: who owns what this
	// change touches.
	if num := strings.TrimSpace(r.URL.Query().Get("pull")); num != "" && found {
		pr, err := loadPull(ctx, re.ID, num)
		if err != nil {
			http.Error(w, "pull request not found", http.StatusNotFound)
			return
		}
		changes, err := diffStat(ctx, re.ID, pr.TargetRef, pr.localSourceRef())
		if err != nil {
			slog.WarnContext(ctx, "codeowners: diffstat", "Repo_id", re.ID, "error", err)
		} else {
			out["owners"] = ownersFor(rules, changes)
		}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, out)
}

// ownersFor maps a changeset to the distinct owners responsible for it, sorted so the
// answer is stable between calls.
func ownersFor(rules []codeownersRule, changes []fileChange) []string {
	seen := map[string]bool{}
	for _, c := range changes {
		for _, o := range matchCodeowners(rules, c.Path) {
			seen[o] = true
		}
	}
	out := make([]string, 0, len(seen))
	for o := range seen {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}
