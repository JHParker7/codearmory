package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

// A workflow whose scoped role was provisioned before the permission-derivation
// logic changed (role_perms_version < current) must re-provision its role on the
// next trigger, then never again. Without this a pre-fix workflow keeps a role
// missing the async-poll grant and every run hangs at "running".
func TestHandleTriggerRun_HealsStaleWorkflowRole(t *testing.T) {
	requireDB(t)
	withCatalog(t, map[string]ActionDef{
		"forge/run": {
			Name:               "forge/run",
			RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"},
			Async:              &AsyncConfig{PollPath: "/executions/{id}"},
		},
	})

	var roleCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "workflow-roles") && r.Method == http.MethodPost:
			roleCalls++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"role_id":"role-%d"}`, roleCalls)
		case strings.Contains(r.URL.Path, "workflow-roles") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "run-tokens"):
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"token":"run-token","session_id":"sess-1"}`)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"authorized":true,"user_id":"hu","org_id":"ho"}`)) //nolint:errcheck
		}
	}))
	origCliURL, origURL, origKey := gatekeeperClient.URL, gatekeeperURL, gatekeeperKey
	gatekeeperClient.URL = srv.URL
	gatekeeperURL = srv.URL
	gatekeeperKey = func() string { return "k" }
	t.Cleanup(func() {
		gatekeeperClient.URL = origCliURL
		gatekeeperURL = origURL
		gatekeeperKey = origKey
		srv.Close()
	})

	// A forge step + a workflow created through the normal path (role-1, current version).
	now := time.Now().UTC()
	step := Step{
		StepID: uuid.New().String(), Name: "fr-" + uuid.New().String(), Action: "forge/run",
		With: map[string]any{"image": "alpine:3.19", "run": "echo hi"}, Timeout: 30,
		CreatedBy: "hu", OrgID: "ho", Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := step.Add(context.Background()); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM steps WHERE step_id = ?`, step.StepID) }) //nolint:errcheck

	body, _ := json.Marshal(map[string]any{"name": "wf-" + uuid.New().String(), "steps": []map[string]any{{"step_id": step.StepID}}})
	cw := httptest.NewRecorder()
	handleCreateWorkflow(cw, authReq(http.MethodPost, "/pipelines", body))
	if cw.Code != http.StatusCreated {
		t.Fatalf("create workflow got %d: %s", cw.Code, cw.Body.String())
	}
	var wf Workflow
	json.Unmarshal(cw.Body.Bytes(), &wf) //nolint:errcheck
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, wf.WorkflowID) //nolint:errcheck
		connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID)      //nolint:errcheck
	})

	// Simulate a pre-fix workflow: stale version + an old role id.
	if err := connect().Exec(`UPDATE workflows SET role_perms_version = 0, role_id = ? WHERE workflow_id = ?`,
		"stale-role", wf.WorkflowID).Error; err != nil {
		t.Fatalf("force stale: %v", err)
	}
	callsBefore := roleCalls

	// Trigger → should heal (re-provision the role, bump the version).
	tr := authReq(http.MethodPost, "/pipelines/"+wf.WorkflowID+"/runs", []byte(`{}`))
	tr.SetPathValue("id", wf.WorkflowID)
	tw := httptest.NewRecorder()
	handleTriggerRun(tw, tr)
	if tw.Code != http.StatusCreated && tw.Code != http.StatusAccepted && tw.Code != http.StatusOK {
		t.Fatalf("trigger run got %d: %s", tw.Code, tw.Body.String())
	}
	if roleCalls == callsBefore {
		t.Fatal("expected a re-provision (workflow-roles POST) on stale trigger, got none")
	}

	got, err := getWorkflow(context.Background(), wf.WorkflowID)
	if err != nil {
		t.Fatalf("refetch workflow: %v", err)
	}
	if got.RolePermsVersion != workflowRolePermsVersion {
		t.Errorf("role_perms_version = %d, want %d", got.RolePermsVersion, workflowRolePermsVersion)
	}
	if got.RoleID == "stale-role" || got.RoleID == "" {
		t.Errorf("role_id not healed: %q", got.RoleID)
	}

	// Triggering again must NOT re-provision — the version is now current.
	callsAfterHeal := roleCalls
	tr2 := authReq(http.MethodPost, "/pipelines/"+wf.WorkflowID+"/runs", []byte(`{}`))
	tr2.SetPathValue("id", wf.WorkflowID)
	handleTriggerRun(httptest.NewRecorder(), tr2)
	if roleCalls != callsAfterHeal {
		t.Errorf("re-provisioned again though version current: calls %d -> %d", callsAfterHeal, roleCalls)
	}
}

// A create-volume step's run must also be able to tear its volumes down: the run
// role needs deleteVolume alongside createVolume, or the end-of-run DELETE 403s and
// volumes linger until forge's age reaper.
func TestCollectWorkflowPermissions_CreateVolumeGrantsDelete(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/create-volume": {
			Name:               "forge/create-volume",
			RequiredPermission: &PermissionSpec{Service: "forge", Action: "createVolume", Resource: "forge/volumes"},
		},
	})
	perms := collectWorkflowPermissions([]WorkflowStep{{Step: Step{Action: "forge/create-volume"}}})
	var hasCreate, hasDelete bool
	for _, p := range perms {
		if p.Service == "forge" && p.Resource == "forge/volumes" {
			switch p.Action {
			case "createVolume":
				hasCreate = true
			case "deleteVolume":
				hasDelete = true
			}
		}
	}
	if !hasCreate || !hasDelete {
		t.Fatalf("perms = %+v, want both createVolume and deleteVolume on forge/volumes", perms)
	}
}

func TestWorkflowUsesVolumes(t *testing.T) {
	if !workflowUsesVolumes([]WorkflowStep{{Step: Step{Action: "forge/run"}}, {Step: Step{Action: ActionForgeCreateVolume}}}) {
		t.Error("want true when a create-volume step is present")
	}
	if workflowUsesVolumes([]WorkflowStep{{Step: Step{Action: "forge/run"}}}) {
		t.Error("want false when no create-volume step")
	}
}
