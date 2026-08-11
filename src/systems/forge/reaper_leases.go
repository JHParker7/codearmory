package main

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// leaseReaperInterval is how often the sweep runs. A lease holds real memory for
// its whole life, so the sweep is the difference between a caller that crashed
// costing forge a runner class for a few minutes and costing it one until restart.
var leaseReaperInterval = time.Duration(envIntOrDefault("FORGE_LEASE_REAPER_INTERVAL_SECS", 30)) * time.Second

// LeaseSweeper is implemented by runtimes that can enumerate the sandboxes they are
// holding, so the reaper can collect ones with no lease row behind them.
//
// This is separate from LeaseRuntime because it answers a different question. The
// deadline sweep works from rows and asks "which of my leases should end?"; this
// works from the runtime and asks "which sandboxes does nothing know about?" — the
// only way to find a sandbox whose row was never committed, which is exactly what a
// crash between creating the pod and inserting the row leaves behind.
type LeaseSweeper interface {
	// OrphanedLeasePods returns the names of lease sandboxes whose lease id is not a
	// key in known.
	OrphanedLeasePods(ctx context.Context, known map[string]bool) ([]string, error)
	// DeleteLeasePodByName tears down a sandbox addressed by name, for orphans with
	// no row left to derive the name from.
	DeleteLeasePodByName(ctx context.Context, name string) error
}

// startLeaseReaper stops leases that have outlived their deadlines and collects
// sandboxes that no longer have a lease behind them.
//
// A pass runs immediately at startup, before the interval: a restart is precisely
// when orphans exist, since every in-flight StartLease goroutine died with the old
// process and any sandbox it had created is now untracked.
func startLeaseReaper(ctx context.Context, reg *runtimeRegistry) {
	if leaseReaperInterval <= 0 {
		slog.InfoContext(ctx, "lease reaper disabled (interval <= 0)")
		return
	}
	slog.InfoContext(ctx, "lease reaper started", "interval_secs", int(leaseReaperInterval.Seconds()))
	reapLeases(ctx, reg)
	ticker := time.NewTicker(leaseReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reapLeases(ctx, reg)
		}
	}
}

// reapLeases runs one sweep: expire past-deadline leases, then collect orphans.
func reapLeases(ctx context.Context, reg *runtimeRegistry) {
	ctx, span := otel.Tracer("forge").Start(ctx, "reap_leases")
	defer span.End()

	active, err := listActiveLeases(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "lease reaper: list active", "error", err)
		return
	}

	now := time.Now().UTC()
	// Leases that survive this pass are what the orphan sweep treats as known. It is
	// built from the pre-expiry set minus everything expired below, rather than by
	// re-reading: a lease created between the two reads would not be in a second read
	// either, and deleting its sandbox as an orphan would kill a live lease.
	known := make(map[string]bool, len(active))
	for _, lease := range active {
		known[lease.LeaseID] = true
	}

	for _, lease := range active {
		reason := leaseExpiryReason(lease, now)
		if reason == "" {
			continue
		}
		delete(known, lease.LeaseID)
		if err := releaseLease(ctx, reg, lease, "reaped: "+reason); err != nil {
			slog.ErrorContext(ctx, "lease reaper: release", "lease_id", lease.LeaseID, "error", err)
			continue
		}
		slog.WarnContext(ctx, "lease reaper: reaped lease", "lease_id", lease.LeaseID, "reason", reason,
			"age_secs", int(now.Sub(lease.CreatedAt).Seconds()))
	}

	sweepOrphanedLeaseSandboxes(ctx, reg, known)
	span.SetStatus(codes.Ok, "")
}

// leaseExpiryReason reports why a lease should be reaped now, or "" to keep it.
//
// The order matters: lifetime is checked before idleness so a lease that is both
// is reported as the more specific of the two, which is what a caller wondering
// where their sandbox went needs to read.
func leaseExpiryReason(lease Lease, now time.Time) string {
	switch {
	case now.After(lease.hardDeadline()):
		return "exceeded its maximum lifetime"
	case now.After(lease.idleDeadline()):
		return "idle"
	case lease.Status == leaseStarting && now.After(lease.CreatedAt.Add(time.Duration(leaseStartTimeoutSecs)*time.Second)):
		// A lease still starting well past the start timeout means the goroutine that
		// was booting it is gone — the usual cause is a forge restart. Nothing else
		// will ever move this row, so the reaper has to.
		return "never finished starting"
	default:
		return ""
	}
}

// sweepOrphanedLeaseSandboxes deletes lease sandboxes with no active lease row.
//
// It sweeps every configured backend rather than only the ones with live leases,
// because an orphan's defining property is that no row names its backend — if the
// row existed we would not be looking for it this way.
func sweepOrphanedLeaseSandboxes(ctx context.Context, reg *runtimeRegistry, known map[string]bool) {
	for _, backend := range reg.Backends(ctx) {
		rt, err := reg.Get(ctx, backend)
		if err != nil {
			continue
		}
		sweeper, ok := rt.(LeaseSweeper)
		if !ok {
			continue
		}
		orphans, err := sweeper.OrphanedLeasePods(ctx, known)
		if err != nil {
			slog.WarnContext(ctx, "lease reaper: list orphans", "backend", backend, "error", err)
			continue
		}
		for _, name := range orphans {
			if err := sweeper.DeleteLeasePodByName(ctx, name); err != nil {
				slog.WarnContext(ctx, "lease reaper: delete orphan", "backend", backend, "sandbox", name, "error", err)
				continue
			}
			slog.WarnContext(ctx, "lease reaper: deleted orphaned lease sandbox", "backend", backend, "sandbox", name)
		}
	}
}
