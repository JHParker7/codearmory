package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// roleStub is a fake gatekeeper that authorizes everything, mints run tokens, and lets
// a test control (and observe) the workflow-role endpoints.
type roleStub struct {
	mu          sync.Mutex
	provisioned int
	deleted     []string
	// failProvision, once true, makes every subsequent POST /internal/workflow-roles
	// fail — the transient-gatekeeper-error case.
	failProvision bool
}

func (s *roleStub) setFailProvision(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failProvision = v
}

func (s *roleStub) deletedRoles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// newRoleStub installs the stub as gatekeeper for the test's duration, with a catalog
// containing one permission-bearing action so a workflow actually needs a role.
func newRoleStub(t *testing.T, user, org string) *roleStub {
	t.Helper()
	withCatalog(t, map[string]ActionDef{
		"forge/run": {
			Name:               "forge/run",
			ServiceName:        "forge",
			Method:             http.MethodPost,
			Path:               "/executions",
			RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"},
		},
	})
	s := &roleStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "workflow-roles") && r.Method == http.MethodPost:
			s.mu.Lock()
			fail := s.failProvision
			if !fail {
				s.provisioned++
			}
			n := s.provisioned
			s.mu.Unlock()
			if fail {
				http.Error(w, "gatekeeper unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"role_id": "role-" + strconv.Itoa(n)}) //nolint:errcheck
		case strings.Contains(r.URL.Path, "workflow-roles") && r.Method == http.MethodDelete:
			s.mu.Lock()
			s.deleted = append(s.deleted, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "run-tokens"):
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"token":"run-token","session_id":"sess-1"}`)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"authorized":true,"user_id":"` + user + `","org_id":"` + org + `"}`)) //nolint:errcheck
		}
	}))
	origCli, origURL, origKey := gatekeeperClient.URL, gatekeeperURL, gatekeeperKey
	gatekeeperClient.URL = srv.URL
	gatekeeperURL = srv.URL
	gatekeeperKey = func() string { return "k" }
	t.Cleanup(func() {
		gatekeeperClient.URL = origCli
		gatekeeperURL = origURL
		gatekeeperKey = origKey
		srv.Close()
	})
	return s
}

