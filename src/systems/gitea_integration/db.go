package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

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
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/gitea_integration")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to connect to database: %v\n", err)
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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/gitea_integration")
	}
	conn, err := gorm.Open(postgres.Open(readURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to connect to read database: %v\n", err)
		os.Exit(1)
	}
	gormDBRead = conn
	return gormDBRead
}

// ── GiteaAccount ─────────────────────────────────────────────────────────────

func (a GiteaAccount) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("gitea").Start(ctx, "db.account.add")
	defer span.End()
	span.SetAttributes(attribute.String("account.user_id", a.UserID))
	if err := connect().WithContext(ctx).Create(&a).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (a GiteaAccount) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("gitea").Start(ctx, "db.account.update")
	defer span.End()
	a.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&a).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (a GiteaAccount) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("gitea").Start(ctx, "db.account.remove")
	defer span.End()
	if err := connect().WithContext(ctx).Where("user_id = ?", a.UserID).Delete(&GiteaAccount{}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (a GiteaAccount) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("gitea").Start(ctx, "db.account.get")
	defer span.End()
	span.SetAttributes(attribute.String("account.user_id", a.UserID))
	var out GiteaAccount
	if err := connectRead().WithContext(ctx).Where("user_id = ?", a.UserID).First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (a GiteaAccount) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("gitea").Start(ctx, "db.account.list")
	defer span.End()
	var accounts []GiteaAccount
	q := connectRead().WithContext(ctx).Order("user_id")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}
	if err := q.Find(&accounts).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(accounts))
	for i, acc := range accounts {
		rows[i] = acc
	}
	return rows, nil
}

func isDbNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
