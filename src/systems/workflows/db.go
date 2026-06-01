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
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/workflows")), &gorm.Config{
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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/workflows")
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

// ── Step ──────────────────────────────────────────────────────────────────────

func (s Step) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step.add")
	defer span.End()
	span.SetAttributes(attribute.String("step.id", s.StepID))
	if err := connect().WithContext(ctx).Create(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Step) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step.update")
	defer span.End()
	span.SetAttributes(attribute.String("step.id", s.StepID))
	s.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&s).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Step) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step.remove")
	defer span.End()
	span.SetAttributes(attribute.String("step.id", s.StepID))
	if err := connect().WithContext(ctx).Model(&Step{}).
		Where("step_id=? AND active=?", s.StepID, true).
		Updates(map[string]any{"active": false, "updated_at": time.Now().UTC()}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s Step) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step.get")
	defer span.End()
	span.SetAttributes(attribute.String("step.id", s.StepID))
	var result Step
	if err := connectRead().WithContext(ctx).Where("step_id=? AND active=?", s.StepID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (s Step) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step.list")
	defer span.End()
	var steps []Step
	s.Active = true
	q := connectRead().WithContext(ctx).Where(s)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&steps).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(steps))
	for i, st := range steps {
		result[i] = st
	}
	return result, nil
}

// getStep returns a single active step by ID.
func getStep(ctx context.Context, id string) (Step, error) {
	row, err := (Step{StepID: id}).Get(ctx)
	if err != nil {
		return Step{}, err
	}
	return row.(Step), nil
}

// listSteps returns active steps accessible to the caller, with an optional name filter.
func listSteps(ctx context.Context, userID, orgID, nameFilter string) ([]Step, error) {
	q := connectRead().WithContext(ctx).
		Where("active=? AND (created_by=? OR (org_id!='' AND org_id=?))", true, userID, orgID).
		Order("name ASC").
		Limit(200)
	if nameFilter != "" {
		q = q.Where("name=?", nameFilter)
	}
	var steps []Step
	if err := q.Find(&steps).Error; err != nil {
		return nil, err
	}
	if steps == nil {
		steps = []Step{}
	}
	return steps, nil
}

// stepNameExists reports whether an active step with the given name already
// exists and is accessible to the caller.
func stepNameExists(ctx context.Context, name, userID, orgID string) (bool, error) {
	var existing Step
	err := connectRead().WithContext(ctx).
		Where("name=? AND active=true AND (created_by=? OR (org_id!='' AND org_id=?))", name, userID, orgID).
		First(&existing).Error
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return false, err
}

// getStepsByIDs returns active steps matching the given IDs.
func getStepsByIDs(ctx context.Context, ids []string) ([]Step, error) {
	var steps []Step
	if err := connectRead().WithContext(ctx).Where("step_id IN ? AND active=?", ids, true).Find(&steps).Error; err != nil {
		return nil, err
	}
	return steps, nil
}

// cancelRun transitions a pending or running run to cancelled status.
// Returns the number of rows affected (0 if the run was not in a cancellable state).
func cancelRun(ctx context.Context, id string) (int64, error) {
	result := connect().WithContext(ctx).Exec(
		"UPDATE workflow_runs SET status='cancelled', ended_at=now(), token=NULL WHERE run_id=? AND status IN ('pending','running')", id,
	)
	return result.RowsAffected, result.Error
}

// ── Workflow ──────────────────────────────────────────────────────────────────

func (wf Workflow) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow.add")
	defer span.End()
	span.SetAttributes(attribute.String("workflow.id", wf.WorkflowID))
	if err := connect().WithContext(ctx).Create(&wf).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (wf Workflow) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow.update")
	defer span.End()
	span.SetAttributes(attribute.String("workflow.id", wf.WorkflowID))
	wf.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&wf).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (wf Workflow) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow.remove")
	defer span.End()
	span.SetAttributes(attribute.String("workflow.id", wf.WorkflowID))
	if err := connect().WithContext(ctx).Model(&Workflow{}).
		Where("workflow_id=? AND active=?", wf.WorkflowID, true).
		Updates(map[string]any{"active": false, "updated_at": time.Now().UTC()}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (wf Workflow) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow.get")
	defer span.End()
	span.SetAttributes(attribute.String("workflow.id", wf.WorkflowID))
	var result Workflow
	if err := connectRead().WithContext(ctx).Where("workflow_id=? AND active=?", wf.WorkflowID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (wf Workflow) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow.list")
	defer span.End()
	var workflows []Workflow
	wf.Active = true
	q := connectRead().WithContext(ctx).Where(wf)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&workflows).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(workflows))
	for i, w := range workflows {
		result[i] = w
	}
	return result, nil
}

