package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// Tag and release management.
//
// Tags were listable but not creatable, which meant the one thing a tag is FOR — marking
// a release from the platform that builds it — had to be done by pushing from a
// workstation. A pipeline that builds and publishes could not also tag what it published.
//
// A release is a tag plus notes. It is stored in git rather than the database: an
// annotated tag already carries a message, an author and a date, so a separate releases
// table would duplicate all three and then disagree with them the moment someone pushed
// a tag directly. Reading a release means reading the tag object.

// tagNameRe is the allowlist for a tag name. It becomes a git argv element and a ref
// path, so it is held to the same shape as a branch.
var tagNameRe = branchNameRe

func handleCreateTag(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleCreateTag")
	defer span.End()

	// Creating a tag writes to the repository — it is not a metadata edit.
	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "createTag")
	if !ok {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Ref     string `json:"ref"`     // what to tag; defaults to the default branch
		Message string `json:"message"` // present ⇒ an ANNOTATED tag (a release)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Ref = strings.TrimSpace(req.Ref)
	if !tagNameRe.MatchString(req.Name) {
		http.Error(w, "tag name is invalid", http.StatusBadRequest)
		return
	}
	if req.Ref == "" {
		req.Ref = headBranch(ctx, re.ID)
	}
	if !branchNameRe.MatchString(req.Ref) {
		http.Error(w, "ref is invalid", http.StatusBadRequest)
		return
	}
	if tagExists(ctx, re.ID, req.Name) {
		// Tags are immutable by convention and by expectation: something already built
		// against v1.0 must keep meaning the same commit.
		http.Error(w, "tag "+req.Name+" already exists", http.StatusConflict)
		return
	}
	if err := createTag(ctx, re.ID, req.Name, req.Ref, req.Message, userID); err != nil {
		slog.ErrorContext(ctx, "create tag", "Repo_id", re.ID, "tag", req.Name, "error", err)
		http.Error(w, "could not create the tag", http.StatusConflict)
		return
	}
	_ = touchRepo(ctx, re.ID)
	slog.InfoContext(ctx, "tag created", "Repo_id", re.ID, "tag", req.Name, "ref", req.Ref, "user_id", userID)
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":       req.Name,
		"ref":        req.Ref,
		"sha":        refSHA(ctx, re.ID, "refs/tags/"+req.Name),
		"annotated":  req.Message != "",
		"message":    req.Message,
		"created_at": time.Now().UTC(),
	})
}

func handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleDeleteTag")
	defer span.End()

	re, userID, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "deleteTag")
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !tagNameRe.MatchString(name) {
		http.Error(w, "tag name is invalid", http.StatusBadRequest)
		return
	}
	if !tagExists(ctx, re.ID, name) {
		http.Error(w, "tag not found", http.StatusNotFound)
		return
	}
	dir, err := localDirFor(ctx, re.ID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if out, err := exec.CommandContext(ctx, gitBinary, "-C", dir,
		"update-ref", "-d", "refs/tags/"+name).CombinedOutput(); err != nil {
		slog.ErrorContext(ctx, "delete tag", "Repo_id", re.ID, "tag", name,
			"error", err, "output", strings.TrimSpace(string(out)))
		http.Error(w, "could not delete the tag", http.StatusInternalServerError)
		return
	}
	_ = touchRepo(ctx, re.ID)
	slog.InfoContext(ctx, "tag deleted", "Repo_id", re.ID, "tag", name, "user_id", userID)
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleGetTag returns one tag, including the annotation when it has one — which is
// what makes it a release rather than just a pointer.
func handleGetTag(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(serviceName).Start(r.Context(), "handleGetTag")
	defer span.End()

	re, _, ok := authorizeRepo(ctx, w, r, r.PathValue("id"), "listTag")
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !tagNameRe.MatchString(name) {
		http.Error(w, "tag name is invalid", http.StatusBadRequest)
		return
	}
	if !tagExists(ctx, re.ID, name) {
		http.Error(w, "tag not found", http.StatusNotFound)
		return
	}
	dir, err := localDirFor(ctx, re.ID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	out := map[string]any{
		"name": name,
		"sha":  refSHA(ctx, re.ID, "refs/tags/"+name),
	}
	// cat-file -t distinguishes an annotated tag (its own object) from a lightweight
	// one (a ref straight at a commit). Only the former carries a message.
	var typ bytes.Buffer
	c := exec.CommandContext(ctx, gitBinary, "-C", dir, "cat-file", "-t", "refs/tags/"+name)
	c.Stdout = &typ
	if c.Run() == nil && strings.TrimSpace(typ.String()) == "tag" {
		out["annotated"] = true
		var msg bytes.Buffer
		m := exec.CommandContext(ctx, gitBinary, "-C", dir, "tag", "-l", "--format=%(contents)", name)
		m.Stdout = &msg
		if m.Run() == nil {
			out["message"] = strings.TrimRight(msg.String(), "\n")
		}
		var tagger bytes.Buffer
		tg := exec.CommandContext(ctx, gitBinary, "-C", dir, "tag", "-l", "--format=%(taggername)|%(taggerdate:iso-strict)", name)
		tg.Stdout = &tagger
		if tg.Run() == nil {
			if who, when, found := strings.Cut(strings.TrimSpace(tagger.String()), "|"); found {
				out["tagger"], out["tagged_at"] = who, when
			}
		}
	} else {
		out["annotated"] = false
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, out)
}

// tagExists reports whether a tag ref resolves.
func tagExists(ctx context.Context, repoID, name string) bool {
	dir, err := localDirFor(ctx, repoID)
	if err != nil || !tagNameRe.MatchString(name) {
		return false
	}
	return exec.CommandContext(ctx, gitBinary, "-C", dir,
		"show-ref", "--verify", "--quiet", "refs/tags/"+name).Run() == nil
}

// createTag writes the tag. An annotated tag needs an identity for the tagger, which is
// supplied per-invocation rather than configured globally — the repo's git config is
// shared by every caller, so writing the acting user into it would be a race.
func createTag(ctx context.Context, repoID, name, ref, message, userID string) error {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return err
	}
	args := []string{"-C", dir, "tag"}
	if message != "" {
		args = append(args, "-m", message)
	}
	// "--" so a name or ref beginning with a dash cannot be read as an option.
	args = append(args, "--", name, ref)
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	if message != "" {
		who := userID
		if who == "" {
			who = "codearmory"
		}
		cmd.Env = append(cmd.Environ(),
			"GIT_COMMITTER_NAME="+who, "GIT_COMMITTER_EMAIL="+who+"@codearmory",
			"GIT_AUTHOR_NAME="+who, "GIT_AUTHOR_EMAIL="+who+"@codearmory")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return &forkCopyError{out: strings.TrimSpace(string(out)), err: err}
	}
	return nil
}
