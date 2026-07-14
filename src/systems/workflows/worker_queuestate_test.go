package main

import (
	"context"
	"testing"
)

func TestIsQueuedState_DefaultsToPending(t *testing.T) {
	// An action that declares no queued_states (every manifest today) must still treat
	// forge's "pending" as queued — that is what makes the behaviour work with no
	// manifest change.
	async := &AsyncConfig{}
	if !isQueuedState(async, StatusPending) {
		t.Fatal("pending should be queued by default")
	}
	for _, s := range []string{StatusRunning, StatusCompleted, StatusFailed, ""} {
		if isQueuedState(async, s) {
			t.Fatalf("%q should not be queued by default", s)
		}
	}
}

func TestIsQueuedState_ExplicitStatesReplaceTheDefault(t *testing.T) {
	async := &AsyncConfig{QueuedStates: []string{"queued", "admission_hold"}}
	for _, s := range []string{"queued", "admission_hold"} {
		if !isQueuedState(async, s) {
			t.Fatalf("%q should be queued when declared", s)
		}
	}
	// Declaring queued_states replaces the default, so "pending" is no longer implied.
	if isQueuedState(async, StatusPending) {
		t.Fatal("explicit queued_states should replace the pending default")
	}
}

func TestStepRunIDFrom_EmptyWhenAbsent(t *testing.T) {
	// A scatter's resolve/gather owns no step run; the poll must degrade to a no-op
	// rather than writing status onto some other row.
	if got := stepRunIDFrom(context.Background()); got != "" {
		t.Fatalf("want empty step run id, got %q", got)
	}
	ctx := withStepRunID(context.Background(), "sr-1")
	if got := stepRunIDFrom(ctx); got != "sr-1" {
		t.Fatalf("want sr-1, got %q", got)
	}
}
