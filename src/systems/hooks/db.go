package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
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
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/hooks")), &gorm.Config{
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
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/hooks")
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

// ── PipelineRule ──────────────────────────────────────────────────────────────

func (rule PipelineRule) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.pipeline_rule.add")
	defer span.End()
	span.SetAttributes(attribute.String("rule.id", rule.RuleID))
	if err := connect().WithContext(ctx).Create(&rule).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (rule PipelineRule) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.pipeline_rule.update")
	defer span.End()
	span.SetAttributes(attribute.String("rule.id", rule.RuleID))
	rule.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&rule).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (rule PipelineRule) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.pipeline_rule.remove")
	defer span.End()
	span.SetAttributes(attribute.String("rule.id", rule.RuleID))
	if err := connect().WithContext(ctx).Model(&PipelineRule{}).
		Where("rule_id=? AND active=?", rule.RuleID, true).
		Updates(map[string]any{"active": false, "updated_at": time.Now().UTC()}).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (rule PipelineRule) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.pipeline_rule.get")
	defer span.End()
	span.SetAttributes(attribute.String("rule.id", rule.RuleID))
	var result PipelineRule
	if err := connectRead().WithContext(ctx).Where("rule_id=? AND active=?", rule.RuleID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (rule PipelineRule) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.pipeline_rule.list")
	defer span.End()
	var rules []PipelineRule
	rule.Active = true
	q := connectRead().WithContext(ctx).Where(rule)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&rules).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(rules))
	for i, r := range rules {
		result[i] = r
	}
	return result, nil
}

// getRule fetches a single active pipeline rule by ID.
func getRule(ctx context.Context, id string) (PipelineRule, error) {
	row, err := (PipelineRule{RuleID: id}).Get(ctx)
	if err != nil {
		return PipelineRule{}, err
	}
	return row.(PipelineRule), nil
}

// listRules returns active pipeline rules accessible to the caller.
func listRules(ctx context.Context, userID, orgID string) ([]PipelineRule, error) {
	var rules []PipelineRule
	if err := connectRead().WithContext(ctx).
		Where("active=? AND (created_by=? OR (org_id!='' AND org_id=?))", true, userID, orgID).
		Order("created_at DESC").
		Limit(100).
		Find(&rules).Error; err != nil {
		return nil, err
	}
	if rules == nil {
		rules = []PipelineRule{}
	}
	return rules, nil
}

// getMatchedRules returns active pipeline rules matching a source and event type.
// Only rules with a non-empty secret are returned; rows with NULL or empty
// secrets are excluded so they can never fire without HMAC verification.
func getMatchedRules(ctx context.Context, source, event string) ([]PipelineRule, error) {
	var rules []PipelineRule
	if err := connectRead().WithContext(ctx).Raw(
		`SELECT rule_id, name, repo, events, ref_filter, workflow_id, secret, input_mapping, created_by, org_id
		 FROM pipeline_rules
		 WHERE repo = ? AND active = true
		   AND events::jsonb @> jsonb_build_array(?::text)
		   AND secret IS NOT NULL AND secret != ''`,
		source, event,
	).Scan(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}

// ── HookEvent ─────────────────────────────────────────────────────────────────

func (e HookEvent) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_event.add")
	defer span.End()
	span.SetAttributes(attribute.String("event.id", e.EventID))
	if err := connect().WithContext(ctx).Create(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (e HookEvent) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_event.update")
	defer span.End()
	span.SetAttributes(attribute.String("event.id", e.EventID))
	if err := connect().WithContext(ctx).Save(&e).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (e HookEvent) Remove(ctx context.Context) error { return nil }

func (e HookEvent) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_event.get")
	defer span.End()
	span.SetAttributes(attribute.String("event.id", e.EventID))
	var result HookEvent
	if err := connectRead().WithContext(ctx).Where("event_id=?", e.EventID).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (e HookEvent) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_event.list")
	defer span.End()
	var events []HookEvent
	q := connectRead().WithContext(ctx).Where(e)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&events).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(events))
	for i, ev := range events {
		result[i] = ev
	}
	return result, nil
}

// getEvent returns a single hook event by ID.
func getEvent(ctx context.Context, id string) (HookEvent, error) {
	row, err := (HookEvent{EventID: id}).Get(ctx)
	if err != nil {
		return HookEvent{}, err
	}
	return row.(HookEvent), nil
}

// listEvents returns hook events visible to the caller via their pipeline rules.
func listEvents(ctx context.Context, userID, orgID, source string) ([]HookEvent, error) {
	var events []HookEvent
	var err error
	if source != "" {
		err = connectRead().WithContext(ctx).Raw(
			`SELECT DISTINCT he.event_id, he.repo, he.event_type, he.ref, he.payload,
			        he.rules_matched, he.status, he.created_at
			 FROM hook_events he
			 JOIN hook_triggers ht ON ht.event_id = he.event_id
			 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
			 WHERE (pr.created_by = ? OR (pr.org_id != '' AND pr.org_id = ?))
			   AND he.repo = ?
			 ORDER BY he.created_at DESC LIMIT 100`,
			userID, orgID, source,
		).Scan(&events).Error
	} else {
		err = connectRead().WithContext(ctx).Raw(
			`SELECT DISTINCT he.event_id, he.repo, he.event_type, he.ref, he.payload,
			        he.rules_matched, he.status, he.created_at
			 FROM hook_events he
			 JOIN hook_triggers ht ON ht.event_id = he.event_id
			 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
			 WHERE pr.created_by = ? OR (pr.org_id != '' AND pr.org_id = ?)
			 ORDER BY he.created_at DESC LIMIT 100`,
			userID, orgID,
		).Scan(&events).Error
	}
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = []HookEvent{}
	}
	return events, nil
}

