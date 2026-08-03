package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// A cluster-wide lease, so that work which must happen ONCE per interval happens once
// however many replicas are running.
//
// This exists because shared storage changed what a replica is. With a ReadWriteOnce
// volume the repack worker ran in-process precisely because there was only ever one
// writer — "one pod" and "one sweeper" were the same statement, and maintain.go says so.
// On a shared filesystem every replica can reach every repo, so an unguarded ticker
// means N pods walking the whole store N times per interval. That is worst exactly where
// shared storage is supposed to pay off: making N elastic is the point of the design, and
// an HPA would multiply the repack load by the replica count.
//
// IT IS NOT A CORRECTNESS MECHANISM, and nothing should be built on it as though it were.
// Two sweeps overlapping is safe on its own terms: `git gc` takes its own gc.pid lock and
// refuses to run twice against one repo, and the ref-lock janitor is age-gated and already
// tolerates a racing pod by design — reflock.go treats a vanished file as "what a racing
// janitor on another pod leaves behind". What the lease buys is not safety, it is cost: on
// a metadata-engine filesystem a full store walk is a great many round-trips, and doing it
// once rather than N times is the whole difference.
//
// That distinction is what licenses the deliberately plain implementation below — one
// conditional UPDATE and a wall-clock expiry, rather than a consensus protocol. Clock skew
// between nodes can hand the lease over early or late; both merely shift WHEN a sweep
// happens, and neither can corrupt a repository. Paying for Raft to schedule a repack
// would be the wrong trade.

// MaintenanceLease is one row per named lease. The row is the lease: it is created once
// and then only ever updated, so a lease's existence never depends on which replica
// happened to start first.
type MaintenanceLease struct {
	Name      string    `json:"name"       gorm:"column:name;primaryKey"`
	Holder    string    `json:"holder"     gorm:"column:holder;default:''"`
	ExpiresAt time.Time `json:"expires_at" gorm:"column:expires_at;index"`
}

func (MaintenanceLease) TableName() string { return "maintenance_leases" }

// maintenanceLeaseName is the one lease this service takes today. Named rather than
// implicit so a second periodic job can be added later without either inheriting this
// one's schedule or having to invent the mechanism again.
const maintenanceLeaseName = "maintenance-sweep"

// leaseEpoch is the "expired" timestamp — any past instant would do. Releasing writes
// this rather than deleting the row, so release and expiry take the same code path.
var leaseEpoch = time.Unix(0, 0).UTC()

// minMaintenanceLeaseTTL is the shortest TTL worth honouring. The holder renews at a
// third of the TTL (runLeasedSweep), so this is also what keeps that interval a
// positive duration: time.NewTicker panics on a non-positive one, and any TTL under 3ns
// divides to exactly zero. A sub-second lease is a typo rather than a tuning decision
// either way — it would renew tens of times a second against the database and hand the
// lease over on ordinary query latency.
const minMaintenanceLeaseTTL = time.Second

// maintenanceLeaseTTL is how long a lease is granted for, from
// GIT_MAINTENANCE_LEASE_TTL. It is NOT how long a sweep may take — the holder renews
// while it works — it is how long the sweep stays blocked after a holder dies without
// releasing. Shorter means faster takeover and more renewal traffic; the default of 5
// minutes is well under the default hourly interval, so a killed pod costs at most one
// skipped tick.
//
// This is the ONLY place a TTL is validated, so it must return something every caller
// can use unconditionally: at or above minMaintenanceLeaseTTL, always positive.
func maintenanceLeaseTTL() time.Duration {
	const def = 5 * time.Minute
	raw := strings.TrimSpace(envOrDefault("GIT_MAINTENANCE_LEASE_TTL", ""))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < minMaintenanceLeaseTTL {
		// Fall back rather than clamp. A value this far out is a mistake, and running
		// on a silently corrected one hides it — whereas the default is at least a
		// duration someone chose.
		slog.Warn("maintenance: bad GIT_MAINTENANCE_LEASE_TTL, using the default",
			"value", raw, "minimum", minMaintenanceLeaseTTL.String(), "default", def.String())
		return def
	}
	return d
}

