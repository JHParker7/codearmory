package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Code search within a repo.
//
// `git grep` against a ref, rather than an index. An index would be faster on a large
// store and is the right answer eventually, but it is also a whole subsystem —
// build, invalidate, reconcile after a force-push — and an index that silently drifts
// from the repo returns confidently wrong results. Asking git means the answer is always
// exactly what the ref contains.
//
// The pattern is passed as a FIXED STRING (-F), never a regex. Two reasons, and the
// second is the important one: a user searching for "foo(bar)" means those characters,
// and an unbounded regex from an unauthenticated-ish surface is a cost the server pays
// on someone else's behalf.

// searchMaxResults bounds one response. A search that matches everything must not try to
// serialise the whole repo.
const searchMaxResults = 200

func handleSearchCode(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleSearchCode")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getBlob")
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	if len(q) > 512 {
		http.Error(w, "q is too long", http.StatusBadRequest)
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
	dir, err := localDirFor(ctx, re.ID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !hasCommits(ctx, dir, ref) {
		writeJSON(w, http.StatusOK, map[string]any{"ref": ref, "results": []any{}, "truncated": false})
		return
	}

	// -I skips binary files (a match inside a PNG is noise), -n numbers lines, and
	// "--" ends option parsing so a query beginning with a dash is a query and not a
	// flag. -e names the pattern explicitly for the same reason.
	args := []string{"-C", dir, "grep", "-I", "-n", "--no-color"}
	if strings.EqualFold(r.URL.Query().Get("ignore_case"), "true") {
		args = append(args, "-i")
	}
	args = append(args, "-F", "-e", q, ref, "--")
	if p := strings.TrimSpace(r.URL.Query().Get("path")); p != "" {
		// A pathspec is data to git, not a shell glob, and it follows "--" so it can
		// never be read as an option.
		args = append(args, p)
	}

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// git grep exits 1 for "no matches", which is not an error.
		if ee, okExit := err.(*exec.ExitError); !okExit || ee.ExitCode() != 1 {
			slog.WarnContext(ctx, "code search failed", "Repo_id", re.ID,
				"error", err, "stderr", strings.TrimSpace(stderr.String()))
			http.Error(w, "search failed", http.StatusInternalServerError)
			return
		}
	}

	type hit struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	results := []hit{}
	truncated := false
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if len(results) >= searchMaxResults {
			truncated = true
			break
		}
		// "<ref>:<path>:<line>:<text>" — split from the left exactly three times, since
		// the text itself may contain colons.
		rest, okCut := strings.CutPrefix(line, ref+":")
		if !okCut {
			continue
		}
		p, after, okCut := strings.Cut(rest, ":")
		if !okCut {
			continue
		}
		numStr, text, okCut := strings.Cut(after, ":")
		if !okCut {
			continue
		}
		n, convErr := strconv.Atoi(numStr)
		if convErr != nil {
			continue
		}
		// Long lines are truncated rather than dropped: a minified file should not be
		// able to put a megabyte into a search response, but it should still be findable.
		if len(text) > 512 {
			text = text[:512]
		}
		results = append(results, hit{Path: p, Line: n, Text: text})
	}

	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":     ref,
		"query":   q,
		"results": results,
		// Reported rather than silently applied, so a caller can tell "200 results" from
		// "at least 200 results".
		"truncated": truncated,
	})
}