// createRoleWorkflow POSTs a pipeline with one permission-bearing step (plus optional
// extra request fields) and returns the stored workflow.
func createRoleWorkflow(t *testing.T, extra map[string]any) Workflow {
	t.Helper()
	body := map[string]any{
		"name":  "wf-" + uuid.New().String(),
		"steps": []map[string]any{{"name": "build", "action": "forge/run"}},
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", raw))
	if w.Code != http.StatusCreated {
		t.Fatalf("create workflow got %d: %s", w.Code, w.Body.String())
	}
	var wf Workflow
	json.Unmarshal(w.Body.Bytes(), &wf) //nolint:errcheck
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM workflow_runs WHERE workflow_id = ?`, wf.WorkflowID) //nolint:errcheck
		connect().Exec(`DELETE FROM workflows WHERE workflow_id = ?`, wf.WorkflowID)     //nolint:errcheck
	})
	return wf
}

func storedWorkflow(t *testing.T, id string) Workflow {
	t.Helper()
	wf, err := getWorkflow(context.Background(), id)
	if err != nil {
		t.Fatalf("refetch workflow: %v", err)
	}
	return wf
}

// A create whose role provisioning fails must fail the request, not persist a workflow
// with no role. An empty role id means "no role" to createRunToken, so every run of the
// workflow would carry the OWNER'S FULL SESSION PERMISSIONS — and the stamped-current
// RolePermsVersion would stop the trigger-time heal from ever retrying.
func TestHandleCreateWorkflow_RoleProvisionFailureFailsRequest(t *testing.T) {
	requireDB(t)
	stub := newRoleStub(t, "ru", "ro")
	stub.setFailProvision(true)

	body, _ := json.Marshal(map[string]any{
		"name":  "wf-" + uuid.New().String(),
		"steps": []map[string]any{{"name": "build", "action": "forge/run"}},
	})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 — a failed role provision must not create a workflow that runs unscoped: %s", w.Code, w.Body.String())
	}
}

// The same on update: keep the workflow's existing role, keep its version, and do NOT
// delete the previous scoped role.
func TestHandleUpdateWorkflow_RoleProvisionFailureKeepsExistingRole(t *testing.T) {
	requireDB(t)
	stub := newRoleStub(t, "ru", "ro")
	wf := createRoleWorkflow(t, nil)
	if wf.RoleID == "" {
		t.Fatalf("setup: workflow was created without a role")
	}

	stub.setFailProvision(true)
	body, _ := json.Marshal(map[string]any{
		"name":  wf.Name,
		"steps": []map[string]any{{"name": "build", "action": "forge/run"}},
	})
	r := authReq(http.MethodPut, "/pipelines/"+wf.WorkflowID, body)
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleUpdateWorkflow(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 on a failed re-provision: %s", w.Code, w.Body.String())
	}

	got := storedWorkflow(t, wf.WorkflowID)
	if got.RoleID != wf.RoleID {
		t.Errorf("role_id changed to %q on a failed provision, want the stored %q kept", got.RoleID, wf.RoleID)
	}
	for _, d := range stub.deletedRoles() {
		if d == wf.RoleID {
			t.Errorf("the workflow's live role %q was deleted after a failed re-provision", d)
		}
	}
}

// A state_machine PUT carries no ticket field — and it is the only round-trippable edit
// shape — so an unguarded assignment silently deleted the workflow's ticket-mirroring
// config and re-provisioned its role without the ticket permissions.
func TestHandleUpdateWorkflow_StateMachinePutKeepsTicketConfig(t *testing.T) {
	requireDB(t)
	newRoleStub(t, "ru", "ro")
	wf := createRoleWorkflow(t, map[string]any{
		"ticket": map[string]any{"enabled": true, "title": "run of ${inputs.env}", "board_id": "b-1"},
	})
	if wf.Ticket == nil || !wf.Ticket.Enabled {
		t.Fatalf("setup: ticket config was not stored: %+v", wf.Ticket)
	}

	// Exactly what a GET returns and a client PUTs back: the computed state machine.
	sm := modelToSM(wf.Name, wf.Description, wf.Steps, deriveRoutes(wf.Steps), wf.Maps, wf.Inputs, wf.Outputs)
	body, _ := json.Marshal(map[string]any{"name": wf.Name, "state_machine": sm})
	r := authReq(http.MethodPut, "/pipelines/"+wf.WorkflowID, body)
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleUpdateWorkflow(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("state_machine PUT got %d: %s", w.Code, w.Body.String())
	}

	got := storedWorkflow(t, wf.WorkflowID)
	if got.Ticket == nil || !got.Ticket.Enabled {
		t.Fatalf("ticket config wiped by a state_machine round-trip: %+v", got.Ticket)
	}
	if got.Ticket.BoardID != "b-1" || got.Ticket.Title != "run of ${inputs.env}" {
		t.Errorf("ticket config mangled: %+v", got.Ticket)
	}
}

// Deleting a workflow must not revoke the role an in-flight run is authenticating with:
// its remaining steps would 403 and leave a half-applied deploy. The role is instead
// reclaimed by the run-completion GC in the worker.
func TestHandleDeleteWorkflow_KeepsRoleWhileARunIsInFlight(t *testing.T) {
	requireDB(t)
	stub := newRoleStub(t, "ru", "ro")
	wf := createRoleWorkflow(t, nil)

	run := WorkflowRun{
		RunID: uuid.New().String(), WorkflowID: wf.WorkflowID, TriggeredBy: "ru", OrgID: "ro",
		Status: StatusRunning, RoleID: wf.RoleID, CreatedAt: time.Now().UTC(),
	}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	r := authReq(http.MethodDelete, "/pipelines/"+wf.WorkflowID, nil)
	r.SetPathValue("id", wf.WorkflowID)
	w := httptest.NewRecorder()
	handleDeleteWorkflow(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete got %d: %s", w.Code, w.Body.String())
	}
	for _, d := range stub.deletedRoles() {
		if d == wf.RoleID {
			t.Fatalf("role %q was revoked while a run was still using it", d)
		}
	}

	// Once the run is terminal the same call reclaims it, so the role is not leaked.
	connect().Exec(`UPDATE workflow_runs SET status='completed' WHERE run_id=?`, run.RunID) //nolint:errcheck
	deleteWorkflowRoleIfUnused(context.Background(), wf.RoleID, "")
	var reclaimed bool
	for _, d := range stub.deletedRoles() {
		if d == wf.RoleID {
			reclaimed = true
		}
	}
	if !reclaimed {
		t.Errorf("role %q was never reclaimed after the run finished", wf.RoleID)
	}
}