// acquireLease grants name to holder for ttl, and reports whether it was granted. It is
// safe to call from every replica on every tick: that is the intended usage.
func acquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	db := connect().WithContext(ctx)

	// Ensure the row exists, already expired, so the UPDATE below is what actually
	// grants it. DoNothing on conflict is what keeps several replicas racing through a
	// cold start from turning first boot into an error — GORM renders it as ON CONFLICT
	// DO NOTHING on both Postgres and SQLite.
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&MaintenanceLease{Name: name, ExpiresAt: leaseEpoch}).Error; err != nil {
		return false, fmt.Errorf("ensure lease row: %w", err)
	}

	// The grant. One conditional UPDATE, which both engines execute atomically: claiming
	// the lease and proving it was free are the SAME statement, so there is no window
	// between the check and the claim for a second replica to slip through. Whoever's
	// UPDATE matches the expired row wins and everyone else sees RowsAffected 0.
	res := db.Model(&MaintenanceLease{}).
		Where("name = ? AND expires_at <= ?", name, now).
		Updates(map[string]any{"holder": holder, "expires_at": now.Add(ttl)})
	if res.Error != nil {
		return false, fmt.Errorf("acquire lease: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// renewLease extends a lease the caller already holds, and reports false once it no
// longer does. The holder predicate is the important half: a pod whose lease expired
// while it was stalled must not be able to renew its way back into ownership behind the
// new holder's back — it has to discover it lost and stop.
func renewLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	res := connect().WithContext(ctx).Model(&MaintenanceLease{}).
		Where("name = ? AND holder = ? AND expires_at > ?", name, holder, now).
		Update("expires_at", now.Add(ttl))
	if res.Error != nil {
		return false, fmt.Errorf("renew lease: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// coolDownLease holds the lease until the next sweep is due, and is what actually makes
// the lease save anything.
//
// Releasing on completion instead would only prevent CONCURRENT sweeps: replicas tick
// on their own offsets, so the pod that ticks next in the same interval would find the
// lease free and walk the whole store again. N replicas would still cost N full sweeps
// per interval — serialised rather than overlapping, but exactly the cost this file
// exists to avoid. Keeping the lease until the interval elapses is what turns "one
// sweeper at a time" into "one sweep per interval".
//
// Measured from the END of the sweep, so the gap between sweeps is at least d even when
// a sweep runs long. That makes the true period interval + sweep duration rather than
// interval, which is the right way round: drifting slightly slow costs nothing, while
// dating the cooldown from the start would let a sweep lasting longer than the interval
// be followed immediately by another.
//
// A holder that dies during the cooldown blocks nothing: the row expires exactly when
// the next sweep was due anyway, so any replica picks it up on schedule.
func coolDownLease(ctx context.Context, name, holder string, d time.Duration) error {
	// WithoutCancel for the same reason as releaseLease: this runs on the way out of a
	// sweep, and failing here on a cancelled context would leave the lease on its short
	// sweep TTL instead of the cooldown.
	ctx = context.WithoutCancel(ctx)
	if err := connect().WithContext(ctx).Model(&MaintenanceLease{}).
		Where("name = ? AND holder = ?", name, holder).
		Update("expires_at", time.Now().UTC().Add(d)).Error; err != nil {
		return fmt.Errorf("cool down lease: %w", err)
	}
	return nil
}

// releaseLease gives the lease up early, so the next tick can go to any replica instead
// of waiting out the TTL. Scoped to the holder so a pod that already lost the lease
// cannot expire the new holder's grant on its way out.
//
// Used when a sweep ends WITHOUT having covered the store — shutdown, specifically.
// A completed sweep cools down instead; releasing there is the bug coolDownLease
// documents.
func releaseLease(ctx context.Context, name, holder string) error {
	// WithoutCancel: release runs on the way out of a sweep, including when that sweep
	// ended because ctx was cancelled at shutdown. Releasing on the cancelled context
	// would fail exactly when release matters most, leaving the lease held by a process
	// that is gone and blocking maintenance until the TTL runs out.
	ctx = context.WithoutCancel(ctx)
	if err := connect().WithContext(ctx).Model(&MaintenanceLease{}).
		Where("name = ? AND holder = ?", name, holder).
		Update("expires_at", leaseEpoch).Error; err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}
