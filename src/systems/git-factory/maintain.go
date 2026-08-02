package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Repository maintenance: keeping the bytes on a fixed-size volume bounded.
//
// Three parts, all of them consequences of "the git plane owns a disk":
//
//   - ACCOUNTING. A push is the only thing that grows a repo, so size is measured
//     after each one and stored, rather than walked on read. It is an approximation
//     by design (git's own count-objects numbers, not a du) — cheap enough to run on
//     every push, which is what makes it stay true.
//   - QUOTA. Enforced by git itself: receive-pack is given the number of bytes still
//     available as receive.maxInputSize, so it refuses an oversized push with an
//     error the client sees, instead of writing the objects and having us delete them
//     afterwards.
//   - GC. Loose objects from every push accumulate forever otherwise. A worker in
//     THIS pod repacks each repo periodically — not a sidecar Deployment: the volume
//     is ReadWriteOnce and single-writer, so the only process that may touch the
//     bytes is this one.
//
// The lock below is what keeps gc off a repo that is being pushed to. It is
// in-process, which is sufficient for exactly the same reason the worker is in-pod:
// there is only one writer. If the plane ever becomes multi-writer (§5 Step 3, one
// node per shard), the lock moves to the node that owns the shard — still one writer
// per repo, still local.
//
// It is unrelated to the .lock FILES git writes on disk to serialise ref updates.
// Those outlive the process that made them and are the janitor's business, not this
// mutex's — see reflock.go, which the sweep below also drives.

// repoLocks hands out one RWMutex per repo id. A push takes it shared (git does its
// own ref locking, so concurrent pushes are git's problem, not ours); maintenance
// takes it exclusively and gives up rather than waiting, since a repo that is busy
// now is a repo worth repacking on the next tick instead.
type repoLockSet struct {
	mu    sync.Mutex
	locks map[string]*sync.RWMutex
}

var repoLocks = &repoLockSet{locks: map[string]*sync.RWMutex{}}

func (s *repoLockSet) get(repoID string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.locks[repoID]; ok {
		return l
	}
	l := &sync.RWMutex{}
	s.locks[repoID] = l
	return l
}

// repoQuotaBytes is the per-repo ceiling, from GIT_REPO_QUOTA_MB. 0 (the default)
// means unlimited, so the feature is opt-in and an unset deployment behaves exactly
// as it did before.
func repoQuotaBytes() int64 {
	mb, err := strconv.ParseInt(strings.TrimSpace(envOrDefault("GIT_REPO_QUOTA_MB", "0")), 10, 64)
	if err != nil || mb <= 0 {
		return 0
	}
	return mb * 1024 * 1024
}

// maintenanceInterval is how often the gc worker sweeps every repo, from
// GIT_MAINTENANCE_INTERVAL (a Go duration). Empty or unparseable means the default;
// "0" disables the worker entirely.
func maintenanceInterval() time.Duration {
	raw := strings.TrimSpace(envOrDefault("GIT_MAINTENANCE_INTERVAL", ""))
	if raw == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("maintenance: bad GIT_MAINTENANCE_INTERVAL, using the default", "value", raw)
		return time.Hour
	}
	return d
}

// repoSizeBytes asks git how much space a repo occupies. count-objects -v reports
// KiB, covering both the loose objects a fresh push leaves and the packs a repack
// produces — the two things that actually grow. It deliberately does not walk the
// directory: this runs on the push path, and a du over a large repo does not.
func repoSizeBytes(ctx context.Context, dir string) (int64, error) {
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "count-objects", "-v")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var kib int64
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ": ")
		if !ok {
			continue
		}
		// "size" is loose objects, "size-pack" is packed ones; a repo mid-life has
		// both, so the total is the sum rather than either alone.
		if key != "size" && key != "size-pack" {
			continue
		}
		n, convErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if convErr != nil {
			continue
		}
		kib += n
	}
	return kib * 1024, nil
}

// recordRepoSize measures a repo and stores the result. Failure is logged and
// swallowed by callers: the push it follows has already succeeded, and a stale size
// is not worth failing a completed write over.
func recordRepoSize(ctx context.Context, repoID string) error {
	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		return err
	}
	size, err := repoSizeBytes(ctx, dir)
	if err != nil {
		return err
	}
	return setRepoSize(ctx, repoID, size)
}

