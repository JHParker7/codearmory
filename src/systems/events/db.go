package main

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Storage follows the codearmory GORM pattern: lazy connect() singletons, CREATE-TABLE
// auto-migration on startup, and Add/Get/List/Update methods on the model structs.
var (
	dbInitMu   sync.Mutex
	gormDB     *gorm.DB
	gormDBRead *gorm.DB
)

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/events")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to database", "error", err)
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
	url := secretOrDefault("DATABASE_READ_URL", secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/events"))
	conn, err := gorm.Open(postgres.Open(url), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		slog.Error("unable to connect to read database", "error", err)
		os.Exit(1)
	}
	gormDBRead = conn
	return gormDBRead
}

// eventRow is the stored form of an Event envelope — one row in the append-only event log.
// Actor is flattened to OrgID/UserID columns so the log indexes and filters by tenant.
type eventRow struct {
	ID          string         `gorm:"primaryKey"`
	Type        string         `gorm:"index:idx_events_org_type,priority:2"`
	Source      string         ``
	Subject     string         `gorm:"index:idx_events_org_subject,priority:2"`
	OrgID       string         `gorm:"index:idx_events_org_type,priority:1;index:idx_events_org_subject,priority:1"`
	UserID      string         ``
	OccurredAt  string         ``
	ReceivedAt  time.Time      `gorm:"index;autoCreateTime"`
	TraceID     string         ``
	CausationID string         ``
	Data        map[string]any `gorm:"serializer:json"`
}

func (eventRow) TableName() string { return "events" }

func toRow(e Event) eventRow {
	return eventRow{
		ID: e.ID, Type: e.Type, Source: e.Source, Subject: e.Subject,
		OrgID: e.Actor.OrgID, UserID: e.Actor.UserID, OccurredAt: e.OccurredAt,
		TraceID: e.TraceID, CausationID: e.CausationID, Data: e.Data,
	}
}

func (r eventRow) toEvent() Event {
	return Event{
		ID: r.ID, SpecVersion: SpecVersion, Type: r.Type, Source: r.Source, Subject: r.Subject,
		Actor: Actor{OrgID: r.OrgID, UserID: r.UserID}, OccurredAt: r.OccurredAt,
		TraceID: r.TraceID, CausationID: r.CausationID, Data: r.Data,
	}
}

// addEvent stores an event idempotently — a duplicate id (redelivery) is a no-op, not an error.
func addEvent(ctx context.Context, e Event) error {
	row := toRow(e)
	return connect().WithContext(ctx).
		Where("id = ?", e.ID).
		FirstOrCreate(&row).Error
}

// listEvents returns the tenant's recent events, newest first, optionally filtered by type.
func listEvents(ctx context.Context, orgID, userID, typ string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := connectRead().WithContext(ctx).Model(&eventRow{}).Order("received_at DESC").Limit(limit)
	q = scopeTenant(q, orgID, userID, "user_id")
	if typ != "" {
		q = q.Where("type = ?", typ)
	}
	var rows []eventRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Event, len(rows))
	for i, r := range rows {
		out[i] = r.toEvent()
	}
	return out, nil
}

// scopeTenant restricts a query to the caller's org (when in an org) or their own records.
// The owner column differs by table — events store it as user_id, triggers as created_by —
// so the caller names it. This is the multi-tenant isolation every read/match honours.
func scopeTenant(q *gorm.DB, orgID, userID, ownerCol string) *gorm.DB {
	if orgID != "" {
		return q.Where("org_id = ?", orgID)
	}
	return q.Where(ownerCol+" = ? AND (org_id = '' OR org_id IS NULL)", userID)
}

// triggersForTenant returns the enabled triggers that could match an event of this tenant.
func triggersForTenant(ctx context.Context, orgID, userID string) ([]Trigger, error) {
	q := connectRead().WithContext(ctx).Model(&Trigger{}).Where("enabled = ?", true)
	q = scopeTenant(q, orgID, userID, "created_by")
	var ts []Trigger
	return ts, q.Find(&ts).Error
}

// Trigger CRUD.
func (t *Trigger) Add(ctx context.Context) error { return connect().WithContext(ctx).Create(t).Error }
func (t *Trigger) Save(ctx context.Context) error {
	return connect().WithContext(ctx).Save(t).Error
}
func getTrigger(ctx context.Context, id, orgID, userID string) (*Trigger, error) {
	var t Trigger
	q := scopeTenant(connectRead().WithContext(ctx).Where("id = ?", id), orgID, userID, "created_by")
	if err := q.First(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}
func listTriggers(ctx context.Context, orgID, userID string) ([]Trigger, error) {
	var ts []Trigger
	q := scopeTenant(connectRead().WithContext(ctx).Model(&Trigger{}).Order("created_at DESC"), orgID, userID, "created_by")
	return ts, q.Find(&ts).Error
}
func removeTrigger(ctx context.Context, id, orgID, userID string) error {
	q := scopeTenant(connect().WithContext(ctx).Where("id = ?", id), orgID, userID, "created_by")
	return q.Delete(&Trigger{}).Error
}

// dispatchRow tracks one (trigger, event) delivery for at-least-once + retry. The unique
// (trigger_id, event_id) index is the idempotency guard against double-firing on redelivery.
type dispatchRow struct {
	ID            string     `gorm:"primaryKey"`
	TriggerID     string     `gorm:"uniqueIndex:uq_dispatch,priority:1"`
	EventID       string     `gorm:"uniqueIndex:uq_dispatch,priority:2"`
	OrgID         string     ``
	Status        string     `gorm:"index"` // pending | done | failed | dead
	Attempts      int        ``
	LastError     string     ``
	NextAttemptAt *time.Time `gorm:"index"`
	CreatedAt     time.Time  `gorm:"autoCreateTime"`
	UpdatedAt     time.Time  `gorm:"autoUpdateTime"`
}

func migrate(ctx context.Context) error {
	return connect().WithContext(ctx).AutoMigrate(&eventRow{}, &Trigger{}, &dispatchRow{})
}
