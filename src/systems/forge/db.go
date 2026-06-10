package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

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
	gormDB   *gorm.DB
	gormDBMu sync.Mutex
)

func connect() *gorm.DB {
	gormDBMu.Lock()
	defer gormDBMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/forge")), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: connect to database: %v\n", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

// ── Execution ─────────────────────────────────────────────────────────────────

func (e Execution) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.execution.add")
	defer span.End()
	span.SetAttributes(attribute.String("execution.id", e.ExecutionID))
	if err := connect().WithContext(ctx).Create(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (e Execution) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.execution.update")
	defer span.End()
	span.SetAttributes(attribute.String("execution.id", e.ExecutionID))
	if err := connect().WithContext(ctx).Save(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (e Execution) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.execution.remove")
	defer span.End()
	span.SetAttributes(attribute.String("execution.id", e.ExecutionID))
	result := connect().WithContext(ctx).Where("execution_id = ?", e.ExecutionID).Delete(&Execution{})
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

// Get finds the execution by ExecutionID and UserID (both must be set on the receiver).
func (e Execution) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.execution.get")
	defer span.End()
	span.SetAttributes(attribute.String("execution.id", e.ExecutionID))
	var out Execution
	if err := connect().WithContext(ctx).
		Where("execution_id = ? AND user_id = ?", e.ExecutionID, e.UserID).
		First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

// List returns executions for the UserID on the receiver, ordered most recent first.
// A limit <= 0 defaults to 100.
func (e Execution) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.execution.list")
	defer span.End()
	span.SetAttributes(attribute.String("user.id", e.UserID))
	if limit <= 0 {
		limit = 100
	}
	var executions []Execution
	q := connect().WithContext(ctx).
		Select("execution_id, user_id, image, status, exit_code, created_at, started_at, ended_at, runner_class").
		Where("user_id = ?", e.UserID).
		Order("created_at DESC").
		Limit(limit)
	if offset > 0 {
		q = q.Offset(offset)
	}
	if err := q.Find(&executions).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(executions))
	for i, ex := range executions {
		rows[i] = ex
	}
	return rows, nil
}

// Cancel sets a pending execution to cancelled. Returns false when the execution
// was not in pending status (already transitioned or not found).
func (e Execution) Cancel(ctx context.Context) (bool, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.execution.cancel")
	defer span.End()
	span.SetAttributes(attribute.String("execution.id", e.ExecutionID))
	result := connect().WithContext(ctx).Exec(
		`UPDATE executions SET status = 'cancelled', ended_at = now() WHERE execution_id = ? AND status = 'pending'`,
		e.ExecutionID,
	)
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		return false, result.Error
	}
	span.SetStatus(codes.Ok, "")
	return result.RowsAffected > 0, nil
}