// quotaHeadroom returns the bytes a push may still add, and whether a limit applies
// at all. A repo already at or over its quota yields 0 headroom, which the caller
// turns into a refusal — accepting a push that could only make it worse is the one
// outcome a quota exists to prevent.
func quotaHeadroom(ctx context.Context, repoID string) (headroom int64, limited bool) {
	quota := repoQuotaBytes()
	if quota <= 0 {
		return 0, false
	}
	used, err := getRepoSize(ctx, repoID)
	if err != nil {
		// Unknown usage: let the push through rather than blocking writes on a
		// bookkeeping failure. The next successful push records a real number.
		slog.WarnContext(ctx, "maintenance: could not read repo size for quota", "Repo_id", repoID, "error", err)
		return 0, false
	}
	if used >= quota {
		return 0, true
	}
	return quota - used, true
}

// gcRepo repacks one repo, skipping it when a push holds the lock. "git gc" rather
// than a bare repack so git's own heuristics (loose-object thresholds, reflog expiry,
// pruning unreachable objects past their grace period) decide what is worth doing.
func gcRepo(ctx context.Context, repoID string) (ran bool, err error) {
	ctx, span := otel.Tracer(serviceName).Start(ctx, "maintenance.gc")
	defer span.End()
	span.SetAttributes(attribute.String("Repo.id", repoID))

	lock := repoLocks.get(repoID)
	if !lock.TryLock() {
		span.SetStatus(codes.Ok, "")
		return false, nil
	}
	defer lock.Unlock()

	dir, err := localDirFor(ctx, repoID)
	if err != nil {
		span.RecordError(err)
		return false, err
	}
	cmd := exec.CommandContext(ctx, gitBinary, "-C", dir, "gc", "--auto", "--quiet")
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		span.RecordError(runErr)
		span.SetStatus(codes.Error, runErr.Error())
		return false, fmt.Errorf("git gc: %w (%s)", runErr, strings.TrimSpace(string(out)))
	}
	// Size after gc, not before: repacking is the one thing that makes a repo
	// smaller, so the stored number would otherwise only ever ratchet up.
	if size, sizeErr := repoSizeBytes(ctx, dir); sizeErr == nil {
		if err := setRepoSize(ctx, repoID, size); err != nil {
			slog.WarnContext(ctx, "maintenance: could not store size after gc", "Repo_id", repoID, "error", err)
		}
	}
	span.SetStatus(codes.Ok, "")
	return true, nil
}

// runMaintenance sweeps every repo once. Errors are per-repo and never abort the
// sweep: one broken repository must not stop the rest from being repacked.
func runMaintenance(ctx context.Context) (swept, skipped, locksCleared int) {
	ids, err := listAllRepoIDs(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "maintenance: could not list repos", "error", err)
		return 0, 0, 0
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		// Locks before gc, and unconditionally — not inside gcRepo. A stale
		// packed-refs.lock makes the repack itself fail, so clearing it first lets the
		// same tick get the gc it would otherwise have lost; and gcRepo gives up on a
		// busy repo, which is exactly the repo whose locks most need looking at.
		if n, err := sweepStaleLocks(ctx, id); err != nil {
			slog.WarnContext(ctx, "maintenance: lock sweep failed", "Repo_id", id, "error", err)
		} else {
			locksCleared += n
		}

		ran, err := gcRepo(ctx, id)
		switch {
		case err != nil:
			slog.WarnContext(ctx, "maintenance: gc failed", "Repo_id", id, "error", err)
		case ran:
			swept++
		default:
			skipped++
		}
	}
	return swept, skipped, locksCleared
}

// startMaintenance runs the sweep on an interval until ctx is cancelled. Started from
// main; a zero interval turns it off.
func startMaintenance(ctx context.Context) {
	interval := maintenanceInterval()
	if interval <= 0 {
		slog.Info("maintenance: disabled")
		return
	}
	slog.Info("maintenance: started",
		"interval", interval.String(),
		"quota_mb", repoQuotaBytes()/(1024*1024),
		"ref_lock_max_age", refLockMaxAge().String())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				swept, skipped, locks := runMaintenance(ctx)
				slog.InfoContext(ctx, "maintenance: sweep complete",
					"repacked", swept, "skipped_busy", skipped, "stale_locks_cleared", locks)
			}
		}
	}()
}
