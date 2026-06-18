package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMarkFailed_BackoffWhenNotExhausted covers the retry-spacing fix: a
// non-exhausted failure keeps its attempt count and is scheduled for a future
// retry via next_attempt_at, instead of being re-claimed on the next tick.
func TestMarkFailed_BackoffWhenNotExhausted(t *testing.T) {
	withTestDB(t)
	ctx := context.Background()
	before := time.Now().UTC()
	n := addNotif(t, Notification{NotificationID: uuid.New().String(), ChannelID: "c", Body: "b", Status: StatusPending, Attempts: 2})

	markFailed(ctx, n, "boom", false)

	got, err := getNotification(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want unchanged (2) when not exhausted", got.Attempts)
	}
	if !got.NextAttemptAt.After(before) {
		t.Errorf("next_attempt_at = %v, want scheduled in the future (backoff)", got.NextAttemptAt)
	}
	if got.LastError != "boom" {
		t.Errorf("last_error = %q, want boom", got.LastError)
	}
}

// TestMarkFailed_PinsAttemptsWhenExhausted confirms an exhausted failure is made
// terminal by pinning attempts to the cap, so the worker stops retrying it.
func TestMarkFailed_PinsAttemptsWhenExhausted(t *testing.T) {
	withTestDB(t)
	ctx := context.Background()
	n := addNotif(t, Notification{NotificationID: uuid.New().String(), ChannelID: "c", Body: "b", Status: StatusPending, Attempts: 3})

	markFailed(ctx, n, "dead", true)

	got, _ := getNotification(ctx, n.NotificationID)
	if got.Attempts != maxAttempts {
		t.Errorf("attempts = %d, want pinned to maxAttempts (%d) when exhausted", got.Attempts, maxAttempts)
	}
}

// TestMarkRetryable_RollsBackAttempt covers the transient-failure fix: a transient
// channel-lookup error must not consume a delivery attempt, so the attempt that
// claimRetryable incremented is rolled back and the row stays claimable — even when
// it was on its final attempt.
func TestMarkRetryable_RollsBackAttempt(t *testing.T) {
	withTestDB(t)
	ctx := context.Background()
	// Simulate the final claim: claimRetryable has already incremented to maxAttempts.
	n := addNotif(t, Notification{NotificationID: uuid.New().String(), ChannelID: "c", Body: "b", Status: StatusPending, Attempts: maxAttempts})

	markRetryable(ctx, n, "channel lookup failed")

	got, err := getNotification(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Attempts != maxAttempts-1 {
		t.Errorf("attempts = %d, want rolled back to %d (transient errors must not consume the budget)", got.Attempts, maxAttempts-1)
	}
	if got.Attempts >= maxAttempts {
		t.Error("row must remain claimable (attempts < maxAttempts) after a transient failure")
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !got.NextAttemptAt.After(time.Now().UTC().Add(-time.Second)) {
		t.Errorf("next_attempt_at should be scheduled, got %v", got.NextAttemptAt)
	}
}

// TestProcessNotification_MissingChannelIsTerminal covers the deleted-channel path:
// a notification whose channel no longer exists is permanently failed (no point
// retrying).
func TestProcessNotification_MissingChannelIsTerminal(t *testing.T) {
	withTestDB(t)
	ctx := context.Background()
	n := addNotif(t, Notification{NotificationID: uuid.New().String(), ChannelID: "does-not-exist", Body: "b", Status: StatusPending, Attempts: 1})

	processNotification(ctx, n)

	got, _ := getNotification(ctx, n.NotificationID)
	if got.Status != StatusFailed || got.Attempts != maxAttempts {
		t.Errorf("missing channel should be terminal: status=%q attempts=%d (want failed/%d)", got.Status, got.Attempts, maxAttempts)
	}
	if got.LastError != "channel not found" {
		t.Errorf("last_error = %q, want 'channel not found'", got.LastError)
	}
}

// TestProcessNotification_DisabledChannelIsTerminal confirms delivery to a disabled
// channel is not retried.
func TestProcessNotification_DisabledChannelIsTerminal(t *testing.T) {
	withTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ch := Channel{
		ChannelID: uuid.New().String(), Name: "x", Type: ChannelTypeWebhook,
		Config: map[string]string{"url": "https://example.com"}, Enabled: false,
		Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := ch.Add(ctx); err != nil {
		t.Fatalf("add channel: %v", err)
	}
	n := addNotif(t, Notification{NotificationID: uuid.New().String(), ChannelID: ch.ChannelID, Body: "b", Status: StatusPending, Attempts: 1})

	processNotification(ctx, n)

	got, _ := getNotification(ctx, n.NotificationID)
	if got.Status != StatusFailed || got.Attempts != maxAttempts {
		t.Errorf("disabled channel should be terminal: status=%q attempts=%d", got.Status, got.Attempts)
	}
}
