package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
		slog.Error("forge: connect to database", "error", err)
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
	// command, env, and timeout_secs are selected so list consumers (e.g. the CLI
	// rerun action) can resubmit an execution without a second fetch; stdout/stderr
	// stay out of the list because they can be large.
	q := connect().WithContext(ctx).
		Select("execution_id, user_id, image, command, env, timeout_secs, status, exit_code, memory_used_mb, memory_limit_mb, created_at, started_at, ended_at, runner_class, project").
		Where("user_id = ?", e.UserID).
		Order("created_at DESC").
		Limit(limit)
	// Project is an optional view filter, not a security boundary.
	if e.Project != "" {
		q = q.Where("project = ?", e.Project)
	}
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
	outputs := result.Outputs
	if outputs == nil {
		outputs = map[string]string{}
	}
	outputsJSON, _ := json.Marshal(outputs)
	if err := connect().WithContext(context.Background()).Exec(`
		UPDATE executions
		SET status = ?, exit_code = ?, stdout = ?, stderr = ?, outputs = ?,
		    memory_used_mb = ?, memory_limit_mb = ?, ended_at = now()
		WHERE execution_id = ?`,
		status, result.ExitCode, result.Stdout, result.Stderr, string(outputsJSON),
		result.MemoryUsedMB, result.MemoryLimitMB, e.ExecutionID,
	).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// claimSchedulerLockKey serialises the pending-execution scheduling decision
// across all worker goroutines and forge replicas via a transaction-scoped
// Postgres advisory lock. Only one claim transaction runs its count-and-pick step
// at a time, so the per-org / per-user running counts a claim reads are never
// stale relative to a sibling claim that is mid-flight (uncommitted status
// updates are invisible under READ COMMITTED, which would otherwise let two
// workers both see "under cap" and both claim, blowing past the limit). The lock
// is held only for the fast pick+mark step (the actual run happens after commit),
// and it is released automatically when the transaction ends. The value is the
// ASCII of "forge".
const claimSchedulerLockKey int64 = 0x666f726765