// getWorkflow fetches an active workflow and enriches it with full step definitions.
func getWorkflow(ctx context.Context, id string) (Workflow, error) {
	row, err := (Workflow{WorkflowID: id}).Get(ctx)
	if err != nil {
		return Workflow{}, err
	}
	wf := row.(Workflow)
	steps, err := enrichStepRefs(ctx, wf.StepRefs)
	if err != nil {
		return Workflow{}, err
	}
	wf.Steps = steps
	return wf, nil
}

// listWorkflows returns active workflows accessible to the caller.
func listWorkflows(ctx context.Context, userID, orgID string) ([]Workflow, error) {
	var wfs []Workflow
	if err := connectRead().WithContext(ctx).
		Where("active=? AND (created_by=? OR (org_id!='' AND org_id=?))", true, userID, orgID).
		Order("created_at desc").
		Limit(100).
		Find(&wfs).Error; err != nil {
		return nil, err
	}
	if wfs == nil {
		wfs = []Workflow{}
	}
	return wfs, nil
}

// enrichStepRefs looks up the full Step definition for each ref and assembles
// WorkflowStep objects. Deleted steps are omitted.
func enrichStepRefs(ctx context.Context, refs []WorkflowStepRef) ([]WorkflowStep, error) {
	if len(refs) == 0 {
		return []WorkflowStep{}, nil
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.StepID
	}
	var dbSteps []Step
	if err := connectRead().WithContext(ctx).Where("step_id IN ? AND active=?", ids, true).Find(&dbSteps).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]Step, len(dbSteps))
	for _, s := range dbSteps {
		byID[s.StepID] = s
	}
	result := make([]WorkflowStep, 0, len(refs))
	for _, ref := range refs {
		s, ok := byID[ref.StepID]
		if !ok {
			continue
		}
		result = append(result, WorkflowStep{Step: s, ParallelGroup: ref.ParallelGroup})
	}
	return result, nil
}

// ── WorkflowRun ───────────────────────────────────────────────────────────────

func (run WorkflowRun) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow_run.add")
	defer span.End()
	span.SetAttributes(attribute.String("run.id", run.RunID))
	if err := connect().WithContext(ctx).Create(&run).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (run WorkflowRun) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow_run.update")
	defer span.End()
	span.SetAttributes(attribute.String("run.id", run.RunID))
	if err := connect().WithContext(ctx).Save(&run).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (run WorkflowRun) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow_run.remove")
	defer span.End()
	span.SetAttributes(attribute.String("run.id", run.RunID))
	if err := connect().WithContext(ctx).Exec(
		"UPDATE workflow_runs SET status='cancelled', ended_at=now(), token=NULL WHERE run_id=? AND status IN ('pending','running')", run.RunID,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (run WorkflowRun) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow_run.get")
	defer span.End()
	span.SetAttributes(attribute.String("run.id", run.RunID))
	var result WorkflowRun
	if err := connectRead().WithContext(ctx).Where("run_id=?", run.RunID).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (run WorkflowRun) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow_run.list")
	defer span.End()
	var runs []WorkflowRun
	q := connectRead().WithContext(ctx).Where(run)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&runs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(runs))
	for i, r := range runs {
		result[i] = r
	}
	return result, nil
}

// getRun returns a single workflow run by ID.
func getRun(ctx context.Context, id string) (WorkflowRun, error) {
	row, err := (WorkflowRun{RunID: id}).Get(ctx)
	if err != nil {
		return WorkflowRun{}, err
	}
	return row.(WorkflowRun), nil
}

// listRuns returns workflow runs accessible to the caller, with an optional workflow filter.
func listRuns(ctx context.Context, userID, orgID, workflowID string) ([]WorkflowRun, error) {
	q := connectRead().WithContext(ctx).
		Where("triggered_by=? OR (org_id != '' AND org_id = ?)", userID, orgID)
	if workflowID != "" {
		q = q.Where("workflow_id=?", workflowID)
	}
	var runs []WorkflowRun
	if err := q.Order("created_at DESC").Limit(100).Find(&runs).Error; err != nil {
		return nil, err
	}
	if runs == nil {
		runs = []WorkflowRun{}
	}
	return runs, nil
}

// getStepRuns returns all step runs for a given workflow run ordered by step index.
func getStepRuns(ctx context.Context, runID string) ([]WorkflowStepRun, error) {
	var stepRuns []WorkflowStepRun
	if err := connectRead().WithContext(ctx).Raw(
		`SELECT step_run_id, run_id, step_index, step_name, status,
		        response_body, started_at, ended_at
		 FROM workflow_step_runs WHERE run_id=? ORDER BY step_index`, runID,
	).Scan(&stepRuns).Error; err != nil {
		return nil, err
	}
	if stepRuns == nil {
		stepRuns = []WorkflowStepRun{}
	}
	return stepRuns, nil
}

