package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRotateToken(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "u", "o") // createRunToken mints "run-token"; revoke target
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: "w1", TriggeredBy: "u", OrgID: "o", Status: "running", Token: "old", CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                                 //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck

	orig := rotationIntervalFn
	rotationIntervalFn = func() time.Duration { return 5 * time.Millisecond }
	t.Cleanup(func() { rotationIntervalFn = orig })

	store := newTokenStore("old", "oldsess")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&WorkerPool{}).rotateToken(ctx, store, run.RunID, "u", "role"); close(done) }()

	rotated := false
	for range 200 {
		if store.getToken() != "old" {
			rotated = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if !rotated {
		t.Error("run token was not rotated")
	}
}

func TestLoop_RunsAndStops(t *testing.T) {
	requireDB(t)
	connect().Exec(`DELETE FROM workflow_runs WHERE status='pending'`) //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	// Empty queue: tryOne→Dequeue finds nothing, loop backs off, then ctx times out.
	(&WorkerPool{}).loop(ctx)
}

func TestStart_SpawnsAndStops(t *testing.T) {
	requireDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	(&WorkerPool{}).Start(ctx, 2)
	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(30 * time.Millisecond) // let the loops observe cancellation and return
}