// Complete records the final status and output of a finished execution.
// Always uses context.Background() internally: the worker context may already
// be cancelled on shutdown or user cancel, but the result must always be persisted.
func (e Execution) Complete(_ context.Context, status string, result RunResult) error {
	_, span := otel.Tracer("forge").Start(context.Background(), "db.execution.complete")
	defer span.End()
	span.SetAttributes(
		attribute.String("execution.id", e.ExecutionID),
		attribute.String("status", status),
	)
	if err := connect().WithContext(context.Background()).Exec(`
		UPDATE executions
		SET status = ?, exit_code = ?, stdout = ?, stderr = ?, ended_at = now()
		WHERE execution_id = ?`,
		status, result.ExitCode, result.Stdout, result.Stderr, e.ExecutionID,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// claimPendingExecution atomically dequeues one pending execution and marks it
// as running. Returns the claimed execution and true on success; false when no
// pending work is available.
func claimPendingExecution(ctx context.Context) (Execution, bool) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.claim_pending_execution")
	defer span.End()

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		return Execution{}, false
	}

	// pendingRow holds the raw JSONB bytes for manual unmarshal; using []byte avoids
	// needing GORM's serializer in a raw-SQL scan path.
	type pendingRow struct {
		ExecutionID string `gorm:"column:execution_id"`
		UserID      string `gorm:"column:user_id"`
		Image       string `gorm:"column:image"`
		Command     []byte `gorm:"column:command"`
		Env         []byte `gorm:"column:env"`
		TimeoutSecs int64  `gorm:"column:timeout_secs"`
		RunnerClass string `gorm:"column:runner_class"`
	}
	var raw pendingRow

	// FOR UPDATE SKIP LOCKED lets multiple workers run in parallel: each goroutine
	// locks exactly one pending row and skips any already locked by a sibling.
	result := tx.Raw(`
		SELECT execution_id, user_id, image, command, env, timeout_secs, runner_class
		FROM executions WHERE status = 'pending' ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED
	`).Scan(&raw)
	if result.Error != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		slog.Error("worker: query pending row", "error", result.Error)
		return Execution{}, false
	}
	if result.RowsAffected == 0 {
		tx.Rollback() //nolint:errcheck
		span.SetStatus(codes.Ok, "")
		return Execution{}, false
	}

	var exec Execution
	exec.ExecutionID = raw.ExecutionID
	exec.UserID = raw.UserID
	exec.Image = raw.Image
	exec.TimeoutSecs = raw.TimeoutSecs
	exec.RunnerClass = raw.RunnerClass

	if err := json.Unmarshal(raw.Command, &exec.Command); err != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("worker: unmarshal command", "execution_id", exec.ExecutionID, "error", err)
		return Execution{}, false
	}
	if err := json.Unmarshal(raw.Env, &exec.Env); err != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("worker: unmarshal env", "execution_id", exec.ExecutionID, "error", err)
		return Execution{}, false
	}

	if err := tx.Exec(`UPDATE executions SET status = 'running', started_at = now() WHERE execution_id = ?`, exec.ExecutionID).Error; err != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("worker: mark running", "execution_id", exec.ExecutionID, "error", err)
		return Execution{}, false
	}
	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return Execution{}, false
	}

	span.SetAttributes(attribute.String("execution.id", exec.ExecutionID))
	span.SetStatus(codes.Ok, "")
	return exec, true
}

// ── RunnerClass ───────────────────────────────────────────────────────────────

func (rc RunnerClass) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runnerclass.add")
	defer span.End()
	span.SetAttributes(attribute.String("runner_class.name", rc.Name))
	if err := connect().WithContext(ctx).Create(&rc).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (rc RunnerClass) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runnerclass.update")
	defer span.End()
	span.SetAttributes(attribute.String("runner_class.name", rc.Name))
	result := connect().WithContext(ctx).Model(&RunnerClass{}).Where("name = ?", rc.Name).Updates(map[string]any{
		"memory_mb":      rc.MemoryMB,
		"cpu_millicores": rc.CPUMillicores,
		"pids_limit":     rc.PidsLimit,
		"tmpfs_mb":       rc.TmpfsMB,
		"enabled":        rc.Enabled,
	})
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

func (rc RunnerClass) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runnerclass.remove")
	defer span.End()
	span.SetAttributes(attribute.String("runner_class.name", rc.Name))
	result := connect().WithContext(ctx).Where("name = ?", rc.Name).Delete(&RunnerClass{})
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

func (rc RunnerClass) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runnerclass.get")
	defer span.End()
	span.SetAttributes(attribute.String("runner_class.name", rc.Name))
	var out RunnerClass
	if err := connect().WithContext(ctx).Where("name = ?", rc.Name).First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (rc RunnerClass) List(ctx context.Context, _ int, _ int) ([]db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runnerclass.list")
	defer span.End()
	var classes []RunnerClass
	if err := connect().WithContext(ctx).Order("name").Find(&classes).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(classes))
	for i, c := range classes {
		rows[i] = c
	}
	return rows, nil
}
