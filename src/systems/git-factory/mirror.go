package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// The pull-through-mirror plane (DESIGN-read-replicas.md §7). git-connector asks this
// service to keep a local, warm copy of an UPSTREAM repo (one that lives on Forgejo /
// GitHub / GitLab) so Workflows clone from here — in-cluster, warm, offloaded — instead
// of paying a full clone against the upstream every run.
//
// This is entirely OPT-IN: nothing is mirrored unless git-connector calls
// /internal/mirrors for it, and existing native repos are untouched. The service holds
// no upstream credentials: the caller supplies an already-authenticated UpstreamURL per
// request, it is used as a fetch argv and then discarded — only the credential-free URL
// is ever written to the row (sanitizeUpstreamURL).

// mirrorInternalKey is the shared secret that authenticates git-connector's calls to the
// /internal/mirrors surface (the same shape as the git-connector broker's own
// GIT_INTERNAL_KEY). Read per call rather than cached so a rotation via a mounted-secret
// file is picked up, and so tests can set it with t.Setenv. Empty disables the surface.
func mirrorInternalKey() string { return secret("GIT_FACTORY_INTERNAL_KEY") }

// internalKeyOK reports whether a request carries the correct X-Internal-Key. When no
// key is configured the surface is closed entirely (reject all) rather than open — an
// unset key must never mean "anyone may create mirrors". The compare is constant-time.
func internalKeyOK(r *http.Request) bool {
	want := mirrorInternalKey()
	if want == "" {
		return false
	}
	got := r.Header.Get("X-Internal-Key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// sanitizeUpstreamURL strips any userinfo (user:pass@) from a URL so a mirror row can
// record WHERE it fetches from without persisting the credential the caller supplied.
// A URL that does not parse is returned as-is minus everything up to an '@', a
// belt-and-braces fallback so a malformed value can never leak a secret into the DB.
func sanitizeUpstreamURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.User = nil
		return u.String()
	}
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		if scheme := strings.Index(raw, "://"); scheme >= 0 && scheme < at {
			return raw[:scheme+3] + raw[at+1:]
		}
	}
	return raw
}

// mirrorRefspecs mirror every branch and tag, forced (upstream is authoritative, so a
// non-fast-forward or a rewritten history must overwrite the local copy) and pruned (a
// branch deleted upstream disappears here too).
var mirrorRefspecs = []string{
	"+refs/heads/*:refs/heads/*",
	"+refs/tags/*:refs/tags/*",
}

// mirrorFetch ensures the bare repo for re exists and pulls UPSTREAM into it via authURL.
// authURL carries credentials and is passed as an argv element (never a shell, never
// written to git config), so it is used and forgotten. HEAD is reconciled afterwards so
// a subsequent clone checks a branch out rather than landing detached.
func mirrorFetch(ctx context.Context, re Repo, authURL string) error {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "mirror.fetch")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", re.ID))

	dir, err := repoDiskPath(re.ID)
	if err != nil {
		return err
	}
	// Ensure a bare repo exists to fetch into. init is idempotent-ish; guard on the
	// path so a re-fetch of an existing mirror skips it.
	if !dirExists(dir) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("mirror mkdir: %w", err)
		}
		if err := exec.CommandContext(ctx, gitBinary, "-C", dir, "init", "--bare",
			"--initial-branch="+initialBranch(re)).Run(); err != nil {
			return fmt.Errorf("mirror init: %w", err)
		}
	}

	args := append([]string{"-C", dir, "fetch", "--prune", "--force", authURL}, mirrorRefspecs...)
	cmd := exec.CommandContext(ctx, gitBinary, args...)
	// Never prompt for credentials: if authURL is missing/invalid, fail fast rather
	// than hang a request waiting on a terminal that isn't there.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "fetch failed")
		// out may echo the URL; it is sanitized before logging so a credential in the
		// remote URL never reaches the logs.
		slog.ErrorContext(ctx, "mirror: fetch failed", "Repo_id", re.ID,
			"upstream", sanitizeUpstreamURL(authURL), "error", err,
			"output", sanitizeUpstreamURL(strings.TrimSpace(string(out))))
		return fmt.Errorf("mirror fetch: %w", err)
	}

	// A clone needs HEAD to point at a real branch; reconcile it to the repo's default
	// (falling back to any existing branch) the same way a push does.
	if err := reconcileHEAD(ctx, re.ID, re.DefaultBranch); err != nil {
		slog.WarnContext(ctx, "mirror: could not reconcile HEAD after fetch", "Repo_id", re.ID, "error", err)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// refExists reports whether ref resolves in the mirror's bare repo — used to enforce the
// refresh-before-clone guarantee (a CI clone at a known commit must not be served a copy
// that predates it). ref may be a branch name, a tag, or a full/abbrev SHA.
func refExists(ctx context.Context, repoID, ref string) bool {
	dir, err := repoDiskPath(repoID)
	if err != nil {
		return false
	}
	// --verify + ^{commit} makes this true only for something that resolves to a real
	// commit object, so a stray ref name that no longer points anywhere reads as absent.
	return exec.CommandContext(ctx, gitBinary, "-C", dir,
		"rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil
}

// markMirrored records a successful fetch: it stamps MirrorAt and (idempotently) the
// kind + sanitized upstream, without disturbing the ownership-scoped columns. A direct
// update keeps this off the owner-scoped Repo.Update path, which exists for user writes.
func markMirrored(ctx context.Context, id, upstream string, at time.Time) error {
	return connect().WithContext(ctx).Model(&Repo{}).Where("id = ?", id).
		Updates(map[string]any{
			"kind":         kindMirror,
			"upstream_url": sanitizeUpstreamURL(upstream),
			"mirror_at":    at,
			"updated_at":   at,
		}).Error
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