// claimPendingExecution atomically dequeues one pending execution and marks it
// as running. It respects per-org and per-user concurrency limits: it only claims
// a pending execution whose org and user are both below their effective running
// caps (a concurrency_limits override, else the FORGE_MAX_CONCURRENT_PER_ORG /
// _PER_USER env default; <= 0 means unlimited, and org_id=” skips the org cap).
// An execution over a cap stays pending and is retried on a later poll once a
// running peer of the same scope finishes. Returns the claimed execution and true
// on success; false when no eligible pending work is available.
func claimPendingExecution(ctx context.Context) (Execution, bool) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.claim_pending_execution")
	defer span.End()

	tx := connect().WithContext(ctx).Begin()
	if tx.Error != nil {
		span.RecordError(tx.Error)
		span.SetStatus(codes.Error, tx.Error.Error())
		return Execution{}, false
	}

	// Serialise the scheduling decision so concurrent claims see accurate running
	// counts (see claimSchedulerLockKey). Auto-released at commit/rollback.
	if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, claimSchedulerLockKey).Error; err != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "worker: acquire scheduler lock", "error", err)
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
		Backend     string `gorm:"column:backend"`
		OrgID       string `gorm:"column:org_id"`
		SecretRefs  []byte `gorm:"column:secret_refs"`
		OutputEnv   []byte `gorm:"column:output_env"`
		Checkout    []byte `gorm:"column:checkout"`
		Volumes     []byte `gorm:"column:volumes"`
	}
	var raw pendingRow

	// Pick the oldest pending execution whose org and user are both under their
	// effective running caps. Running counts come from CTEs; the effective cap is
	// the concurrency_limits override for the scope (LEFT JOIN) or the env default
	// (@defOrg / @defUser) when no row exists; <= 0 means unlimited. org_id='' skips
	// the org cap so org-less users are gated on the per-user cap only rather than
	// being lumped together under the empty org key. FOR UPDATE OF e SKIP LOCKED
	// locks only the chosen executions row, so a sibling claim (holding the advisory
	// lock) never runs concurrently, and a row being cancelled is skipped, not waited
	// on. With both defaults 0 and no overrides this reduces to the original FIFO
	// dequeue.
	result := tx.Raw(`
		WITH org_running AS (
			SELECT org_id, count(*) AS c FROM executions WHERE status = 'running' AND org_id <> '' GROUP BY org_id
		),
		user_running AS (
			SELECT user_id, count(*) AS c FROM executions WHERE status = 'running' GROUP BY user_id
		)
		SELECT e.execution_id, e.user_id, e.image, e.command, e.env, e.timeout_secs, e.runner_class, e.backend, e.org_id, e.secret_refs, e.output_env, e.checkout, e.volumes
		FROM executions e
		LEFT JOIN org_running  orr ON orr.org_id  = e.org_id
		LEFT JOIN user_running urr ON urr.user_id = e.user_id
		LEFT JOIN concurrency_limits ol ON ol.scope = 'org'  AND ol.scope_id = e.org_id
		LEFT JOIN concurrency_limits ul ON ul.scope = 'user' AND ul.scope_id = e.user_id
		WHERE e.status = 'pending'
		  AND (e.org_id = '' OR COALESCE(ol.max_concurrent, @defOrg) <= 0 OR COALESCE(orr.c, 0) < COALESCE(ol.max_concurrent, @defOrg))
		  AND (COALESCE(ul.max_concurrent, @defUser) <= 0 OR COALESCE(urr.c, 0) < COALESCE(ul.max_concurrent, @defUser))
		ORDER BY e.created_at
		LIMIT 1
		FOR UPDATE OF e SKIP LOCKED
	`, sql.Named("defOrg", defaultMaxConcurrentPerOrg), sql.Named("defUser", defaultMaxConcurrentPerUser)).Scan(&raw)
	if result.Error != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		slog.ErrorContext(ctx, "worker: query pending row", "error", result.Error)
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
	exec.Backend = raw.Backend
	exec.OrgID = raw.OrgID

	if len(raw.SecretRefs) > 0 {
		if err := json.Unmarshal(raw.SecretRefs, &exec.SecretRefs); err != nil {
			tx.Rollback() //nolint:errcheck
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "worker: unmarshal secret_refs", "execution_id", exec.ExecutionID, "error", err)
			return Execution{}, false
		}
	}

	if err := json.Unmarshal(raw.Command, &exec.Command); err != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "worker: unmarshal command", "execution_id", exec.ExecutionID, "error", err)
		return Execution{}, false
	}
	if err := json.Unmarshal(raw.Env, &exec.Env); err != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "worker: unmarshal env", "execution_id", exec.ExecutionID, "error", err)
		return Execution{}, false
	}
	// output_env drives structured-output capture (wrapOutputEnv); it must be
	// loaded here or the worker never wraps the command. A '[]'/NULL column
	// leaves OutputEnv nil, which is the no-capture case.
	if len(raw.OutputEnv) > 0 {
		if err := json.Unmarshal(raw.OutputEnv, &exec.OutputEnv); err != nil {
			tx.Rollback() //nolint:errcheck
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "worker: unmarshal output_env", "execution_id", exec.ExecutionID, "error", err)
			return Execution{}, false
		}
	}
	// checkout drives the actions/checkout-style clone prologue; a NULL column
	// leaves Checkout nil (no auto-checkout).
	if len(raw.Checkout) > 0 {
		if err := json.Unmarshal(raw.Checkout, &exec.Checkout); err != nil {
			tx.Rollback() //nolint:errcheck
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "worker: unmarshal checkout", "execution_id", exec.ExecutionID, "error", err)
			return Execution{}, false
		}
	}
	// volumes drives shared-workspace attachment in the runtime; a '[]'/NULL column
	// leaves Volumes nil (no shared storage). Like output_env, it must be loaded here
	// or the worker mounts nothing and the run can't see the checkout.
	if len(raw.Volumes) > 0 {
		if err := json.Unmarshal(raw.Volumes, &exec.Volumes); err != nil {
			tx.Rollback() //nolint:errcheck
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "worker: unmarshal volumes", "execution_id", exec.ExecutionID, "error", err)
			return Execution{}, false
		}
	}

	if err := tx.Exec(`UPDATE executions SET status = 'running', started_at = now() WHERE execution_id = ?`, exec.ExecutionID).Error; err != nil {
		tx.Rollback() //nolint:errcheck
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "worker: mark running", "execution_id", exec.ExecutionID, "error", err)
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
		"disk_gb":        rc.DiskGB,
		"backend":        rc.Backend,
		"enabled":        rc.Enabled,
		"privileged":     rc.Privileged,
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

