package main

import (
	"context"
	"testing"
)

// TestDBMethods_UnderBrokenDB drives every db-layer method against a closed
// connection, exercising the error-return branch of each (graceful propagation,
// no panics). Functions that swallow errors internally (Complete/SetCurrentStep)
// are exercised for their error-logging path.
func TestDBMethods_UnderBrokenDB(t *testing.T) {
	withBrokenDB(t)
	ctx := context.Background()

	mustErr := func(name string, err error) {
		if err == nil {
			t.Errorf("%s: expected an error under broken DB", name)
		}
	}

	mustErr("Step.Add", (Step{StepID: "x"}).Add(ctx))
	mustErr("Step.Update", (Step{StepID: "x"}).Update(ctx))
	mustErr("Step.Remove", (Step{StepID: "x"}).Remove(ctx))
	if _, err := (Step{StepID: "x"}).Get(ctx); err == nil {
		t.Error("Step.Get: expected error")
	}
	if _, err := (Step{}).List(ctx, 10, 0); err == nil {
		t.Error("Step.List: expected error")
	}

	mustErr("Workflow.Add", (Workflow{WorkflowID: "x"}).Add(ctx))
	mustErr("Workflow.Update", (Workflow{WorkflowID: "x"}).Update(ctx))
	mustErr("Workflow.Remove", (Workflow{WorkflowID: "x"}).Remove(ctx))
	if _, err := (Workflow{WorkflowID: "x"}).Get(ctx); err == nil {
		t.Error("Workflow.Get: expected error")
	}
	if _, err := (Workflow{}).List(ctx, 10, 0); err == nil {
		t.Error("Workflow.List: expected error")
	}

	mustErr("WorkflowRun.Add", (WorkflowRun{RunID: "x"}).Add(ctx))
	mustErr("WorkflowRun.Update", (WorkflowRun{RunID: "x"}).Update(ctx))
	mustErr("WorkflowRun.Remove", (WorkflowRun{RunID: "x"}).Remove(ctx))
	mustErr("WorkflowRun.UpdateToken", (WorkflowRun{RunID: "x"}).UpdateToken(ctx, "t", "s"))
	if _, err := (WorkflowRun{RunID: "x"}).Get(ctx); err == nil {
		t.Error("WorkflowRun.Get: expected error")
	}
	if _, err := (WorkflowRun{}).List(ctx, 10, 0); err == nil {
		t.Error("WorkflowRun.List: expected error")
	}
	if _, err := (WorkflowRun{}).Dequeue(ctx); err == nil {
		t.Error("WorkflowRun.Dequeue: expected error")
	}
	(WorkflowRun{RunID: "x"}).SetCurrentStep(ctx, 1) // logs internally; just exercise
	(WorkflowRun{RunID: "x"}).Complete(ctx, StatusFailed)

	mustErr("WorkflowStepRun.Add", (WorkflowStepRun{StepRunID: "x"}).Add(ctx))
	mustErr("WorkflowStepRun.Update", (WorkflowStepRun{StepRunID: "x"}).Update(ctx))
	if _, err := (WorkflowStepRun{StepRunID: "x"}).Get(ctx); err == nil {
		t.Error("WorkflowStepRun.Get: expected error")
	}
	if _, err := (WorkflowStepRun{}).List(ctx, 10, 0); err == nil {
		t.Error("WorkflowStepRun.List: expected error")
	}
	(WorkflowStepRun{StepRunID: "x"}).Complete(ctx, StatusFailed, nil, nil, nil)

	// Package-level query helpers.
	if _, err := cancelRun(ctx, "x"); err == nil {
		t.Error("cancelRun: expected error")
	}
	if _, err := stepNameExists(ctx, "n", "u", "o"); err == nil {
		t.Error("stepNameExists: expected error")
	}
	if _, err := listSteps(ctx, "u", "o", ""); err == nil {
		t.Error("listSteps: expected error")
	}
	if _, err := listWorkflows(ctx, "u", "o", ""); err == nil {
		t.Error("listWorkflows: expected error")
	}
	if _, err := listRuns(ctx, "u", "o", ""); err == nil {
		t.Error("listRuns: expected error")
	}
	if _, err := getStep(ctx, "x"); err == nil {
		t.Error("getStep: expected error")
	}
	if _, err := getWorkflow(ctx, "x"); err == nil {
		t.Error("getWorkflow: expected error")
	}
	if _, err := getRun(ctx, "x"); err == nil {
		t.Error("getRun: expected error")
	}
	recoverStuckRunsDB() // returns count; exercise under broken DB
}