// getTriggersForEvent returns all hook triggers for an event ordered by creation time.
func getTriggersForEvent(ctx context.Context, eventID string) ([]HookTrigger, error) {
	var triggers []HookTrigger
	if err := connectRead().WithContext(ctx).
		Where("event_id=?", eventID).
		Order("created_at").
		Find(&triggers).Error; err != nil {
		return nil, err
	}
	if triggers == nil {
		triggers = []HookTrigger{}
	}
	return triggers, nil
}

// countEventAccess returns the number of pipeline rules triggered by an event
// that are accessible to the caller.
func countEventAccess(ctx context.Context, eventID, userID, orgID string) (int, error) {
	var count int
	if err := connectRead().WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM hook_triggers ht
		 JOIN pipeline_rules pr ON pr.rule_id = ht.rule_id
		 WHERE ht.event_id = ?
		   AND (pr.created_by = ? OR (pr.org_id != '' AND pr.org_id = ?))`,
		eventID, userID, orgID,
	).Scan(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// countRulesForRepo returns the number of active pipeline rules for a repo
// that are accessible to the caller (used as a fallback access check).
func countRulesForRepo(ctx context.Context, repo, userID, orgID string) (int, error) {
	var count int
	if err := connectRead().WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM pipeline_rules
		 WHERE repo = ? AND active = true
		   AND (created_by = ? OR (org_id != '' AND org_id = ?))`,
		repo, userID, orgID,
	).Scan(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// ── HookTrigger ───────────────────────────────────────────────────────────────

func (t HookTrigger) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_trigger.add")
	defer span.End()
	span.SetAttributes(attribute.String("trigger.id", t.TriggerID))
	if err := connect().WithContext(ctx).Create(&t).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (t HookTrigger) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_trigger.update")
	defer span.End()
	span.SetAttributes(attribute.String("trigger.id", t.TriggerID))
	if err := connect().WithContext(ctx).Save(&t).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (t HookTrigger) Remove(ctx context.Context) error { return nil }

func (t HookTrigger) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_trigger.get")
	defer span.End()
	span.SetAttributes(attribute.String("trigger.id", t.TriggerID))
	var result HookTrigger
	if err := connectRead().WithContext(ctx).Where("trigger_id=?", t.TriggerID).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (t HookTrigger) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("hooks").Start(ctx, "db.hook_trigger.list")
	defer span.End()
	var triggers []HookTrigger
	q := connectRead().WithContext(ctx).Where(t).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&triggers).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(triggers))
	for i, tr := range triggers {
		result[i] = tr
	}
	return result, nil
}

// updateEventStatus updates a hook event's rules_matched count and status.
func updateEventStatus(eventID string, rulesMatched int, status string) {
	if err := connect().Exec(
		`UPDATE hook_events SET rules_matched=?, status=? WHERE event_id=?`,
		rulesMatched, status, eventID,
	).Error; err != nil {
		slog.Error("updateEventStatus: db update failed", "event_id", eventID, "error", err)
	}
}

// addRetry inserts a dead-letter retry record for a failed dispatch.
func addRetry(ctx context.Context, triggerID, workflowID, triggeredBy, orgID string, inputs map[string]string, lastError string) error {
	retry := HookTriggerRetry{
		RetryID:     uuid.New().String(),
		TriggerID:   triggerID,
		WorkflowID:  workflowID,
		TriggeredBy: triggeredBy,
		OrgID:       orgID,
		Inputs:      inputs,
		Attempt:     1,
		LastError:   lastError,
		NextRetryAt: time.Now().UTC().Add(60 * time.Second),
		CreatedAt:   time.Now().UTC(),
	}
	return connect().WithContext(ctx).Create(&retry).Error
}

// claimDueRetries returns up to n retry records whose next_retry_at has passed.
func claimDueRetries(ctx context.Context, n int) ([]HookTriggerRetry, error) {
	var retries []HookTriggerRetry
	err := connect().WithContext(ctx).
		Where("next_retry_at <= now()").
		Order("next_retry_at").
		Limit(n).
		Find(&retries).Error
	return retries, err
}

// retryBackoff returns the delay before the nth attempt (1-indexed).
// Sequence: 60s, 120s, 240s, 480s, capped at 900s.
func retryBackoff(attempt int) time.Duration {
	return time.Duration(min(60*(1<<(attempt-1)), 900)) * time.Second
}

// markTriggerTriggered sets a trigger's status to "triggered" and records the run ID.
func markTriggerTriggered(ctx context.Context, triggerID string, runID *string) error {
	return connect().WithContext(ctx).Model(&HookTrigger{}).
		Where("trigger_id = ?", triggerID).
		Updates(map[string]any{"status": "triggered", "run_id": runID}).Error
}

// markTriggerFailed sets a trigger's status to "failed" with the given error message.
func markTriggerFailed(ctx context.Context, triggerID, errMsg string) error {
	return connect().WithContext(ctx).Model(&HookTrigger{}).
		Where("trigger_id = ?", triggerID).
		Updates(map[string]any{"status": "failed", "error": errMsg}).Error
}

// deleteRetry removes a retry record by ID.
func deleteRetry(ctx context.Context, retryID string) error {
	return connect().WithContext(ctx).Delete(&HookTriggerRetry{RetryID: retryID}).Error
}

// advanceRetry schedules a retry record for its next attempt.
func advanceRetry(ctx context.Context, retryID string, attempt int, lastError string, nextRetryAt time.Time) error {
	return connect().WithContext(ctx).Model(&HookTriggerRetry{}).
		Where("retry_id = ?", retryID).
		Updates(map[string]any{
			"attempt":       attempt,
			"last_error":    lastError,
			"next_retry_at": nextRetryAt,
		}).Error
}
