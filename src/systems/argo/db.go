package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
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
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/argo")), &gorm.Config{
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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/argo")
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

// orgScope returns the tenant-scoping key for an app/sync.
func orgScope(orgID, userID string) string {
	if orgID != "" {
		return orgID
	}
	return "u:" + userID
}

// listApps returns active apps for a tenant.
func listApps(ctx context.Context, orgKey string) ([]App, error) {
	var apps []App
	err := connectRead().WithContext(ctx).
		Where("org_key=? AND active=?", orgKey, true).
		Order("name").Find(&apps).Error
	return apps, err
}

func getApp(ctx context.Context, orgKey, name string) (App, error) {
	var a App
	err := connectRead().WithContext(ctx).
		Where("org_key=? AND name=? AND active=?", orgKey, name, true).First(&a).Error
	return a, err
}

func getSync(ctx context.Context, id string) (Sync, error) {
	var s Sync
	err := connectRead().WithContext(ctx).Where("sync_id=? AND active=?", id, true).First(&s).Error
	return s, err
}

func (s Sync) Add(ctx context.Context) error {
	return connect().WithContext(ctx).Create(&s).Error
}

// upsertAppState records the latest Argo Application status learned from an
// app-state event. The tenant scope is derived the same way for events and user
// requests: org_id when present, else the owning user — so a self-hosted
// single-user (no-org) deployment sees its own outpost's apps, and every member
// of an org sees the org's apps.
func upsertAppState(ctx context.Context, orgID, userID, outpostID, name, syncStatus, healthStatus, revision, opPhase string) error {
	ctx, span := otel.Tracer("argo").Start(ctx, "db.app.upsert")
	defer span.End()
	orgKey := orgScope(orgID, userID)
	now := time.Now().UTC()

	var a App
	err := connect().WithContext(ctx).Where("org_key=? AND name=?", orgKey, name).First(&a).Error
	if err != nil && !isNotFound(err) {
		span.RecordError(err)
		return err
	}
	if isNotFound(err) {
		a = App{
			AppID: uuid.New().String(), OrgID: orgID, UserID: userID, OutpostID: outpostID, Name: name, OrgKey: orgKey,
			SyncStatus: syncStatus, HealthStatus: healthStatus, Revision: revision, OperationPhase: opPhase,
			Active: true, CreatedAt: now, UpdatedAt: now,
		}
		return connect().WithContext(ctx).Create(&a).Error
	}
	return connect().WithContext(ctx).Model(&App{}).Where("app_id=?", a.AppID).
		Updates(map[string]any{
			"outpost_id": outpostID, "sync_status": syncStatus, "health_status": healthStatus,
			"revision": revision, "operation_phase": opPhase, "active": true, "updated_at": now,
		}).Error
}

// updateInFlightSync correlates an app-state event to the most recent
// non-terminal sync for the same app on the same outpost and advances its
// status. Correlating by outpost_id (not org) keeps this robust regardless of
// org/user scoping.
func updateInFlightSync(ctx context.Context, outpostID, appName, syncStatus, healthStatus, opPhase, message string) (Sync, bool, error) {
	var s Sync
	err := connect().WithContext(ctx).
		Where("outpost_id=? AND app_name=? AND status IN ? AND active=?", outpostID, appName, []string{SyncPending, SyncRunning}, true).
		Order("created_at DESC").First(&s).Error
	if err != nil {
		if isNotFound(err) {
			return Sync{}, false, nil
		}
		return Sync{}, false, err
	}

	status := syncOutcome(syncStatus, healthStatus, opPhase)
	if status == s.Status {
		return s, false, nil
	}
	updates := map[string]any{"status": status, "message": message}
	now := time.Now().UTC()
	// Stamp started_at on the first non-pending transition — including a sync that
	// converges straight to a terminal state in one step — so a terminal sync is
	// never left with a null started_at.
	if s.StartedAt == nil && status != SyncPending {
		updates["started_at"] = now
	}
	if status == SyncSynced || status == SyncFailed {
		updates["ended_at"] = now
	}
	// Compare-and-swap on the observed status: events are at-least-once and may
	// arrive concurrently, so gate the write on the row still being in the status
	// we read. RowsAffected==0 means a peer already advanced it — report no change
	// so the resolved metric doesn't double-count.
	res := connect().WithContext(ctx).Model(&Sync{}).
		Where("sync_id=? AND status=?", s.SyncID, s.Status).Updates(updates)
	if res.Error != nil {
		return Sync{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		return s, false, nil
	}
	s.Status = status
	s.Message = message
	return s, true, nil
}

// markSyncFailed terminally fails a sync (used when dispatch to the outpost
// fails). Follows the entity-update convention and returns the error so callers
// don't strand a sync silently in a non-terminal state.
func markSyncFailed(ctx context.Context, syncID, message string) error {
	now := time.Now().UTC()
	return connect().WithContext(ctx).Model(&Sync{}).
		Where("sync_id=?", syncID).
		Updates(map[string]any{"status": SyncFailed, "message": message, "ended_at": now}).Error
}

// span error helper unused import guard removed.

// markSyncRunning flips a pending sync to running on the sync-started event.
func markSyncRunning(ctx context.Context, syncID string) error {
	now := time.Now().UTC()
	return connect().WithContext(ctx).Model(&Sync{}).
		Where("sync_id=? AND status=?", syncID, SyncPending).
		Updates(map[string]any{"status": SyncRunning, "started_at": now}).Error
}

// syncOutcome maps Argo status fields to a sync record status.
func syncOutcome(syncStatus, healthStatus, opPhase string) string {
	switch opPhase {
	case "Failed", "Error":
		return SyncFailed
	case "Succeeded":
		if healthStatus == "Healthy" && syncStatus == "Synced" {
			return SyncSynced
		}
		// Operation succeeded but app not yet healthy/synced — keep waiting.
		return SyncRunning
	case "Running", "Terminating":
		return SyncRunning
	}
	// No operation in progress: treat a healthy+synced app as done.
	if healthStatus == "Healthy" && syncStatus == "Synced" {
		return SyncSynced
	}
	return SyncRunning
}
