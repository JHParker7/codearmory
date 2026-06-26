package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestValidateResourceName(t *testing.T) {
	valid := []string{"deploy-prod", "build_step", "a", "v1.2.3", "DeployProd", strings.Repeat("x", 100)}
	for _, n := range valid {
		if msg := validateResourceName(n); msg != "" {
			t.Errorf("validateResourceName(%q) = %q, want accepted", n, msg)
		}
	}
	invalid := []string{"", "has space", "a/b", "trailing/", "tab\tname", strings.Repeat("x", 101), "emoji😀"}
	for _, n := range invalid {
		if msg := validateResourceName(n); msg == "" {
			t.Errorf("validateResourceName(%q) accepted, want rejected", n)
		}
	}
}

// resolveWorkflowRef/resolveStepRef map a URL ref to a row by unique name (scoped
// to the caller) first, falling back to the id — and never resolve another
// owner's row by name.
func TestNameOrIdResolution(t *testing.T) {
	if !testDBReady {
		t.Skip("test DB not initialized")
	}
	ctx := context.Background()
	user := "u-resolve-1"
	now := time.Now().UTC()

	st := Step{StepID: "step-resolve-1", Name: "build-app", Action: "forge/run", CreatedBy: user, Active: true, CreatedAt: now, UpdatedAt: now}
	if err := st.Add(ctx); err != nil {
		t.Fatalf("add step: %v", err)
	}
	wf := Workflow{WorkflowID: "wf-resolve-1", Name: "deploy-prod", CreatedBy: user, Active: true, StepRefs: []WorkflowStepRef{}, CreatedAt: now, UpdatedAt: now}
	if err := wf.Add(ctx); err != nil {
		t.Fatalf("add workflow: %v", err)
	}

	if got, err := resolveStepRef(ctx, "build-app", user, ""); err != nil || got.StepID != "step-resolve-1" {
		t.Fatalf("resolveStepRef by name = %+v, %v", got, err)
	}
	if got, err := resolveStepRef(ctx, "step-resolve-1", user, ""); err != nil || got.Name != "build-app" {
		t.Fatalf("resolveStepRef by id = %+v, %v", got, err)
	}
	if got, err := resolveWorkflowRef(ctx, "deploy-prod", user, ""); err != nil || got.WorkflowID != "wf-resolve-1" {
		t.Fatalf("resolveWorkflowRef by name = %+v, %v", got, err)
	}
	if got, err := resolveWorkflowRef(ctx, "wf-resolve-1", user, ""); err != nil || got.Name != "deploy-prod" {
		t.Fatalf("resolveWorkflowRef by id = %+v, %v", got, err)
	}
	// Owner-scoped: a different user cannot resolve the name (no leak across users).
	if _, err := resolveWorkflowRef(ctx, "deploy-prod", "other-user", ""); err == nil {
		t.Fatal("resolveWorkflowRef resolved another user's workflow by name")
	}
}

// workflowNameExists / *Conflict back the create + rename uniqueness guards.
func TestNameUniquenessHelpers(t *testing.T) {
	if !testDBReady {
		t.Skip("test DB not initialized")
	}
	ctx := context.Background()
	user := "u-unique-1"
	now := time.Now().UTC()
	wf := Workflow{WorkflowID: "wf-unique-1", Name: "nightly", CreatedBy: user, Active: true, StepRefs: []WorkflowStepRef{}, CreatedAt: now, UpdatedAt: now}
	if err := wf.Add(ctx); err != nil {
		t.Fatalf("add workflow: %v", err)
	}

	if exists, err := workflowNameExists(ctx, "nightly", user, ""); err != nil || !exists {
		t.Fatalf("workflowNameExists(own) = %v, %v; want true", exists, err)
	}
	if exists, _ := workflowNameExists(ctx, "nightly", "different-user", ""); exists {
		t.Fatal("workflowNameExists leaked across owners")
	}
	// Rename: keeping its own name is allowed; another row with the name conflicts.
	if conflict, _ := workflowNameConflict(ctx, "nightly", "wf-unique-1", user, ""); conflict {
		t.Fatal("keeping own name reported a conflict")
	}
	if conflict, _ := workflowNameConflict(ctx, "nightly", "wf-other", user, ""); !conflict {
		t.Fatal("a different row with the same name should conflict")
	}
}
