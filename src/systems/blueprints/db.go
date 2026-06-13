package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// db is the common interface implemented by all persistent entities.
type db interface {
	Add(ctx context.Context) error
	Update(ctx context.Context) error
	Remove(ctx context.Context) error
	Get(ctx context.Context) (db, error)
	List(ctx context.Context, limit, offset int) ([]db, error)
}

var (
	// Sentinel errors returned by lock operations so handlers can map them to
	// the correct HTTP status codes without inspecting raw error strings.
	ErrWorkspaceLocked = errors.New("workspace is locked")
	ErrLockIDMismatch  = errors.New("lock ID mismatch")
	ErrAlreadyLocked   = errors.New("workspace already locked")
)

// State stores encrypted Terraform state blobs keyed by workspace.
type State struct {
	Workspace string    `gorm:"column:workspace;primaryKey"`
	Data      []byte    `gorm:"column:data;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;default:now()"`
}

// StateLock holds the exclusive lock record for a workspace.
type StateLock struct {
	Workspace string    `gorm:"column:workspace;primaryKey"`
	LockData  string    `gorm:"column:lock_data;not null"`
	CreatedAt time.Time `gorm:"column:created_at;default:now()"`
	UpdatedAt time.Time `gorm:"column:updated_at;default:now()"`
}

func (StateLock) TableName() string { return "locks" }

// BackendCredential is an ephemeral mTLS/token credential for a workspace backend.
type BackendCredential struct {
	CredentialID string    `gorm:"column:credential_id;primaryKey"`
	Workspace    string    `gorm:"column:workspace;not null"`
	CertFP       string    `gorm:"column:cert_fp;not null;uniqueIndex"`
	TokenHash    string    `gorm:"column:token_hash;not null;uniqueIndex"`
	CreatedBy    string    `gorm:"column:created_by;not null"`
	ExpiresAt    time.Time `gorm:"column:expires_at;not null;index:idx_backend_creds_expires"`
	CreatedAt    time.Time `gorm:"column:created_at;not null;default:now()"`
}

var (
	gormDB   *gorm.DB
	gormDBMu sync.Mutex
)

func connect() *gorm.DB {
	gormDBMu.Lock()
	defer gormDBMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	dsn := secret("DATABASE_URL")
	if dsn == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	conn, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		slog.Error("blueprints: connect to database", "error", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

// ── State ─────────────────────────────────────────────────────────────────────

func (s State) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.state.add")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", s.Workspace))
	if err := connect().WithContext(ctx).Create(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s State) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.state.update")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", s.Workspace))
	if err := connect().WithContext(ctx).Save(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s State) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.state.remove")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", s.Workspace))
	if err := connect().WithContext(ctx).Exec("DELETE FROM states WHERE workspace = ?", s.Workspace).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Get retrieves the state for the workspace set on the receiver.
// Returns (nil, gorm.ErrRecordNotFound) when no row exists.
func (s State) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.state.get")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", s.Workspace))
	var out State
	result := connect().WithContext(ctx).Where("workspace = ?", s.Workspace).First(&out)
	if result.RowsAffected == 0 {
		span.SetStatus(codes.Ok, "")
		return nil, gorm.ErrRecordNotFound
	}
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		return nil, result.Error
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (s State) List(_ context.Context, _, _ int) ([]db, error) {
	return nil, errors.New("state.List not implemented")
}

// UpsertAtomic writes encrypted state data within a transaction that checks the
// workspace lock. On success it returns ("", nil). On a lock conflict it returns
// (existingLockJSON, ErrWorkspaceLocked or ErrLockIDMismatch).
func (s State) UpsertAtomic(ctx context.Context, data []byte, lockID string) (string, error) {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.state.upsertAtomic")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", s.Workspace))

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		return "", tx.Error
	}
	defer tx.Rollback() //nolint:errcheck

	var existingLock string
	// FOR UPDATE serializes concurrent requests on the same workspace row, preventing
	// TOCTOU races between the lock check and the subsequent state write.
	lockResult := tx.Raw("SELECT lock_data FROM locks WHERE workspace = ? FOR UPDATE", s.Workspace).Scan(&existingLock)
	if lockResult.Error != nil {
		span.RecordError(lockResult.Error)
		span.SetStatus(codes.Error, lockResult.Error.Error())
		return "", lockResult.Error
	}

	if lockResult.RowsAffected > 0 {
		if lockID == "" {
			span.SetStatus(codes.Ok, "")
			return existingLock, ErrWorkspaceLocked
		}
		var lockObj map[string]any
		if err := json.Unmarshal([]byte(existingLock), &lockObj); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "corrupt lock data")
			return "", fmt.Errorf("corrupt lock data: %w", err)
		}
		if id, _ := lockObj["ID"].(string); subtle.ConstantTimeCompare([]byte(id), []byte(lockID)) != 1 {
			span.SetStatus(codes.Ok, "")
			return existingLock, ErrLockIDMismatch
		}
	}

	if err := tx.Exec(
		`INSERT INTO states (workspace, data) VALUES (?, ?) ON CONFLICT (workspace) DO UPDATE SET data = ?, updated_at = now()`,
		s.Workspace, data, data,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	span.SetStatus(codes.Ok, "")
	return "", nil
}

// ── StateLock ─────────────────────────────────────────────────────────────────

