package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	maxDispatchAttempts = 6
	dispatchBaseBackoff = 15 * time.Second
)

// detach returns a context that keeps the request's values (trace, logger) but is not
// cancelled when the request returns — dispatch outlives the HTTP handler.
func detach(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// dispatchObserver is notified of what a matched trigger's actions produced. It exists for
// adapters that must report a pipeline run back to the system that sent the event — the
// GitHub App creating a check run on the pushed commit. It runs after the outcome is
// recorded, and must not block: implementations hand off to a goroutine.
type dispatchObserver func(ctx context.Context, t Trigger, e Event, outs []actionOutcome)

// evaluateAndDispatch matches every enabled trigger of the event's tenant and runs the
// actions of those whose filter passes. A (trigger, event) pair is claimed exactly once
// (unique index), so a redelivery of the same event never double-fires.
func evaluateAndDispatch(ctx context.Context, e Event) {
	evaluateAndDispatchObserved(ctx, e, nil)
}

// evaluateAndDispatchObserved is evaluateAndDispatch with a per-trigger completion callback.
func evaluateAndDispatchObserved(ctx context.Context, e Event, obs dispatchObserver) {
	triggers, err := triggersForTenant(ctx, e.Actor.OrgID, e.Actor.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "load triggers", "error", err)
		return
	}
	for _, t := range triggers {
		ok, err := EvalEvent(t.Match, e)
		if err != nil {
			slog.WarnContext(ctx, "filter eval failed", "trigger", t.ID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		countTriggerMatched(ctx, e.Type)
		if !claimDispatch(ctx, t, e) {
			continue // already handled (idempotent)
		}
		runAndRecord(ctx, t, e, obs)
	}
}

// claimDispatch inserts the pending dispatch row; a duplicate (unique trigger+event) means
// another delivery already claimed it, so we return false and skip.
func claimDispatch(ctx context.Context, t Trigger, e Event) bool {
	row := dispatchRow{ID: uuid.NewString(), TriggerID: t.ID, EventID: e.ID, OrgID: e.Actor.OrgID, Status: "pending"}
	err := connect().WithContext(ctx).Create(&row).Error
	if err != nil {
		return false // unique-violation = already claimed (or a transient error we let retry cover)
	}
	return true
}

// runAndRecord executes a trigger's actions and records the outcome, scheduling a retry on
// failure and dead-lettering after maxDispatchAttempts.
func runAndRecord(ctx context.Context, t Trigger, e Event, obs dispatchObserver) {
	outs, err := runActions(ctx, t, e)
	if obs != nil && len(outs) > 0 {
		obs(ctx, t, e, outs)
	}
	upd := map[string]any{"updated_at": time.Now().UTC()}
	if err == nil {
		upd["status"] = "done"
		upd["next_attempt_at"] = nil
		upd["last_error"] = ""
	} else {
		var attempts int
		connectRead().WithContext(ctx).Model(&dispatchRow{}).
			Select("attempts").Where("trigger_id = ? AND event_id = ?", t.ID, e.ID).Scan(&attempts)
		attempts++
		upd["attempts"] = attempts
		upd["last_error"] = err.Error()
		if attempts >= maxDispatchAttempts {
			upd["status"] = "dead"
			upd["next_attempt_at"] = nil
			slog.ErrorContext(ctx, "dispatch dead-lettered", "trigger", t.ID, "event", e.ID, "error", err)
		} else {
			next := time.Now().UTC().Add(dispatchBaseBackoff * time.Duration(1<<uint(attempts-1)))
			upd["status"] = "failed"
			upd["next_attempt_at"] = next
		}
	}
	connect().WithContext(ctx).Model(&dispatchRow{}).
		Where("trigger_id = ? AND event_id = ?", t.ID, e.ID).Updates(upd)
}

// startRetryLoop re-runs failed dispatches whose backoff has elapsed, giving at-least-once
// delivery across transient action failures (e.g. workflows briefly unavailable).
func startRetryLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				retryDue(ctx)
			}
		}
	}()
}

func retryDue(ctx context.Context) {
	var rows []dispatchRow
	err := connect().WithContext(ctx).
		Where("status = ? AND next_attempt_at IS NOT NULL AND next_attempt_at <= ?", "failed", time.Now().UTC()).
		Limit(50).Find(&rows).Error
	if err != nil {
		return
	}
	for _, d := range rows {
		t, err := getTriggerByID(ctx, d.TriggerID)
		if err != nil {
			continue
		}
		e, err := getEventByID(ctx, d.EventID)
		if err != nil {
			continue
		}
		// No observer on retries: the adapter that would have reported the run (a check run
		// on a commit) is long gone with the request that created it.
		runAndRecord(ctx, *t, *e, nil)
	}
}

// getTriggerByID / getEventByID load without tenant scoping — used by the retry loop, which
// already holds a claimed dispatch row for the pair.
func getTriggerByID(ctx context.Context, id string) (*Trigger, error) {
	var t Trigger
	if err := connectRead().WithContext(ctx).Where("id = ?", id).First(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

func getEventByID(ctx context.Context, id string) (*Event, error) {
	var r eventRow
	if err := connectRead().WithContext(ctx).Where("id = ?", id).First(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		return nil, err
	}
	e := r.toEvent()
	return &e, nil
}
