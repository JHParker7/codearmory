package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var gormDB *gorm.DB
var gormDBRead *gorm.DB
var dbInitMu sync.Mutex

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/outpost_gateway")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to database: %v\n", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

func connectRead() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDBRead != nil {
		return gormDBRead
	}
	if gormDB != nil {
		return gormDB
	}
	readURL := secret("DATABASE_READ_URL")
	if readURL == "" {
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/outpost_gateway")
	}
	conn, err := gorm.Open(postgres.Open(readURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to read database: %v\n", err)
		os.Exit(1)
	}
	gormDBRead = conn
	return gormDBRead
}

func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// isDuplicateKey reports whether err is a Postgres unique-violation. Used to
// treat an outpost re-POSTing the same event id (at-least-once) as success.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "23505")
}

// getOutpost loads an active outpost by id.
func getOutpost(ctx context.Context, id string) (Outpost, error) {
	var o Outpost
	err := connectRead().WithContext(ctx).Where("outpost_id=? AND active=?", id, true).First(&o).Error
	return o, err
}

// listOutposts returns active outposts scoped to an org (or a user when org is empty).
func listOutposts(ctx context.Context, orgID, userID string) ([]Outpost, error) {
	var out []Outpost
	q := connectRead().WithContext(ctx).Where("active=?", true)
	if orgID != "" {
		q = q.Where("org_id=?", orgID)
	} else {
		q = q.Where("user_id=?", userID)
	}
	err := q.Order("created_at DESC").Find(&out).Error
	return out, err
}

func (o Outpost) Add(ctx context.Context) error {
	return connect().WithContext(ctx).Create(&o).Error
}

func (o Outpost) Save(ctx context.Context) error {
	o.UpdatedAt = time.Now().UTC()
	return connect().WithContext(ctx).Save(&o).Error
}

// softDeleteOutpost deactivates an outpost (frees nothing else; commands/events are kept for audit).
func softDeleteOutpost(ctx context.Context, id string) error {
	return connect().WithContext(ctx).Model(&Outpost{}).
		Where("outpost_id=? AND active=?", id, true).
		Update("active", false).Error
}

// touchOutpost records liveness from a heartbeat or successful long-poll.
func touchOutpost(ctx context.Context, id string) error {
	now := time.Now().UTC()
	return connect().WithContext(ctx).Model(&Outpost{}).
		Where("outpost_id=?", id).
		Updates(map[string]any{"last_seen_at": now, "status": OutpostConnected, "updated_at": now}).Error
}

// enqueueCommandDB inserts a pending command for an outpost.
func enqueueCommandDB(ctx context.Context, c OutpostCommand) error {
	return connect().WithContext(ctx).Create(&c).Error
}

// claimCommands atomically claims up to n deliverable commands for one outpost
// using FOR UPDATE SKIP LOCKED, flips them to 'claimed', and returns them.
// Modeled on the workflows run dequeue so any gateway replica can serve any
// outpost off the shared queue. A command is deliverable when it is pending, or
// when it was claimed but never acked and its claim lease has expired — that
// reclaim makes command delivery at-least-once across outpost crashes and lost
// acks (otherwise a claimed-but-unacked command would be orphaned forever).
func claimCommands(ctx context.Context, outpostID string, n int) ([]OutpostCommand, error) {
	ctx, span := otel.Tracer("outpost-gateway").Start(ctx, "db.commands.claim")
	defer span.End()

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	defer tx.Rollback() //nolint:errcheck

	leaseCutoff := time.Now().UTC().Add(-commandClaimLease)
	var cmds []OutpostCommand
	res := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("outpost_id=? AND (status=? OR (status=? AND claimed_at < ?))",
			outpostID, CmdPending, CmdClaimed, leaseCutoff).
		Order("created_at").
		Limit(n).
		Find(&cmds)
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return nil, res.Error
	}
	if len(cmds) == 0 {
		return nil, tx.Commit().Error
	}

	ids := make([]string, len(cmds))
	for i := range cmds {
		ids[i] = cmds[i].ID
	}
	now := time.Now().UTC()
	if err := tx.Model(&OutpostCommand{}).Where("id IN ?", ids).
		Updates(map[string]any{"status": CmdClaimed, "claimed_at": now}).Error; err != nil {
		span.RecordError(err)
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return cmds, nil
}

