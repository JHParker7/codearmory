package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Forks.
//
// Without them a pull request could only ever go branch → branch INSIDE one repo, which
// means every contributor needs write access to the thing they are contributing to.
// That rules out the entire outside-contributor flow: to propose a change you must first
// be trusted enough not to need to.
//
// A fork is a full repo record of its own — its own id, owner, namespace and grants —
// that remembers where it came from (ForkOf). It is NOT a mirror: a mirror is read-only
// and refreshes from upstream, whereas a fork is yours to push to and diverge from. The
// two share the "copy of someone else's repo" shape and nothing else, which is why Kind
// stays "native" here.
//
// The object copy is a local `git clone --bare`. On one node that is cheap and the
// obvious thing; when the git plane is sharded the source may live elsewhere, so this
// goes through the same placement check every other write does and refuses rather than
// silently forking an empty repo.

// handleForkRepo creates a fork of a repo the caller can read, owned by the caller.
func handleForkRepo(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleForkRepo")
	defer span.End()

	// Authorised as a READ of the source: being able to see a repo is what entitles you
	// to fork it. The write that follows lands in the caller's own namespace, which
	// their createRepo grant covers.
	src, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getRepo")
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"` // optional: defaults to the source's name
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = src.Name
	}
	if !validRepoName(name) {
		http.Error(w, "repo name is invalid", http.StatusBadRequest)
		return
	}

	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	user, err := getUser(ctx, token, userID)
	if err != nil {
		http.Error(w, "failed to get the user name", http.StatusBadRequest)
		return
	}
	namespace := user.Username

	// Forking your own repo into the same namespace would collide on (namespace, name)
	// and is almost always a mistake rather than an intent to rename.
	if namespace == src.Namespace && name == src.Name {
		http.Error(w, "a fork needs a different name in your namespace", http.StatusConflict)
		return
	}

	fork := Repo{
		ID:        uuid.New().String(),
		Owner:     userID,
		Namespace: namespace,
		Name:      name,
		// Carried over so the fork reads like the thing it came from.
		Description:   src.Description,
		DefaultBranch: src.DefaultBranch,
		// A fork of a public repo starts PRIVATE. Inheriting visibility would publish a
		// copy of someone's code under a new owner as a side effect of a button press;
		// making it public is then a deliberate act by the new owner.
		Visibility: visibilityPrivate,
		Kind:       kindNative,
		ForkOf:     src.ID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	if err := fork.Add(ctx); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "a repo with this name already exists in your namespace", http.StatusConflict)
			return
		}
		slog.ErrorContext(ctx, "fork: create row", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := copyRepoObjects(ctx, src.ID, fork.ID); err != nil {
		// Roll the row back rather than leaving a fork whose bytes never arrived: an
		// empty repo that claims to be a fork is worse than a failed request, because
		// the next push would make it look like a legitimately divergent one.
		if derr := connect().WithContext(ctx).Delete(&Repo{}, "id = ?", fork.ID).Error; derr != nil {
			slog.ErrorContext(ctx, "fork: rollback failed", "Repo_id", fork.ID, "error", derr)
		}
		slog.ErrorContext(ctx, "fork: copy objects", "source", src.ID, "error", err)
		http.Error(w, "could not copy the repository", http.StatusInternalServerError)
		return
	}

	// Protections are deliberately NOT copied: they are the source owner's policy, and
	// silently applying them to someone else's repo would be surprising in both
	// directions. The hook file is still written so the fork has the machinery in place.
	if err := syncProtection(ctx, fork.ID); err != nil {
		slog.WarnContext(ctx, "fork: could not initialise protection hook", "Repo_id", fork.ID, "error", err)
	}

	out, err := getRepo(ctx, fork.Owner, fork.ID)
	if err != nil {
		slog.ErrorContext(ctx, "fork: read back", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	meterReposCreated.Add(ctx, 1)
	span.SetAttributes(attribute.String("Repo.id", fork.ID), attribute.String("Repo.fork_of", src.ID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "repo forked", "source", src.ID, "fork", fork.ID, "user_id", userID)
	auditEvent(ctx, userID, auditActionRepoCreate, fork.ID, auditDetail(out, "fork_of="+src.ID))
	writeJSON(w, http.StatusCreated, out)
}

// copyRepoObjects clones src's bare repository into dst's path.
//
// `clone --bare` rather than a file copy: it is what produces a consistent snapshot of
// the object database plus refs without needing to know git's on-disk layout, and it
// skips the source's hooks — which matters here, because the source's pre-receive hook
// encodes the SOURCE owner's branch protection and must not be inherited.
func copyRepoObjects(ctx context.Context, srcID, dstID string) error {
	srcDir, err := localDirFor(ctx, srcID)
	if err != nil {
		return err
	}
	dstDir, err := repoDiskPath(dstID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	// Clone into a path that must not already exist, so remove the empty dir we just
	// ensured the parent of. git refuses to clone into a non-empty directory.
	if err := os.RemoveAll(dstDir); err != nil {
		return err
	}
	// "--" before the paths for the same reason mirror.go passes it: these are argv
	// elements, and a value beginning with a dash would otherwise be read as an option.
	out, err := exec.CommandContext(ctx, gitBinary, "clone", "--bare", "--no-hardlinks",
		"--", srcDir, dstDir).CombinedOutput()
	if err != nil {
		return &forkCopyError{out: strings.TrimSpace(string(out)), err: err}
	}
	return nil
}

type forkCopyError struct {
	out string
	err error
}

func (e *forkCopyError) Error() string { return "clone --bare: " + e.err.Error() + ": " + e.out }
func (e *forkCopyError) Unwrap() error { return e.err }

// handleListForks lists the forks of a repo. Only those the caller may see: a fork is a
// private repo of its owner's until they publish it, so listing them all would leak both
// the existence of the fork and who made it.
func handleListForks(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleListForks")
	defer span.End()

	src, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "getRepo")
	if !ok {
		return
	}
	var forks []Repo
	if err := connectRead().WithContext(ctx).
		Where("fork_of = ?", src.ID).
		Where("visibility = ? OR owner = ?", visibilityPublic, userID).
		Order("created_at").Find(&forks).Error; err != nil {
		slog.ErrorContext(ctx, "list forks", "Repo_id", src.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if forks == nil {
		forks = []Repo{}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, forks)
}

// fetchCrossRepo brings a fork's branch into the target repo under a scratch ref, so a
// cross-repo pull request can be diffed and merged with the ordinary single-repo
// machinery.
//
// This is what makes a fork PR work without a second code path: git already merges refs
// within one object database, so the whole job is getting the source commits into it.
// The scratch ref lives under refs/codearmory/pulls/, outside refs/heads/, so it never
// shows up as a branch, is never advertised to a clone, and cannot collide with a real
// branch name.
func crossRepoRef(pullID string) string { return "refs/codearmory/pulls/" + pullID }

func fetchCrossRepo(ctx context.Context, targetRepoID, sourceRepoID, sourceBranch, pullID string) error {
	srcDir, err := localDirFor(ctx, sourceRepoID)
	if err != nil {
		return err
	}
	dstDir, err := localDirFor(ctx, targetRepoID)
	if err != nil {
		return err
	}
	if !branchNameRe.MatchString(sourceBranch) {
		return errUnsafeFetchURL
	}
	out, err := exec.CommandContext(ctx, gitBinary, "-C", dstDir, "fetch", "--force",
		"--", srcDir, "refs/heads/"+sourceBranch+":"+crossRepoRef(pullID)).CombinedOutput()
	if err != nil {
		return &forkCopyError{out: strings.TrimSpace(string(out)), err: err}
	}
	return nil
}