// ── RuntimeBackend ────────────────────────────────────────────────────────────

func (b RuntimeBackend) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runtimebackend.add")
	defer span.End()
	span.SetAttributes(attribute.String("runtime_backend.name", b.Name))
	if err := connect().WithContext(ctx).Create(&b).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (b RuntimeBackend) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runtimebackend.update")
	defer span.End()
	span.SetAttributes(attribute.String("runtime_backend.name", b.Name))
	result := connect().WithContext(ctx).Model(&RuntimeBackend{}).Where("name = ?", b.Name).Updates(map[string]any{
		"type":        b.Type,
		"enabled":     b.Enabled,
		"config":      b.Config,
		"secret_refs": b.SecretRefs,
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

func (b RuntimeBackend) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runtimebackend.remove")
	defer span.End()
	span.SetAttributes(attribute.String("runtime_backend.name", b.Name))
	result := connect().WithContext(ctx).Where("name = ?", b.Name).Delete(&RuntimeBackend{})
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

func (b RuntimeBackend) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runtimebackend.get")
	defer span.End()
	span.SetAttributes(attribute.String("runtime_backend.name", b.Name))
	var out RuntimeBackend
	if err := connect().WithContext(ctx).Where("name = ?", b.Name).First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (b RuntimeBackend) List(ctx context.Context, _ int, _ int) ([]db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.runtimebackend.list")
	defer span.End()
	var backends []RuntimeBackend
	if err := connect().WithContext(ctx).Order("name").Find(&backends).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(backends))
	for i, be := range backends {
		rows[i] = be
	}
	return rows, nil
}

// ── ConcurrencyLimit ──────────────────────────────────────────────────────────

func (c ConcurrencyLimit) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.concurrencylimit.add")
	defer span.End()
	span.SetAttributes(attribute.String("concurrency_limit.scope", c.Scope), attribute.String("concurrency_limit.scope_id", c.ScopeID))
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Update changes an existing limit row's cap. Returns gorm.ErrRecordNotFound when
// no row matches (scope, scope_id). The admin PUT handler uses Save (upsert)
// instead; this exists to satisfy the db interface.
func (c ConcurrencyLimit) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.concurrencylimit.update")
	defer span.End()
	span.SetAttributes(attribute.String("concurrency_limit.scope", c.Scope), attribute.String("concurrency_limit.scope_id", c.ScopeID))
	result := connect().WithContext(ctx).Model(&ConcurrencyLimit{}).
		Where("scope = ? AND scope_id = ?", c.Scope, c.ScopeID).
		Updates(map[string]any{"max_concurrent": c.MaxConcurrent, "updated_at": gorm.Expr("now()")})
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

// Save upserts the limit by (scope, scope_id): it creates the row or overwrites
// its cap, refreshing updated_at. This is the admin "set limit" write path — a
// PUT should not care whether an override already existed.
func (c ConcurrencyLimit) Save(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.concurrencylimit.save")
	defer span.End()
	span.SetAttributes(attribute.String("concurrency_limit.scope", c.Scope), attribute.String("concurrency_limit.scope_id", c.ScopeID))
	if err := connect().WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope"}, {Name: "scope_id"}},
		DoUpdates: clause.Assignments(map[string]any{"max_concurrent": c.MaxConcurrent, "updated_at": gorm.Expr("now()")}),
	}).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c ConcurrencyLimit) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.concurrencylimit.remove")
	defer span.End()
	span.SetAttributes(attribute.String("concurrency_limit.scope", c.Scope), attribute.String("concurrency_limit.scope_id", c.ScopeID))
	result := connect().WithContext(ctx).Where("scope = ? AND scope_id = ?", c.Scope, c.ScopeID).Delete(&ConcurrencyLimit{})
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