// ackCommand marks a claimed command done.
func ackCommand(ctx context.Context, outpostID, id string) error {
	return connect().WithContext(ctx).Model(&OutpostCommand{}).
		Where("id=? AND outpost_id=?", id, outpostID).
		Update("status", CmdDone).Error
}

// addEvent inserts a pending event into the outbox, due immediately.
func addEvent(ctx context.Context, e OutpostEvent) error {
	if e.NextRetryAt.IsZero() {
		e.NextRetryAt = time.Now().UTC()
	}
	return connect().WithContext(ctx).Create(&e).Error
}

// claimDueEvents atomically claims up to n events that are pending and due,
// using FOR UPDATE SKIP LOCKED so multiple dispatcher replicas don't double-send.
// Claimed rows are not status-flipped here; the dispatcher updates each row to
// delivered/failed after attempting it, and bumps next_retry_at on transient
// failure. To avoid two pollers grabbing the same row between claim and update,
// we push next_retry_at forward by a lease while still inside the tx.
func claimDueEvents(ctx context.Context, n int, lease time.Duration) ([]OutpostEvent, error) {
	ctx, span := otel.Tracer("outpost-gateway").Start(ctx, "db.events.claimDue")
	defer span.End()

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	defer tx.Rollback() //nolint:errcheck

	var events []OutpostEvent
	res := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("status=? AND next_retry_at <= ?", EvPending, time.Now().UTC()).
		Order("next_retry_at").
		Limit(n).
		Find(&events)
	if res.Error != nil {
		span.RecordError(res.Error)
		return nil, res.Error
	}
	if len(events) == 0 {
		return nil, tx.Commit().Error
	}
	ids := make([]string, len(events))
	for i := range events {
		ids[i] = events[i].ID
	}
	// Lease: push next_retry_at out so a peer poller won't re-claim while we work.
	if err := tx.Model(&OutpostEvent{}).Where("id IN ?", ids).
		Update("next_retry_at", time.Now().UTC().Add(lease)).Error; err != nil {
		span.RecordError(err)
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return events, nil
}

// markEventDelivered finalizes a successfully dispatched event.
func markEventDelivered(ctx context.Context, id string) error {
	return connect().WithContext(ctx).Model(&OutpostEvent{}).
		Where("id=?", id).
		Updates(map[string]any{"status": EvDelivered, "last_error": ""}).Error
}

// scheduleEventRetry bumps the attempt counter and next_retry_at, or dead-letters
// the event once attempts are exhausted.
func scheduleEventRetry(ctx context.Context, e OutpostEvent, dispatchErr string) error {
	attempt := e.Attempt + 1
	if attempt >= maxDeliveryAttempts {
		return connect().WithContext(ctx).Model(&OutpostEvent{}).
			Where("id=?", e.ID).
			Updates(map[string]any{"status": EvFailed, "attempt": attempt, "last_error": dispatchErr}).Error
	}
	next := time.Now().UTC().Add(retryBackoff(attempt))
	return connect().WithContext(ctx).Model(&OutpostEvent{}).
		Where("id=?", e.ID).
		Updates(map[string]any{"attempt": attempt, "last_error": dispatchErr, "next_retry_at": next}).Error
}

// retryBackoff returns the delay before the nth attempt (1-indexed):
// 30s, 60s, 120s, 240s, ... capped at 900s. Mirrors the hooks dead-letter loop.
func retryBackoff(attempt int) time.Duration {
	d := 30 * (1 << (attempt - 1))
	if d > 900 {
		d = 900
	}
	return time.Duration(d) * time.Second
}
