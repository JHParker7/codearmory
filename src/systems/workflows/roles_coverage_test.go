package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withCatalog installs an action catalog for the test and restores it after.
func withCatalog(t *testing.T, c map[string]ActionDef) {
	t.Helper()
	actionCatalogMu.Lock()
	orig := actionCatalog
	actionCatalog = c
	actionCatalogMu.Unlock()
	t.Cleanup(func() {
		actionCatalogMu.Lock()
		actionCatalog = orig
		actionCatalogMu.Unlock()
	})
}

func TestCollectWorkflowPermissions(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run": {Name: "forge/run", RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"}},
	})
	perms := collectWorkflowPermissions([]WorkflowStep{
		{Step: Step{Action: "forge/run"}}, {Step: Step{Action: "forge/run"}}, // dup collapses
		{Step: Step{Action: ActionHTTP}},    // skipped (runtime-dynamic)
		{Step: Step{Action: "unknown/act"}}, // not in catalog → skipped
	})
	if len(perms) != 1 || perms[0].Action != "createExecution" {
		t.Fatalf("perms = %+v, want one createExecution", perms)
	}
}

// An async action must also grant the poll (read) permission, or the run's scoped
// role can submit the job but not read its status — every poll 403s and the step
// hangs to the timeout instead of completing.
func TestCollectWorkflowPermissions_AsyncGrantsPollPermission(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run": {
			Name:               "forge/run",
			RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"},
			Async:              &AsyncConfig{PollPath: "/executions/{id}"},
		},
	})
	perms := collectWorkflowPermissions([]WorkflowStep{{Step: Step{Action: "forge/run"}}})
	var hasCreate, hasPoll bool
	for _, p := range perms {
		if p.Service == "forge" && p.Action == "createExecution" && p.Resource == "forge/executions" {
			hasCreate = true
		}
		if p.Service == "forge" && p.Action == "getExecution" && p.Resource == "forge/executions/*" {
			hasPoll = true
		}
	}
	if !hasCreate || !hasPoll {
		t.Fatalf("perms = %+v, want createExecution (submit) + getExecution on forge/executions/* (poll)", perms)
	}
}

func TestProvisionAndDeleteWorkflowRole_Success(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run": {Name: "forge/run", RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"}},
	})
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"role_id":"role-9"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	origURL, origKey := gatekeeperURL, gatekeeperKey
	gatekeeperURL = srv.URL
	gatekeeperKey = func() string { return "k" }
	t.Cleanup(func() { gatekeeperURL = origURL; gatekeeperKey = origKey })

	rid := provisionWorkflowRole(context.Background(), "wf1", "u1", "o1", []WorkflowStep{{Step: Step{Action: "forge/run"}}})
	if rid != "role-9" {
		t.Fatalf("role id = %q, want role-9", rid)
	}
	deleteWorkflowRole(context.Background(), rid)
	if !deleted {
		t.Error("deleteWorkflowRole did not call gatekeeper")
	}
}

func TestProvisionWorkflowRole_EmptyPerms(t *testing.T) {
	withCatalog(t, map[string]ActionDef{})
	// No catalog permissions → returns "" without calling gatekeeper.
	if rid := provisionWorkflowRole(context.Background(), "wf", "u", "o", []WorkflowStep{{Step: Step{Action: ActionHTTP}}}); rid != "" {
		t.Errorf("expected empty role id, got %q", rid)
	}
}

func TestHandleTriggerRun_InvalidBody(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "u", "o")
	wf := seedWorkflow(t, "u", "o")
	r := authReq(http.MethodPost, "/pipelines/"+wf.WorkflowID+"/runs", []byte("{bad json"))
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleTriggerRun(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}
