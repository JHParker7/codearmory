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
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/chaos")), &gorm.Config{
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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/chaos")
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

func (e Experiment) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("chaos").Start(ctx, "db.experiment.add")
	defer span.End()
	span.SetAttributes(attribute.String("experiment.id", e.ExperimentID))
	if err := connect().WithContext(ctx).Create(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (e Experiment) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("chaos").Start(ctx, "db.experiment.update")
	defer span.End()
	span.SetAttributes(attribute.String("experiment.id", e.ExperimentID))
	if err := connect().WithContext(ctx).Save(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (e Experiment) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("chaos").Start(ctx, "db.experiment.remove")
	defer span.End()
	span.SetAttributes(attribute.String("experiment.id", e.ExperimentID))
	if err := connect().WithContext(ctx).Model(&Experiment{}).
		Where("experiment_id=? AND active=?", e.ExperimentID, true).
		Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (e Experiment) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("chaos").Start(ctx, "db.experiment.get")
	defer span.End()
	span.SetAttributes(attribute.String("experiment.id", e.ExperimentID))
	var result Experiment
	if err := connectRead().WithContext(ctx).Where("experiment_id=? AND active=?", e.ExperimentID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (e Experiment) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("chaos").Start(ctx, "db.experiment.list")
	defer span.End()
	var experiments []Experiment
	e.Active = true
	q := connectRead().WithContext(ctx).Where(e).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&experiments).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(experiments))
	for i, ex := range experiments {
		result[i] = ex
	}
	return result, nil
}

// applyVerdict transitions an experiment to a terminal verdict state reported by
// the outpost chaos module. It is idempotent: a verdict for an already-terminal
// experiment is ignored so duplicate at-least-once events are harmless. Verdicts
// are matched by experiment_id, which the command carried into the cluster.
func applyVerdict(ctx context.Context, experimentID, verdict, phase, failStep, probeSuccess string) (Experiment, bool, error) {
	ctx, span := otel.Tracer("chaos").Start(ctx, "db.experiment.applyVerdict")
	defer span.End()
	span.SetAttributes(attribute.String("experiment.id", experimentID))

	var exp Experiment
	if err := connect().WithContext(ctx).Where("experiment_id=? AND active=?", experimentID, true).First(&exp).Error; err != nil {
		span.RecordError(err)
		return Experiment{}, false, err
	}
	if isTerminal(exp.Status) {
		span.SetStatus(codes.Ok, "already terminal")
		return exp, false, nil
	}

	status := verdictToStatus(verdict, phase)
	now := time.Now().UTC()
	updates := map[string]any{
		"verdict":       verdict,
		"fail_step":     failStep,
		"probe_success": probeSuccess,
		"status":        status,
	}
	if exp.StartedAt == nil {
		updates["started_at"] = now
	}
	if isTerminal(status) {
		updates["ended_at"] = now
	}
	// Compare-and-swap on the status we observed: events are at-least-once and may
	// arrive concurrently, so gate the write on the row still being in the status
	// we read. If a peer already advanced it, RowsAffected is 0 and we report no
	// change — otherwise the resolved metric and hooks would fire more than once.
	res := connect().WithContext(ctx).Model(&Experiment{}).
		Where("experiment_id=? AND active=? AND status=?", experimentID, true, exp.Status).
		Updates(updates)
	if res.Error != nil {
		span.RecordError(res.Error)
		span.SetStatus(codes.Error, res.Error.Error())
		return Experiment{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		span.SetStatus(codes.Ok, "no-op (already advanced)")
		return exp, false, nil
	}

	exp.Status = status
	exp.Verdict = verdict
	exp.FailStep = failStep
	exp.ProbeSuccess = probeSuccess
	span.SetStatus(codes.Ok, "")
	return exp, true, nil
}

// markRunning flips a pending experiment to running once the outpost
// acknowledges the command (the run-started event).
func markRunning(ctx context.Context, experimentID string) (Experiment, bool, error) {
	var exp Experiment
	if err := connect().WithContext(ctx).Where("experiment_id=? AND active=?", experimentID, true).First(&exp).Error; err != nil {
		return Experiment{}, false, err
	}
	if exp.Status != StatusPending {
		return exp, false, nil
	}
	now := time.Now().UTC()
	if err := connect().WithContext(ctx).Model(&Experiment{}).
		Where("experiment_id=? AND status=?", experimentID, StatusPending).
		Updates(map[string]any{"status": StatusRunning, "started_at": now}).Error; err != nil {
		return Experiment{}, false, err
	}
	exp.Status = StatusRunning
	exp.StartedAt = &now
	return exp, true, nil
}
