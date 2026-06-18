package main

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

const (
	// workerInterval is how often the background worker drains the retry queue
	// even without an explicit nudge.
	workerInterval = 15 * time.Second
	// workerBatch caps how many notifications one drain pass claims.
	workerBatch = 50
	// sendTimeout bounds a single provider delivery attempt.
	sendTimeout = 15 * time.Second
)

// workerWake lets the API nudge the worker to run a drain pass immediately
// (e.g. right after POST /notify) instead of waiting for the next tick. Buffered
// to size 1 so a nudge is never lost and never blocks the caller.
var workerWake = make(chan struct{}, 1)

// nudgeWorker requests an immediate drain pass without blocking.
func nudgeWorker() {
	select {
	case workerWake <- struct{}{}:
	default:
	}
}

// deliverNow delivers a single message through the channel's provider plugin,
// bounded by sendTimeout. It is the one place a Notifier.Send is invoked.
func deliverNow(ctx context.Context, ch Channel, msg Message) error {
	n, ok := getNotifier(ch.Type)
	if !ok {
		return errUnknownChannelType
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return n.Send(ctx, ch.Config, msg)
}

// runWorker drains the retry queue on a ticker and whenever nudged, until ctx is
// cancelled. The first pass runs immediately so notifications left pending by a
// previous crash are recovered on startup.
func runWorker(ctx context.Context) {
	ticker := time.NewTicker(workerInterval)
	defer ticker.Stop()
	drain(ctx) // startup recovery
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drain(ctx)
		case <-workerWake:
			drain(ctx)
		}
	}
}

// drain claims a batch of due notifications and attempts to deliver each.
func drain(ctx context.Context) {
	ctx, span := otel.Tracer("notifications").Start(ctx, "worker.drain")
	defer span.End()

	batch, err := claimRetryable(ctx, workerBatch)
	if err != nil {
		span.RecordError(err)
		slog.ErrorContext(ctx, "worker: claim failed", "error", err)
		return
	}
	if len(batch) == 0 {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.Int("batch.size", len(batch)))
	for i := range batch {
		processNotification(ctx, batch[i])
	}
	span.SetStatus(codes.Ok, "")
}

// processNotification delivers one already-claimed notification and records the
// outcome. attempts has already been incremented by claimRetryable.
func processNotification(ctx context.Context, n Notification) {
	ch, err := getChannel(ctx, n.ChannelID)
	if err != nil {
		if isNotFound(err) {
			// Channel genuinely gone — there's no point retrying. Burn the attempts.
			markFailed(ctx, n, "channel not found", true)
			return
		}
		// Transient lookup failure (DB blip / read-replica failover): no real delivery
		// was attempted, so reschedule WITHOUT consuming the retry budget. Otherwise a
		// blip on the final attempt — claimRetryable having just incremented attempts
		// to maxAttempts — would permanently drop a deliverable notification.
		slog.WarnContext(ctx, "worker: channel lookup failed, will retry",
			"notification_id", n.NotificationID, "channel_id", n.ChannelID, "error", err)
		markRetryable(ctx, n, "channel lookup failed")
		return
	}
	if !ch.Enabled {
		markFailed(ctx, n, "channel is disabled", true)
		return
	}

	if err := deliverNow(ctx, ch, Message{Subject: n.Subject, Body: n.Body}); err != nil {
		exhausted := n.Attempts >= maxAttempts
		markFailed(ctx, n, err.Error(), exhausted)
		slog.WarnContext(ctx, "notification delivery failed",
			"notification_id", n.NotificationID, "channel_id", n.ChannelID,
			"attempts", n.Attempts, "exhausted", exhausted, "error", err)
		return
	}

	now := time.Now().UTC()
	n.Status = StatusSent
	n.LastError = ""
	n.SentAt = &now
	if err := n.Update(ctx); err != nil {
		slog.ErrorContext(ctx, "worker: mark sent failed", "notification_id", n.NotificationID, "error", err)
		return
	}
	meterSent.Add(ctx, 1, metric.WithAttributes(attribute.String("type", n.ChannelType)))
}

// markRetryable reschedules a notification after a failure that did NOT consume a
// real delivery attempt (e.g. a transient channel-lookup error). It rolls back the
// attempt claimRetryable incremented so transient faults can never exhaust the
// budget, and applies backoff so a persistent transient fault doesn't hot-loop.
func markRetryable(ctx context.Context, n Notification, reason string) {
	n.Status = StatusFailed
	n.LastError = reason
	// Back off based on the current (pre-rollback) attempt count so repeated
	// transient faults space out, then roll the attempt back to keep the budget intact.
	n.NextAttemptAt = time.Now().UTC().Add(retryDelay(n.Attempts))
	if n.Attempts > 0 {
		n.Attempts--
	}
	if err := n.Update(ctx); err != nil {
		slog.ErrorContext(ctx, "worker: mark retryable failed", "notification_id", n.NotificationID, "error", err)
	}
}

// markFailed records a delivery failure. When exhausted, attempts is pinned at
// maxAttempts so the worker stops retrying.
func markFailed(ctx context.Context, n Notification, reason string, exhausted bool) {
	n.Status = StatusFailed
	n.LastError = reason
	if exhausted {
		n.Attempts = maxAttempts
	} else {
		// Space the next retry out with exponential backoff so a failing endpoint
		// isn't re-hit on every worker tick.
		n.NextAttemptAt = time.Now().UTC().Add(retryDelay(n.Attempts))
	}
	if err := n.Update(ctx); err != nil {
		slog.ErrorContext(ctx, "worker: mark failed", "notification_id", n.NotificationID, "error", err)
		return
	}
	if exhausted {
		meterFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("type", n.ChannelType)))
	}
}
