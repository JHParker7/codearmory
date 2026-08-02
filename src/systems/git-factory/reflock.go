package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// The stale ref-lock janitor (ARCHITECTURE §6).
//
// git serialises a ref update by creating refs/heads/<name>.lock with O_CREAT|O_EXCL
// and renaming it over the ref when it is done. That file is an ordinary file with no
// lease and no holder recorded in it, so if the process dies mid-push it simply stays
// there. Nothing that comes along later can tell whether the holder is still running:
// a lock held by a live push and a lock left by a process killed an hour ago are byte
// for byte identical.
//
// On one node that is a nuisance — restarting the node clears it, and repoLocks in
// maintain.go covers the in-process case. Shared storage does not fix it, it WIDENS
// it: once every pod sees the same bytes, a lock left behind by a pod that was
// OOM-killed or evicted blocks pushes to that ref from every pod, indefinitely. The
// symptom is "failed to lock ref" on a repo nobody is pushing to, which reads as
// corruption rather than as a leftover file. So this must be in place by the time the
// plane is multi-pod, not after.
//
// Note what this is NOT. git-receive-pack refusing a push because another push holds
// the ref is correct behaviour and stays exactly as it is — telling the loser of a
// genuine race to retry is the whole point of the lock. This only clears locks whose
// holder is gone.

// refLockMaxAge is how long a lock file may sit untouched before it is treated as
// abandoned, from GIT_REF_LOCK_MAX_AGE (a Go duration).
//
// The threshold has to exceed the longest a single git process can legitimately hold a
// lock. That figure is dominated not by network transfer — a push holds the ref lock
// only across the ref transaction, after the objects are already in — but by pack-refs
// during a repack of a large repository, and by whatever a pre-receive hook takes. An
// hour is comfortably past both and matches the default gc interval.
//
// It errs long on purpose, because the two failure directions are not symmetric.
// Waiting too long leaves a ref unpushable until the next tick: loud, and the error
// names the lock file. Clearing too early is silent — unlinking a live lock does not
// make the holder fail, since its rename() over the ref succeeds either way, so two
// pushers each believe they hold the ref and one update is simply lost. A janitor that
// can lose a write is worse than one that is slow.
//
// Zero or negative DISABLES the sweep. It deliberately does not mean "everything is
// stale": a misconfiguration must not turn into deleting every lock on sight.
func refLockMaxAge() time.Duration {
	raw := strings.TrimSpace(envOrDefault("GIT_REF_LOCK_MAX_AGE", ""))
	if raw == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("reflock: bad GIT_REF_LOCK_MAX_AGE, using the default", "value", raw)
		return time.Hour
	}
	return d
}

// lockSuffix is the extension git gives every lock file it takes.
const lockSuffix = ".lock"

// repoLockFiles are the lock files that live outside refs/. Named individually rather
// than found by walking the repository, because the rest of a bare repo is objects/ —
// tens of thousands of entries holding no locks at all, and one stat per entry over a
// network filesystem for every repo on every tick.
var repoLockFiles = []string{
	// Taken by pack-refs, i.e. by our own gc. A stale one blocks every subsequent
	// repack as well as ref updates.
	"packed-refs.lock",
	"HEAD.lock",
	"config.lock",
	filepath.Join("objects", "info", "commit-graph.lock"),
}

// sweepStaleLocksIn clears abandoned lock files under one bare repository and reports
// how many it removed. Split from sweepStaleLocks so it can be driven against a plain
// directory with no database or node placement behind it.
//
// AGE IS MEASURED BY MTIME, which is what git leaves behind and is enough here. git
// sets it when it creates the lock and again when it writes the new ref value in, and
// never refreshes it while it holds the lock afterwards. So the age this reads is a
// LOWER BOUND on the age of the hold, and that is the direction the safety argument
// needs: it can under-estimate and skip a lock that is genuinely stale (swept on the
// next tick, no harm), but it cannot over-estimate and delete one that is live.
//
// Under shared storage the mtime comes from the metadata engine rather than from each
// pod's own clock, so pods compare against one clock; on a local filesystem there is
// only one host, so the same holds. That matters — with per-pod clocks a pod running
// fast would delete locks that are still fresh.
func sweepStaleLocksIn(ctx context.Context, dir string, maxAge time.Duration) (cleared int) {
	if maxAge <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)

	// Stat and remove in one step rather than collecting candidates and deleting them
	// in a second pass. POSIX has no compare-and-unlink, so the check and the unlink
	// can never be atomic; keeping them adjacent makes the window as small as it can
	// be. What closes the gap properly is the threshold: an hour of slack against a
	// window of microseconds.
	consider := func(path string) {
		if ctx.Err() != nil {
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			// Missing is the normal case — most repos hold no locks — and is also what
			// a racing janitor on another pod leaves behind. Neither is worth logging.
			return
		}
		if info.IsDir() || !info.ModTime().Before(cutoff) {
			return
		}
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.WarnContext(ctx, "reflock: could not clear a stale lock", "path", path, "error", err)
			}
			return
		}
		cleared++
		// Logged at info, individually, and never silently: a lock file vanishing is
		// indistinguishable from corruption to someone debugging a failed push, so the
		// janitor has to be able to account for every file it took.
		slog.InfoContext(ctx, "reflock: cleared an abandoned lock file",
			"path", path,
			"age", time.Since(info.ModTime()).Round(time.Second).String(),
			"threshold", maxAge.String())
	}

	// Ref locks are the ones that block pushes, and they mirror the ref namespace, so
	// this walks refs/ rather than guessing names.
	_ = filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry (including refs/ itself not existing) skips that
			// subtree. One broken repo must not stop the sweep.
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), lockSuffix) {
			return nil
		}
		consider(path)
		return nil
	})

	for _, rel := range repoLockFiles {
		consider(filepath.Join(dir, rel))
	}
	return cleared
}

// sweepStaleLocks clears abandoned lock files for one repo.
//
// Safe to run from every pod at once, which is how it will be deployed: the janitors
// need no coordination because the loser of a race gets ENOENT from the unlink, which
// is treated as success. There is no lease to take and nothing to elect.
//
// It deliberately does NOT take the repo's in-process lock the way gcRepo does. That
// lock is held by a live push, and a repo under continuous pushes would then never be
// swept — which is the repo most likely to have accumulated an abandoned lock. Age is
// what makes this safe, not exclusion: a lock held by a push in flight is seconds old
// and never a candidate.
func sweepStaleLocks(ctx context.Context, repoID string) (int, error) {
	maxAge := refLockMaxAge()
	if maxAge <= 0 {
		return 0, nil
	}

	ctx, span := otel.Tracer(serviceName).Start(ctx, "maintenance.reflock")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID))

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return 0, err
	}

	cleared := sweepStaleLocksIn(ctx, dir, maxAge)
	span.SetAttributes(attribute.Int("locks.cleared", cleared))
	span.SetStatus(codes.Ok, "")
	return cleared, nil
}
