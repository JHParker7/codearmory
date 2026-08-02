package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// repoDiskPath builds the on-disk location of a repo's bare repository.
//
// The path derives from the stable id ONLY — never the namespace or name — so a
// rename stays metadata-only (ARCHITECTURE §4) and no user-supplied string is
// joined into a filesystem path (§6).
//
// The id is required to be a *canonical* uuid rather than merely uuid-parseable:
// it reaches here from the URL on the delete path, and this check is what keeps a
// crafted id out of a recursive remove. Canonical form also guarantees id[:2] is
// two hex characters, so the shard slice cannot panic on a short id.
func repoDiskPath(id string) (string, error) {
	if u, err := uuid.Parse(id); err != nil || u.String() != id {
		return "", fmt.Errorf("refusing to build a path from non-canonical repo id %q", id)
	}
	root := secretOrDefault("GIT_STORAGE_ROOT", "temp/repos")
	return filepath.Join(root, shardOf(id), id+".git"), nil
}

// defaultBranchName is the branch a new repo's HEAD points at. It mirrors the
// `gorm:"default:main"` column default on Repo.DefaultBranch — the two must agree, or
// the advertised branch and the on-disk HEAD diverge.
const defaultBranchName = "main"

// branchNameRe is the allowlist for a value that becomes a git argv element. Today
// DefaultBranch is never client-supplied, so this is belt-and-braces; it is here so
// that making the branch settable later cannot turn into argument injection.
var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// initialBranch resolves the branch to initialize HEAD at, falling back to the default
// for an unset or unacceptable value.
func initialBranch(re Repo) string {
	if b := strings.TrimSpace(re.DefaultBranch); branchNameRe.MatchString(b) {
		return b
	}
	return defaultBranchName
}

// CreateGitRepo initializes a bare repository at the repo's id-derived path.
func CreateGitRepo(ctx context.Context, re Repo) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "git.CreateGitRepo")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", re.ID))

	gitPath, err := repoDiskPath(re.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "create git repo: bad id", "Repo_id", re.ID, "error", err)
		return err
	}

	// 0750, not 0777: repository contents are private to the service account.
	if err := os.MkdirAll(gitPath, 0o750); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "create git repo: mkdir", "path", gitPath, "error", err)
		return err
	}
	// --initial-branch matters: without it git picks its own default (master) for HEAD,
	// while the API advertises default_branch. A client that pushes to the advertised
	// branch then clones gets "remote HEAD refers to nonexistent ref, unable to
	// checkout" and an empty working tree — a working push that looks like data loss.
	if err := exec.CommandContext(ctx, gitBinary, "-C", gitPath, "init", "--bare",
		"--initial-branch="+initialBranch(re)).Run(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "create git repo: git init", "path", gitPath, "error", err)
		return err
	}

	slog.InfoContext(ctx, "git repo created", "path", gitPath)
	span.SetStatus(codes.Ok, "")
	return nil
}

// DeleteGitRepo removes a repo's bare repository from disk. It is idempotent: a
// path that does not exist is not an error, so a partially-failed create can
// still be cleaned up.
//
// Callers MUST have already proven ownership — see Repo.Remove, which deletes the
// owner-scoped row first and only calls this once RowsAffected is non-zero.
func DeleteGitRepo(ctx context.Context, re Repo) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "git.DeleteGitRepo")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", re.ID))

	gitPath, err := repoDiskPath(re.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "delete git repo: bad id", "Repo_id", re.ID, "error", err)
		return err
	}

	// os.RemoveAll rather than shelling out to `rm -rf`: no external binary, no
	// PATH dependency, and already a no-op for a missing path.
	if err := os.RemoveAll(gitPath); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "delete git repo", "path", gitPath, "error", err)
		return err
	}

	slog.InfoContext(ctx, "git repo deleted", "path", gitPath)
	span.SetStatus(codes.Ok, "")
	return nil
}
