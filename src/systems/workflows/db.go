package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

// skipLocked applies FOR UPDATE SKIP LOCKED so concurrent workers never dequeue
// the same run twice. It is a Postgres feature; on other dialects (sqlite in unit
// tests, which is single-writer) it is a no-op so the dequeue path stays testable.
func skipLocked(tx *gorm.DB) *gorm.DB {
	if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
	}
	return tx
}

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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/workflows")
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

// cancelRun transitions a pending, running, or awaiting-approval run to cancelled.
// Returns the number of rows affected (0 if the run was not in a cancellable state).
func cancelRun(ctx context.Context, id string) (int64, error) {
	result := connect().WithContext(ctx).Exec(
		"UPDATE workflow_runs SET status='cancelled', ended_at=CURRENT_TIMESTAMP, token=NULL WHERE run_id=? AND status IN ('pending','running','awaiting_approval')", id,
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

// roleInUseByActiveRun reports whether any pending or running run still
// authenticates with roleID. Used to hold off deleting a workflow role that an
// in-flight run's token is scoped to (a mid-run re-provision must not pull the run's
// permissions out from under it). awaiting_approval runs are excluded: they hold no
// live worker and re-mint a fresh token against the current role when they resume.
func roleInUseByActiveRun(ctx context.Context, roleID string) (bool, error) {
	if roleID == "" {
		return false, nil
	}
	var n int64
	if err := connectRead().WithContext(ctx).
		Model(&WorkflowRun{}).
		Where("role_id = ? AND status IN ('pending','running')", roleID).
		Limit(1).
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// runRoleID returns the role a run was triggered with, or "" if unknown.
func runRoleID(ctx context.Context, runID string) string {
	var run WorkflowRun
	if err := connectRead().WithContext(ctx).
		Select("role_id").
		Where("run_id = ?", runID).
		First(&run).Error; err != nil {
		return ""
	}
	return run.RoleID
}

// listWorkflows returns active workflows accessible to the caller: their own, their
// org's, and — via projectIDs, the gatekeeper projects they can reach — any pipeline
// filed into a project they're a member of. projectFilter is a separate optional view
// filter (a slug), unchanged in meaning.
func listWorkflows(ctx context.Context, userID, orgID, projectFilter string, projectIDs []string) ([]Workflow, error) {
	var wfs []Workflow
	q := connectRead().WithContext(ctx)
	if len(projectIDs) > 0 {
		q = q.Where("active=? AND (created_by=? OR (org_id!='' AND org_id=?) OR project_id IN ?)", true, userID, orgID, projectIDs)
	} else {
		q = q.Where("active=? AND (created_by=? OR (org_id!='' AND org_id=?))", true, userID, orgID)
	}
	if projectFilter != "" {
		q = q.Where("project=?", projectFilter)
	}
	if err := q.
		Order("created_at desc").
		Limit(100).
		Find(&wfs).Error; err != nil {
		return nil, err
	}
	if wfs == nil {
		wfs = []Workflow{}
	}
	populateLastRunAt(ctx, wfs)
	return wfs, nil
}

// populateLastRunAt sets each workflow's LastRunAt to the trigger time of its most
// recent run, in a single grouped query over the runs of the listed workflows. It is
// best-effort: a query error leaves LastRunAt nil (the list still renders, just
// without the "last ran" value) rather than failing the whole listing.
func populateLastRunAt(ctx context.Context, wfs []Workflow) {
	if len(wfs) == 0 {
		return
	}
	ids := make([]string, len(wfs))
	for i := range wfs {
		ids[i] = wfs[i].WorkflowID
	}
	type lastRun struct {
		WorkflowID string    `gorm:"column:workflow_id"`
		LastRunAt  time.Time `gorm:"column:last_run_at"`
	}
	var rows []lastRun
	if err := connectRead().WithContext(ctx).
		Model(&WorkflowRun{}).
		Select("workflow_id, MAX(created_at) AS last_run_at").
		Where("workflow_id IN ?", ids).
		Group("workflow_id").
		Scan(&rows).Error; err != nil {
		slog.WarnContext(ctx, "listWorkflows: last-run lookup failed", "error", err)
		return
	}
	byID := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		byID[r.WorkflowID] = r.LastRunAt
	}
	for i := range wfs {
		if t, ok := byID[wfs[i].WorkflowID]; ok {
			at := t
			wfs[i].LastRunAt = &at
		}
	}
}

// enrichStepRefs looks up the full Step definition for each step ref and assembles
// WorkflowStep objects; inline approval gates are synthesised in place (no Step
// lookup). Refs whose stored step was deleted are omitted.
func enrichStepRefs(ctx context.Context, refs []WorkflowStepRef) ([]WorkflowStep, error) {
	if len(refs) == 0 {
		return []WorkflowStep{}, nil
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Approval == nil && r.StepID != "" {
			ids = append(ids, r.StepID)
		}
	}
	byID := make(map[string]Step, len(ids))
	if len(ids) > 0 {
		var dbSteps []Step
		if err := connectRead().WithContext(ctx).Where("step_id IN ? AND active=?", ids, true).Find(&dbSteps).Error; err != nil {
			return nil, err
		}
		for _, s := range dbSteps {
			byID[s.StepID] = s
		}
	}
	result := make([]WorkflowStep, 0, len(refs))
	for _, ref := range refs {
		if ref.Approval != nil {
			ws := synthesiseApprovalStep(ref)
			if ref.Name != "" {
				ws.Name = ref.Name
			}
			result = append(result, ws)
			continue
		}
		// An inline step carries its whole definition on the ref (no stored step to look
		// up) — build the WorkflowStep directly. Must precede the byID lookup below, which
		// would otherwise miss on the empty StepID and silently drop the step.
		if ref.StepID == "" && ref.Action != "" {
			result = append(result, WorkflowStep{
				Step: Step{
					Name: ref.Name, Action: ref.Action, With: ref.With, Timeout: ref.Timeout,
					AllowUnresolved: ref.AllowUnresolved,
					Permissions:     ref.Permissions,
				},
				Matrix:  ref.Matrix,
				Scatter: ref.Scatter,
				MapID:   ref.MapID,
				LoopID:  ref.LoopID,
			})
			continue
		}
		s, ok := byID[ref.StepID]
		if !ok {
			continue
		}
		// A per-occurrence name overrides the step definition's name for this use,
		// so the worker records it as the step-run name and the ${steps.<name>.output}
		// key — s is a local copy, so other occurrences are unaffected.
		if ref.Name != "" {
			s.Name = ref.Name
		}
		// Per-occurrence With overrides (e.g. an input wired to ${steps.X.output})
		// are merged over the step's own With, ref keys winning. The merge is into a
		// fresh map so the shared step definition is never mutated.
		if len(ref.With) > 0 {
			merged := make(map[string]any, len(s.With)+len(ref.With))
			maps.Copy(merged, s.With)
			maps.Copy(merged, ref.With)
			s.With = merged
		}
		// The opt-out is a property of THIS occurrence, not of the shared step
		// definition, so it is copied onto the local step copy like the name override.
		s.AllowUnresolved = ref.AllowUnresolved
		s.Permissions = ref.Permissions
		result = append(result, WorkflowStep{Step: s, Matrix: ref.Matrix, Scatter: ref.Scatter, MapID: ref.MapID, LoopID: ref.LoopID})
	}
	return result, nil
}

// synthesiseApprovalStep builds the WorkflowStep for an inline approval gate: a
// virtual step with Action=approval whose With carries the gate's message and
// approver allow-list, so the worker and approval API treat it exactly like an
// approval step without one existing in the steps table.
func synthesiseApprovalStep(ref WorkflowStepRef) WorkflowStep {
	with := map[string]any{}
	if ref.Approval.Message != "" {
		with["message"] = ref.Approval.Message
	}
	if len(ref.Approval.Approvers) > 0 {
		with["approvers"] = ref.Approval.Approvers
	}
	return WorkflowStep{
		Step:     Step{Name: "approval", Action: ActionApproval, With: with},
		Approval: ref.Approval,
	}
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
		"UPDATE workflow_runs SET status='cancelled', ended_at=CURRENT_TIMESTAMP, token=NULL WHERE run_id=? AND status IN ('pending','running','awaiting_approval')", run.RunID,
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
		        response_body, logs, memory_used_mb, memory_limit_mb, started_at, ended_at
		 FROM workflow_step_runs WHERE run_id=? ORDER BY step_index`, runID,
	).Scan(&stepRuns).Error; err != nil {
		return nil, err
	}
	if stepRuns == nil {
		stepRuns = []WorkflowStepRun{}
	}
	return stepRuns, nil
}

// Dequeue atomically claims one pending workflow run using FOR UPDATE SKIP LOCKED,
// transitions it to 'running', and returns the claimed run.
// Returns nil, nil when the queue is empty or the row is taken by a peer worker.
func (WorkflowRun) Dequeue(ctx context.Context) (*WorkflowRun, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow_run.dequeue")
	defer span.End()

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		return nil, tx.Error
	}
	defer tx.Rollback() //nolint:errcheck

	var run WorkflowRun
	result := skipLocked(tx).
		Where("status = 'pending'").
		Order("created_at").
		Limit(1).
		Find(&run)
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		span.SetStatus(codes.Ok, "")
		return nil, nil
	}

	// The lease is stamped as the run is claimed, not first written by the worker's own
	// heartbeat loop: a run must never sit 'running' with a NULL lease, or a peer's
	// startup recovery would fall back to started_at and could reclaim it.
	r := tx.Exec("UPDATE workflow_runs SET status='running', started_at=CURRENT_TIMESTAMP, heartbeat_at=CURRENT_TIMESTAMP WHERE run_id=?", run.RunID)
	if r.Error != nil {
		span.RecordError(r.Error)
		span.SetStatus(codes.Error, r.Error.Error())
		return nil, r.Error
	}
	if r.RowsAffected == 0 {
		span.SetStatus(codes.Ok, "")
		return nil, nil
	}
	if err := tx.Commit().Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	span.SetAttributes(attribute.String("run.id", run.RunID))
	span.SetStatus(codes.Ok, "")
	return &run, nil
}

// SetCurrentStep records best-effort step progress for a running workflow run.
func (run WorkflowRun) SetCurrentStep(ctx context.Context, step int) {
	connect().WithContext(ctx).Exec( //nolint:errcheck — best-effort progress tracking; failure doesn't affect step execution
		"UPDATE workflow_runs SET current_step=? WHERE run_id=?", step, run.RunID)
}

// SetTicket records the ticket mirroring this run, so a resume after an approval gate
// adopts it instead of opening a second one. Best-effort: mirroring must never fail a
// run, and the worst case of a lost write is a duplicate ticket on resume — not a
// broken pipeline.
func (run WorkflowRun) SetTicket(ctx context.Context, ticketID string) {
	connect().WithContext(ctx).Exec( //nolint:errcheck — best-effort; see doc comment
		"UPDATE workflow_runs SET ticket_id=? WHERE run_id=?", ticketID, run.RunID)
}

// errRunNotAwaiting is returned by the approval transitions when the run is no
// longer paused (already approved/rejected/cancelled), so the API answers 409.
var errRunNotAwaiting = errors.New("run is not awaiting approval")

// PauseForApproval transitions a running run to awaiting_approval, recording the
// step it paused on. The token is intentionally left in place: a paused run keeps
// no live worker, and the approval API re-mints a fresh run token before
// re-queueing. Guarded on status='running' so it never revives a terminal run —
// a no-op (0 rows) when the run was cancelled mid-step is not an error.
func (run WorkflowRun) PauseForApproval(ctx context.Context, step int) error {
	return connect().WithContext(ctx).Exec(
		"UPDATE workflow_runs SET status='awaiting_approval', current_step=? WHERE run_id=? AND status='running'",
		step, run.RunID).Error
}

// resumeAfterApproval marks one approval step run completed and re-queues the
// paused run (awaiting_approval → pending) so a worker resumes it. decision is the
// audit line stored as the step's output (e.g. "approved by alice"). The writes
// share a transaction so a run is never left half-resumed.
//
// A graph run can park on several gates at once, so the run is only re-queued once
// the LAST one is decided; deciding one of several leaves the run paused. The
// step-run update is guarded on status so two simultaneous decisions on the same
// gate cannot both win — the loser updates 0 rows and gets errRunNotAwaiting.
func resumeAfterApproval(ctx context.Context, runID, stepRunID, decision string) error {
	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback() //nolint:errcheck
	sr := tx.Exec(
		`UPDATE workflow_step_runs SET status='completed', response_body=?, ended_at=CURRENT_TIMESTAMP WHERE step_run_id=? AND status=?`,
		decision, stepRunID, StatusAwaitingApproval)
	if sr.Error != nil {
		return sr.Error
	}
	if sr.RowsAffected == 0 {
		return errRunNotAwaiting
	}
	var remaining int64
	if err := tx.Raw(
		`SELECT count(*) FROM workflow_step_runs WHERE run_id=? AND status=?`,
		runID, StatusAwaitingApproval).Scan(&remaining).Error; err != nil {
		return err
	}
	if remaining > 0 {
		// Other branches are still parked; the run stays awaiting_approval.
		return tx.Commit().Error
	}
	r := tx.Exec(`UPDATE workflow_runs SET status='pending' WHERE run_id=? AND status='awaiting_approval'`, runID)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected == 0 {
		return errRunNotAwaiting
	}
	return tx.Commit().Error
}

// rejectAfterApproval marks the approval step run failed and fails the paused run,
// clearing its credentials. decision is the audit line (e.g. "rejected by alice").
func rejectAfterApproval(ctx context.Context, runID, stepRunID, decision string) error {
	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback() //nolint:errcheck
	sr := tx.Exec(
		`UPDATE workflow_step_runs SET status='failed', response_body=?, ended_at=CURRENT_TIMESTAMP WHERE step_run_id=? AND status=?`,
		decision, stepRunID, StatusAwaitingApproval)
	if sr.Error != nil {
		return sr.Error
	}
	if sr.RowsAffected == 0 {
		return errRunNotAwaiting
	}
	// One rejection fails the whole run, so any sibling gate parked on another
	// branch is cancelled rather than left stranded in awaiting_approval under a
	// failed run.
	if err := tx.Exec(
		`UPDATE workflow_step_runs SET status='cancelled', ended_at=CURRENT_TIMESTAMP WHERE run_id=? AND status=?`,
		runID, StatusAwaitingApproval).Error; err != nil {
		return err
	}
	r := tx.Exec(
		`UPDATE workflow_runs SET status='failed', ended_at=CURRENT_TIMESTAMP, token=NULL, run_session_id=NULL WHERE run_id=? AND status='awaiting_approval'`,
		runID)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected == 0 {
		return errRunNotAwaiting
	}
	return tx.Commit().Error
}

// approvalStepRun returns the step run a paused run is currently awaiting approval
// on. gorm.ErrRecordNotFound means the run has no pending gate.
//
// A graph run can park on several gates at once, so prefer approvalStepRuns and
// let the caller disambiguate; this returns the lowest-indexed gate and exists for
// callers that only need "is there a gate, and which step is it".
func approvalStepRun(ctx context.Context, runID string) (WorkflowStepRun, error) {
	var sr WorkflowStepRun
	err := connectRead().WithContext(ctx).
		Where("run_id=? AND status=?", runID, StatusAwaitingApproval).
		Order("step_index").First(&sr).Error
	return sr, err
}

// approvalStepRuns returns every gate a paused run is currently awaiting, in step
// order. A run parked on two concurrent branches has two; a linear pipeline can
// only ever have one, since a gate cannot share a parallel group.
func approvalStepRuns(ctx context.Context, runID string) ([]WorkflowStepRun, error) {
	var srs []WorkflowStepRun
	err := connectRead().WithContext(ctx).
		Where("run_id=? AND status=?", runID, StatusAwaitingApproval).
		Order("step_index").Find(&srs).Error
	return srs, err
}

// Complete marks the run with its final status, records its resolved output map
// (nil for a non-completed run), and clears credentials. Uses context.Background()
// internally: the caller's context may be cancelled on shutdown or user cancel, but
// the terminal state must always be persisted. outputs is stored as the JSON string
// the serializer:json column round-trips (a text/bytea column across postgres and
// the sqlite used in unit tests — no dialect-specific cast).
func (run WorkflowRun) Complete(_ context.Context, status string, outputs map[string]string) {
	_, span := otel.Tracer("workflows").Start(context.Background(), "db.workflow_run.complete")
	defer span.End()
	span.SetAttributes(
		attribute.String("run.id", run.RunID),
		attribute.String("status", status),
	)
	var outParam any // NULL unless the run completed with declared outputs
	if len(outputs) > 0 {
		if b, err := json.Marshal(outputs); err == nil {
			outParam = string(b)
		}
	}
	if err := connect().WithContext(context.Background()).Exec(
		"UPDATE workflow_runs SET status=?, outputs=?, ended_at=CURRENT_TIMESTAMP, token=NULL, run_session_id=NULL WHERE run_id=? AND status='running'",
		status, outParam, run.RunID,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetStatus(codes.Ok, "")
}

// UpdateToken stores a newly-rotated token for an active run.
func (run WorkflowRun) UpdateToken(ctx context.Context, token, sessionID string) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.workflow_run.update_token")
	defer span.End()
	span.SetAttributes(attribute.String("run.id", run.RunID))
	if err := connect().WithContext(ctx).Exec(
		"UPDATE workflow_runs SET token=?, run_session_id=? WHERE run_id=?",
		token, sessionID, run.RunID,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// ── WorkflowStepRun ───────────────────────────────────────────────────────────

// Add inserts a new step run in 'running' state. Uses context.Background()
// internally since the step record must land even if the caller's context
// is near-cancelled at high parallelism.
func (sr WorkflowStepRun) Add(_ context.Context) error {
	_, span := otel.Tracer("workflows").Start(context.Background(), "db.step_run.add")
	defer span.End()
	span.SetAttributes(
		attribute.String("step_run.id", sr.StepRunID),
		attribute.String("run.id", sr.RunID),
	)
	if err := connect().Exec(
		`INSERT INTO workflow_step_runs (step_run_id, run_id, step_index, step_name, status, started_at)
		 VALUES (?, ?, ?, ?, 'running', CURRENT_TIMESTAMP)`,
		sr.StepRunID, sr.RunID, sr.StepIndex, sr.StepName,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// AddAwaitingApproval inserts an approval step run paused for a human decision.
// message (the substituted approval prompt, if any) is stored as the step's
// response body so the run view can show what is being approved.
func (sr WorkflowStepRun) AddAwaitingApproval(message string) error {
	_, span := otel.Tracer("workflows").Start(context.Background(), "db.step_run.add_awaiting_approval")
	defer span.End()
	span.SetAttributes(
		attribute.String("step_run.id", sr.StepRunID),
		attribute.String("run.id", sr.RunID),
	)
	var body any // NULL when no message, so the column stays empty rather than ""
	if message != "" {
		body = message
	}
	if err := connect().Exec(
		`INSERT INTO workflow_step_runs (step_run_id, run_id, step_index, step_name, status, response_body, started_at)
		 VALUES (?, ?, ?, ?, 'awaiting_approval', ?, CURRENT_TIMESTAMP)`,
		sr.StepRunID, sr.RunID, sr.StepIndex, sr.StepName, body,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (sr WorkflowStepRun) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step_run.update")
	defer span.End()
	span.SetAttributes(attribute.String("step_run.id", sr.StepRunID))
	if err := connect().WithContext(ctx).Save(&sr).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (sr WorkflowStepRun) Remove(_ context.Context) error {
	return errors.New("WorkflowStepRun.Remove not implemented")
}

func (sr WorkflowStepRun) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step_run.get")
	defer span.End()
	span.SetAttributes(attribute.String("step_run.id", sr.StepRunID))
	var out WorkflowStepRun
	if err := connectRead().WithContext(ctx).Where("step_run_id=?", sr.StepRunID).First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (sr WorkflowStepRun) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("workflows").Start(ctx, "db.step_run.list")
	defer span.End()
	span.SetAttributes(attribute.String("run.id", sr.RunID))
	var stepRuns []WorkflowStepRun
	q := connectRead().WithContext(ctx).Where("run_id=?", sr.RunID).Order("step_index")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&stepRuns).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(stepRuns))
	for i, s := range stepRuns {
		rows[i] = s
	}
	return rows, nil
}

// Complete records the step run's outcome. Best-effort; run status is authoritative.
// Uses context.Background() internally as the run context may already be cancelled.
func (sr WorkflowStepRun) Complete(_ context.Context, status string, output, logs *string, usedMB, limitMB *int64) {
	_, span := otel.Tracer("workflows").Start(context.Background(), "db.step_run.complete")
	defer span.End()
	span.SetAttributes(
		attribute.String("step_run.id", sr.StepRunID),
		attribute.String("status", status),
	)
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — step result is best-effort; run status is authoritative
		`UPDATE workflow_step_runs SET status=?, response_body=?, logs=?, memory_used_mb=?, memory_limit_mb=?, ended_at=CURRENT_TIMESTAMP WHERE step_run_id=?`,
		status, output, logs, usedMB, limitMB, sr.StepRunID)
	span.SetStatus(codes.Ok, "")
}

// SetStatus updates a step run's live (non-terminal) display status — used to move it
// between 'running' and 'waiting_for_resources' as the backing job leaves and re-enters
// the target service's admission queue. Guarded to rows that are still live so a poll
// racing Complete() can never resurrect a finished step: ended_at IS NULL is the
// terminal marker Complete() always sets.
func (sr WorkflowStepRun) SetStatus(_ context.Context, status string) {
	// Restamp started_at when a queued step is finally admitted. A step run row is created
	// — and started_at stamped — for EVERY leg of a fan-out up front, long before most of
	// them have capacity to run. Left alone, a leg that waited 20 minutes in the admission
	// queue reports that wait as runtime, and every leg of a scatter reports an identical
	// duration spanning the whole group (15 legs all claiming ~1352s when only 5 ever had
	// a container). started_at should mark when the work began, not when it was enqueued.
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — display-only; the terminal status is authoritative
		`UPDATE workflow_step_runs
		    SET status = ?,
		        started_at = CASE WHEN ? = ? THEN CURRENT_TIMESTAMP ELSE started_at END
		  WHERE step_run_id = ? AND ended_at IS NULL`,
		status, status, StatusRunning, sr.StepRunID)
}

// SetLogs streams the backing action's stdout onto the step run WHILE IT RUNS, so
// the run view shows an agent's reasoning and tool calls live rather than only when
// the stage finishes (an agent step used to show nothing for the minutes it ran).
// Display-only and best-effort: the terminal Complete() writes the authoritative
// final logs, and this is guarded on ended_at IS NULL so a late poll cannot
// overwrite a finished row.
func (sr WorkflowStepRun) SetLogs(_ context.Context, logs string) {
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — display-only; Complete writes the authoritative logs
		`UPDATE workflow_step_runs SET logs = ? WHERE step_run_id = ? AND ended_at IS NULL`,
		logs, sr.StepRunID)
}

// stepJob is one async job a step run is still waiting on: the catalog action that
// submitted it (which resolves to the service and the cancel endpoint) and the id the
// service returned.
type stepJob struct {
	StepRunID string `gorm:"column:step_run_id"`
	Action    string `gorm:"column:job_action"`
	JobID     string `gorm:"column:job_id"`
}

// SetJob records the async job this step run submitted, so a later abandonment can
// cancel it — including from another process, which is the whole point of persisting
// it (see jobcancel.go). Guarded on ended_at IS NULL like SetStatus: a submission
// racing a step that has already been completed must not attach a job to a finished
// row, where nothing would ever clear it.
//
// Best-effort: failing to record the id costs the fast cancellation path, not the
// step, and the target service's own reaper remains the backstop.
func (sr WorkflowStepRun) SetJob(_ context.Context, action, jobID string) {
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — bookkeeping for cancellation; the step itself is unaffected
		`UPDATE workflow_step_runs SET job_action=?, job_id=? WHERE step_run_id=? AND ended_at IS NULL`,
		action, jobID, sr.StepRunID)
}

// ClearJob forgets the recorded job, called the moment the step stops owning one —
// either because it reached a terminal state (so cancelling it afterwards would be
// cancelling work that legitimately completed) or because it has just been cancelled.
func (sr WorkflowStepRun) ClearJob(_ context.Context) {
	connect().WithContext(context.Background()).Exec( //nolint:errcheck — see SetJob
		`UPDATE workflow_step_runs SET job_action='', job_id='' WHERE step_run_id=?`,
		sr.StepRunID)
}

// liveStepJobs returns the async jobs a run still has outstanding: recorded against a
// step run that has not finished. ended_at is the terminal marker Complete() always
// stamps, so this cannot return a job whose step already reported an outcome.
func liveStepJobs(ctx context.Context, runID string) ([]stepJob, error) {
	var jobs []stepJob
	err := connectRead().WithContext(ctx).Raw(
		`SELECT step_run_id, job_action, job_id FROM workflow_step_runs
		  WHERE run_id=? AND job_id IS NOT NULL AND job_id<>'' AND ended_at IS NULL`,
		runID).Scan(&jobs).Error
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

// runLeaseHeartbeat is how often the worker executing a run refreshes its lease;
// runLeaseStaleAfter is how long a lease may go unrefreshed before another pod may
// treat the run as orphaned. The gap between them is deliberately an order of
// magnitude: a worker blocked on a slow step, a stop-the-world GC pause, or a brief
// database blip must never be mistaken for a dead one, and the cost of waiting is only
// that a genuinely crashed run is reclaimed a few minutes later.
const (
	runLeaseHeartbeat  = 30 * time.Second
	runLeaseStaleAfter = 5 * time.Minute
)

// leaseCutoffExpr renders the SQL instant a lease must predate to count as stale.
//
// It is evaluated by the DATABASE's clock, not this pod's: a lease is written by
// whichever pod owns the run and read by every other, so judging it against a local
// clock would make the answer depend on inter-pod skew. Interval arithmetic is not
// portable, so the dialect is switched here exactly as skipLocked does (the unit
// suite runs on sqlite, where CURRENT_TIMESTAMP and datetime() share one
// 'YYYY-MM-DD HH:MM:SS' UTC text format and the comparison is a text compare).
func leaseCutoffExpr(tx *gorm.DB, staleAfter time.Duration) string {
	secs := int64(staleAfter.Seconds())
	if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
		return fmt.Sprintf("CURRENT_TIMESTAMP - (%d * interval '1 second')", secs)
	}
	return fmt.Sprintf("datetime('now', '-%d seconds')", secs)
}

// Heartbeat refreshes this run's lease so peer pods do not mistake it for orphaned.
// Guarded on status='running' so a beat racing a terminal transition can never revive
// a finished run's lease. Best-effort: one missed beat is harmless as long as the next
// lands well inside runLeaseStaleAfter, and a lease that stops moving is exactly the
// signal recovery is looking for.
func (run WorkflowRun) Heartbeat(ctx context.Context) {
	connect().WithContext(ctx).Exec( //nolint:errcheck — best-effort; the next beat covers a miss
		"UPDATE workflow_runs SET heartbeat_at=CURRENT_TIMESTAMP WHERE run_id=? AND status='running'", run.RunID)
}

// recoverStuckRunsDB fails runs left 'running' by a worker that died — and ONLY those.
//
// It cannot sweep every 'running' run. The service scales out (the prod chart enables
// the HPA at minReplicas 2) and a rolling deploy surges a new pod while the old one is
// still executing, so an unscoped sweep at startup failed runs that a PEER was actively
// running: that worker keeps going — its in-memory token is still valid, so the side
// effects still land — but its Complete() is guarded on status='running' and silently
// no-ops, permanently recording a successful run as failed.
//
// The lease is what makes ownership decidable without knowing anything about peers: a
// live worker refreshes heartbeat_at every runLeaseHeartbeat (keepRunLease), so a run
// whose lease has not moved for runLeaseStaleAfter has no worker behind it. This is the
// same shape failTimedOutRunsDB already uses for orphans — act only past a grace period,
// so the live path always wins the race and this only catches genuine casualties.
//
// The COALESCE chain covers rows written before the column existed (lease NULL): they
// age out on started_at instead, and the epoch literal is the last resort so a row
// missing both timestamps is still recoverable rather than stuck 'running' forever.
// abandonedRun is a run a sweep reclaimed, carrying exactly what is needed to release
// the remote work it left behind: the id, to look up its outstanding jobs, and its
// still-encrypted token, to authorise cancelling them.
//
// Both are captured BEFORE the sweep runs, because the sweep nulls the token — after it
// there is no credential left to cancel with, and the jobs would be forge's reaper's
// problem for as long as the step's timeout allowed.
type abandonedRun struct {
	RunID string `gorm:"column:run_id"`
	Token string `gorm:"column:token"`
}

// recoverStuckRunsWithJobs is recoverStuckRunsDB plus the rows it reclaimed, so the
// caller can cancel the async jobs those runs abandoned.
//
// The SELECT and the UPDATE are separate statements rather than one RETURNING, because
// this runs on both Postgres and the sqlite used by tests. The window between them is
// harmless: a run that finishes in it is simply not swept, and liveStepJobs re-checks
// ended_at, so the worst case is cancelling nothing. Cancellation itself tolerates a
// job that is already gone (404/409 are treated as success).
func recoverStuckRunsWithJobs() ([]abandonedRun, int64) {
	conn := connect()
	stale := `status='running' AND COALESCE(heartbeat_at, started_at, '1970-01-01') < ` +
		leaseCutoffExpr(conn, runLeaseStaleAfter)

	var runs []abandonedRun
	if err := conn.Raw(`SELECT run_id, token FROM workflow_runs WHERE ` + stale).Scan(&runs).Error; err != nil {
		// Not fatal: the sweep's job is to unwedge the queue, and it can still do that
		// without the job-cancellation half.
		slog.Warn("sweep: could not read runs pending recovery for job cancellation", "error", err)
	}
	result := conn.Exec(`
		UPDATE workflow_runs
		SET status='failed', ended_at=CURRENT_TIMESTAMP, token=NULL, run_session_id=NULL
		WHERE ` + stale)
	if result.Error != nil {
		slog.Error("startup: failed to recover stuck runs", "error", result.Error)
		return nil, 0
	}
	return runs, result.RowsAffected
}

func recoverStuckRunsDB() int64 {
	_, n := recoverStuckRunsWithJobs()
	return n
}

// failTimedOutRunsDB fails any run still 'running' past its workflow's timeout. It is
// the backstop the in-process run-timeout context cannot be: a worker that died
// mid-run leaves no goroutine to trip that context, so the run would sit 'running'
// until the next pod restart (which is how a run reached 18h). Runs are only swept a
// grace period BEYOND the timeout, so a live worker's own context — which fires at the
// timeout — remains the primary path and this only catches genuine orphans. The
// per-workflow timeout is joined in; 0 / NULL reads as the 1800s default.
func failTimedOutRunsDB() int64 {
	result := connect().Exec(`
		UPDATE workflow_runs r
		SET status='failed', ended_at=CURRENT_TIMESTAMP, token=NULL, run_session_id=NULL
		FROM workflows w
		WHERE r.workflow_id = w.workflow_id
		  AND r.status = 'running'
		  AND r.started_at IS NOT NULL
		  AND r.started_at < CURRENT_TIMESTAMP - ((COALESCE(NULLIF(w.run_timeout_secs, 0), 1800) + 120) * interval '1 second')
	`)
	if result.Error != nil {
		slog.Error("run-timeout sweep failed", "error", result.Error)
		return 0
	}
	return result.RowsAffected
}
