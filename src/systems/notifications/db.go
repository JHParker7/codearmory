package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// clauseSkipLocked makes a SELECT take row locks and skip rows already locked by
// another worker, so concurrent replicas never claim the same notification.
var clauseSkipLocked = clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}

// db is the common interface implemented by all persistent entities.
type db interface {
	Add(ctx context.Context) error
	Update(ctx context.Context) error
	Remove(ctx context.Context) error
	Get(ctx context.Context) (db, error)
	List(ctx context.Context, limit, offset int) ([]db, error)
}

var gormDB *gorm.DB
var gormDBRead *gorm.DB
var dbInitMu sync.Mutex

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/notifications")), &gorm.Config{
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
	readURL := secret("DATABASE_READ_URL")
	if readURL == "" {
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/notifications")
	}
	conn, err := gorm.Open(postgres.Open(readURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to read database", "error", err)
		os.Exit(1)
	}
	gormDBRead = conn
	return gormDBRead
}

// isNotFound returns true when err is a GORM record-not-found error.
func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// ── Channel ─────────────────────────────────────────────────────────────────

func (c Channel) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.channel.add")
	defer span.End()
	span.SetAttributes(attribute.String("channel.id", c.ChannelID))
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c Channel) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.channel.update")
	defer span.End()
	span.SetAttributes(attribute.String("channel.id", c.ChannelID))
	c.UpdatedAt = time.Now().UTC()
	// Save issues a full-row update by primary key, so an explicit Enabled=false
	// is persisted (unlike Create, which would drop the zero value for a column
	// carrying a default).
	if err := connect().WithContext(ctx).Save(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c Channel) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.channel.remove")
	defer span.End()
	span.SetAttributes(attribute.String("channel.id", c.ChannelID))
	if err := connect().WithContext(ctx).Model(&Channel{}).
		Where("channel_id=? AND active=?", c.ChannelID, true).
		Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c Channel) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.channel.get")
	defer span.End()
	span.SetAttributes(attribute.String("channel.id", c.ChannelID))
	var result Channel
	if err := connectRead().WithContext(ctx).Where("channel_id=? AND active=?", c.ChannelID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (c Channel) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.channel.list")
	defer span.End()
	var channels []Channel
	c.Active = true
	q := connectRead().WithContext(ctx).Where(c).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&channels).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(channels))
	for i, ch := range channels {
		result[i] = ch
	}
	return result, nil
}

// getChannel returns a single active channel by ID.
func getChannel(ctx context.Context, id string) (Channel, error) {
	row, err := (Channel{ChannelID: id}).Get(ctx)
	if err != nil {
		return Channel{}, err
	}
	return row.(Channel), nil
}

// listChannels returns active channels visible to the caller: their own plus any
// in their org. An optional enabledOnly filter restricts to enabled channels.
func listChannels(ctx context.Context, userID, orgID string, enabledOnly bool) ([]Channel, error) {
	q := connectRead().WithContext(ctx).
		Where("active = ? AND (created_by = ? OR (org_id != '' AND org_id = ?))", true, userID, orgID)
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	var channels []Channel
	if err := q.Order("created_at DESC").Limit(200).Find(&channels).Error; err != nil {
		return nil, err
	}
	if channels == nil {
		channels = []Channel{}
	}
	return channels, nil
}

// ── Notification ──────────────────────────────────────────────────────────────

func (n Notification) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.notification.add")
	defer span.End()
	span.SetAttributes(attribute.String("notification.id", n.NotificationID))
	if err := connect().WithContext(ctx).Create(&n).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (n Notification) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.notification.update")
	defer span.End()
	span.SetAttributes(attribute.String("notification.id", n.NotificationID))
	n.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&n).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (n Notification) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.notification.remove")
	defer span.End()
	if err := connect().WithContext(ctx).
		Where("notification_id=?", n.NotificationID).
		Delete(&Notification{}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (n Notification) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.notification.get")
	defer span.End()
	span.SetAttributes(attribute.String("notification.id", n.NotificationID))
	var result Notification
	if err := connectRead().WithContext(ctx).Where("notification_id=?", n.NotificationID).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (n Notification) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("notifications").Start(ctx, "db.notification.list")
	defer span.End()
	var notifications []Notification
	q := connectRead().WithContext(ctx).Where(n).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&notifications).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(notifications))
	for i, nt := range notifications {
		result[i] = nt
	}
	return result, nil
}

// getNotification returns a single notification by ID.
func getNotification(ctx context.Context, id string) (Notification, error) {
	row, err := (Notification{NotificationID: id}).Get(ctx)
	if err != nil {
		return Notification{}, err
	}
	return row.(Notification), nil
}

// listNotifications returns delivery records visible to the caller, newest first,
// optionally filtered by status.
func listNotifications(ctx context.Context, userID, orgID, statusFilter string) ([]Notification, error) {
	q := connectRead().WithContext(ctx).
		Where("created_by = ? OR (org_id != '' AND org_id = ?)", userID, orgID)
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	var notifications []Notification
	if err := q.Order("created_at DESC").Limit(200).Find(&notifications).Error; err != nil {
		return nil, err
	}
	if notifications == nil {
		notifications = []Notification{}
	}
	return notifications, nil
}

// claimRetryable atomically claims up to limit notifications that are due for a
// (re)send — pending or failed rows that have not exhausted their attempts. It
// selects with FOR UPDATE SKIP LOCKED and bumps attempts in the same
// transaction, so concurrent workers never deliver the same row twice and a
// crashed worker's row is eventually retried (not stuck) up to maxAttempts. The
// returned structs carry the incremented attempt count.
func claimRetryable(ctx context.Context, limit int) ([]Notification, error) {
	var out []Notification
	err := connect().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clauseSkipLocked).
			Where("status IN ? AND attempts < ?", []string{StatusPending, StatusFailed}, maxAttempts).
			Order("created_at").Limit(limit).Find(&out).Error; err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		ids := make([]string, len(out))
		for i := range out {
			ids[i] = out[i].NotificationID
		}
		now := time.Now().UTC()
		if err := tx.Model(&Notification{}).Where("notification_id IN ?", ids).
			Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "updated_at": now}).Error; err != nil {
			return err
		}
		for i := range out {
			out[i].Attempts++
			out[i].UpdatedAt = now
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