func (c ConcurrencyLimit) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.concurrencylimit.get")
	defer span.End()
	span.SetAttributes(attribute.String("concurrency_limit.scope", c.Scope), attribute.String("concurrency_limit.scope_id", c.ScopeID))
	var out ConcurrencyLimit
	if err := connect().WithContext(ctx).Where("scope = ? AND scope_id = ?", c.Scope, c.ScopeID).First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (c ConcurrencyLimit) List(ctx context.Context, _ int, _ int) ([]db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.concurrencylimit.list")
	defer span.End()
	var limits []ConcurrencyLimit
	if err := connect().WithContext(ctx).Order("scope, scope_id").Find(&limits).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(limits))
	for i, l := range limits {
		rows[i] = l
	}
	return rows, nil
}

// ── Volume ────────────────────────────────────────────────────────────────────

func (v Volume) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.volume.add")
	defer span.End()
	span.SetAttributes(attribute.String("volume.resource_name", v.ResourceName))
	if err := connect().WithContext(ctx).Create(&v).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (v Volume) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.volume.update")
	defer span.End()
	span.SetAttributes(attribute.String("volume.resource_name", v.ResourceName))
	if err := connect().WithContext(ctx).Save(&v).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (v Volume) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.volume.remove")
	defer span.End()
	span.SetAttributes(attribute.String("volume.resource_name", v.ResourceName))
	result := connect().WithContext(ctx).Where("resource_name = ?", v.ResourceName).Delete(&Volume{})
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

// Get finds a volume by ResourceName.
func (v Volume) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.volume.get")
	defer span.End()
	span.SetAttributes(attribute.String("volume.resource_name", v.ResourceName))
	var out Volume
	if err := connect().WithContext(ctx).Where("resource_name = ?", v.ResourceName).First(&out).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

// List returns the active volumes for the WorkflowID set on the receiver, ordered
// by creation. An empty WorkflowID lists all active volumes.
func (v Volume) List(ctx context.Context, _ int, _ int) ([]db, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.volume.list")
	defer span.End()
	q := connect().WithContext(ctx).Where("status = ?", volumeStatusActive)
	if v.WorkflowID != "" {
		q = q.Where("workflow_id = ?", v.WorkflowID)
	}
	var volumes []Volume
	if err := q.Order("created_at").Find(&volumes).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	rows := make([]db, len(volumes))
	for i, vol := range volumes {
		rows[i] = vol
	}
	return rows, nil
}

// sumActiveWorkflowVolumeMB totals the size of a workflow's active volumes — the
// figure the per-workflow size cap is checked against before a new create.
func sumActiveWorkflowVolumeMB(ctx context.Context, workflowID string) (int64, error) {
	ctx, span := otel.Tracer("forge").Start(ctx, "db.volume.sum_workflow")
	defer span.End()
	var total *int64
	if err := connect().WithContext(ctx).
		Model(&Volume{}).
		Where("workflow_id = ? AND status = ?", workflowID, volumeStatusActive).
		Select("COALESCE(SUM(size_mb), 0)").
		Scan(&total).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return 0, err
	}
	if total == nil {
		return 0, nil
	}
	return *total, nil
}

// getActiveVolume resolves a volume by its (workflowID, name) handle for attach
// validation. Only active volumes are returned.
func getActiveVolume(ctx context.Context, workflowID, name string) (Volume, error) {
	var out Volume
	err := connect().WithContext(ctx).
		Where("workflow_id = ? AND name = ? AND status = ?", workflowID, name, volumeStatusActive).
		First(&out).Error
	return out, err
}

// markVolumeDeleted flips a volume's row to deleted after its backend resource is
// gone, so it stops counting against the cap and a repeat delete is a no-op.
func markVolumeDeleted(ctx context.Context, resourceName string) error {
	return connect().WithContext(ctx).Exec(
		`UPDATE volumes SET status = ? WHERE resource_name = ?`, volumeStatusDeleted, resourceName,
	).Error
}

// listReapableVolumes returns active volumes created before cutoff — orphans whose
// workflow ended without tearing them down. The reaper deletes their backend
// resource then marks the row deleted.
func listReapableVolumes(ctx context.Context, cutoff time.Time) ([]Volume, error) {
	var volumes []Volume
	err := connect().WithContext(ctx).
		Where("status = ? AND created_at < ?", volumeStatusActive, cutoff).
		Find(&volumes).Error
	return volumes, err
}