func (sl StateLock) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.lock.add")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", sl.Workspace))
	if err := connect().WithContext(ctx).Create(&sl).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (sl StateLock) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.lock.update")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", sl.Workspace))
	if err := connect().WithContext(ctx).Save(&sl).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (sl StateLock) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.lock.remove")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", sl.Workspace))
	if err := connect().WithContext(ctx).Exec("DELETE FROM locks WHERE workspace = ?", sl.Workspace).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (sl StateLock) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.lock.get")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", sl.Workspace))
	var out StateLock
	result := connect().WithContext(ctx).Where("workspace = ?", sl.Workspace).First(&out)
	if result.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		return nil, result.Error
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (sl StateLock) List(_ context.Context, _, _ int) ([]db, error) {
	return nil, errors.New("stateLock.List not implemented")
}

// LockAtomic acquires an exclusive workspace lock within a transaction. On
// success it returns ("", nil). Returns (existingLockJSON, ErrAlreadyLocked)
// when the workspace is already locked by another holder.
func (sl StateLock) LockAtomic(ctx context.Context, data string) (string, error) {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.lock.lockAtomic")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", sl.Workspace))

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		return "", tx.Error
	}
	defer tx.Rollback() //nolint:errcheck

	var existingLock string
	// FOR UPDATE serializes concurrent lock acquisitions on the same workspace.
	lockResult := tx.Raw("SELECT lock_data FROM locks WHERE workspace = ? FOR UPDATE", sl.Workspace).Scan(&existingLock)
	if lockResult.Error != nil {
		span.RecordError(lockResult.Error)
		span.SetStatus(codes.Error, lockResult.Error.Error())
		return "", lockResult.Error
	}

	if lockResult.RowsAffected > 0 {
		span.SetStatus(codes.Ok, "")
		return existingLock, ErrAlreadyLocked
	}

	if err := tx.Exec("INSERT INTO locks (workspace, lock_data) VALUES (?, ?)", sl.Workspace, data).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	span.SetStatus(codes.Ok, "")
	return "", nil
}

// UnlockAtomic releases an exclusive workspace lock within a transaction. It is
// idempotent: if the workspace is already unlocked the call succeeds. Returns
// ErrLockIDMismatch when the supplied reqID does not match the stored lock ID.
func (sl StateLock) UnlockAtomic(ctx context.Context, reqID string) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.lock.unlockAtomic")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", sl.Workspace))

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		return tx.Error
	}
	defer tx.Rollback() //nolint:errcheck

	var existingLock string
	// FOR UPDATE serializes concurrent unlock attempts on the same workspace row.
	lockResult := tx.Raw("SELECT lock_data FROM locks WHERE workspace = ? FOR UPDATE", sl.Workspace).Scan(&existingLock)
	if lockResult.Error != nil {
		span.RecordError(lockResult.Error)
		span.SetStatus(codes.Error, lockResult.Error.Error())
		return lockResult.Error
	}

	if lockResult.RowsAffected > 0 {
		var lockData map[string]any
		if err := json.Unmarshal([]byte(existingLock), &lockData); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "corrupt lock data")
			return fmt.Errorf("corrupt lock data: %w", err)
		}
		storedID, _ := lockData["ID"].(string)
		if reqID == "" || subtle.ConstantTimeCompare([]byte(storedID), []byte(reqID)) != 1 {
			span.SetStatus(codes.Ok, "")
			return ErrLockIDMismatch
		}

		if err := tx.Exec("DELETE FROM locks WHERE workspace = ?", sl.Workspace).Error; err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
	}

	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// ── BackendCredential ─────────────────────────────────────────────────────────

func (bc BackendCredential) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.credential.add")
	defer span.End()
	span.SetAttributes(
		attribute.String("credential.id", bc.CredentialID),
		attribute.String("workspace", bc.Workspace),
	)
	if err := connect().WithContext(ctx).Create(&bc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (bc BackendCredential) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.credential.update")
	defer span.End()
	if err := connect().WithContext(ctx).Save(&bc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (bc BackendCredential) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("blueprints").Start(ctx, "db.credential.remove")
	defer span.End()
	result := connect().WithContext(ctx).Where("credential_id = ?", bc.CredentialID).Delete(&BackendCredential{})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (bc BackendCredential) Get(_ context.Context) (db, error) {
	return nil, errors.New("BackendCredential.Get not implemented; use CheckCert or CheckToken")
}

func (bc BackendCredential) List(_ context.Context, _, _ int) ([]db, error) {
	return nil, errors.New("BackendCredential.List not implemented")
}

// CheckCert returns true when the CertFP and Workspace on the receiver match a
// non-expired credential record.
func (bc BackendCredential) CheckCert(ctx context.Context) bool {
	var cred BackendCredential
	result := connect().WithContext(ctx).
		Select("expires_at").
		Where("cert_fp = ? AND workspace = ?", bc.CertFP, bc.Workspace).
		First(&cred)
	return result.Error == nil && time.Now().Before(cred.ExpiresAt)
}

// CheckToken returns true when the TokenHash and Workspace on the receiver match
// a non-expired credential record.
func (bc BackendCredential) CheckToken(ctx context.Context) bool {
	var cred BackendCredential
	result := connect().WithContext(ctx).
		Select("expires_at").
		Where("token_hash = ? AND workspace = ?", bc.TokenHash, bc.Workspace).
		First(&cred)
	return result.Error == nil && time.Now().Before(cred.ExpiresAt)
}

func isDbNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
