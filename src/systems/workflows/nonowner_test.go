package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Does a stranger reach another user's records?
//
// This is the concrete question behind the "per-record resources should be
// namespace-first" work. Conductor can only template a resource from PATH parameters,
// and gatekeeper then prefixes the CALLER's own namespace — so "workflows/pipelines/{id}"
// evaluates to "<caller>/workflows/pipelines/<id>" no matter whose pipeline <id> is.
// Any caller holding a wildcard over their own pipelines is therefore authorized at the
// gateway for EVERY id in the system. The declaration looks like a per-record gate and
// is not one.
//
// What actually stands between a stranger and someone else's row is each handler
// re-checking ownership itself. That is a convention, and a convention is only as good
// as its coverage — the same sweep over git_factory found 19 of 24 per-record routes
// leaking. So this asserts the coverage rather than assuming it.
//
// The gatekeeper stub deliberately returns AUTHORIZED for a different user. That is not
// a permissive stub hiding the answer: it is precisely what the real gatekeeper returns
// here, because the stranger's own wildcard matches the caller-prefixed resource. The
// stub reproduces the authorization the deployed system grants, so every denial below
// comes from the service and nowhere else.

// strangerGatekeeper makes every CheckPermissions succeed as userID, with no org.
func strangerGatekeeper(t *testing.T, userID string) {
	t.Helper()
	fakeGatekeeper(t, http.StatusOK, `{"authorized":true,"user_id":"`+userID+`"}`)
}

// seedRunFor inserts a run of wf triggered by user.
func seedRunFor(t *testing.T, wf Workflow, user, org string) WorkflowRun {
	t.Helper()
	run := WorkflowRun{
		RunID: uuid.New().String(), WorkflowID: wf.WorkflowID,
		TriggeredBy: user, OrgID: org, Status: StatusPending, CreatedAt: time.Now().UTC(),
	}
	if err := run.Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id = ?`, run.RunID) }) //nolint:errcheck
	return run
}

func TestNonOwnerIsDeniedOnEveryPerRecordRoute(t *testing.T) {
	requireDB(t)

	const owner, stranger = "u-owner", "u-stranger"
	step := seedStep(t, owner, "org-owner")
	wf := seedWorkflow(t, owner, "org-owner")
	run := seedRunFor(t, wf, owner, "org-owner")

	// No org on the stranger: an empty orgID must not tenancy-match the victim's org.
	// canAccessStep/Run/Workflow all guard that with `orgID != ""`, and this is what
	// would catch it if one of them stopped.
	strangerGatekeeper(t, stranger)

	// A worker pool that is real but empty. Cancel() dereferences a sync.Map, so a nil
	// pool would panic — and a panic on a LEAKING route would obscure the leak with a
	// crash instead of reporting it.
	pool := &WorkerPool{}

	stepBody := `{"name":"hijacked","action":"echo","with":{"k":"v"},"timeout":30}`
	wfBody := `{"name":"hijacked","steps":[{"action":"http","name":"a","with":{"service":"forge","path":"/x"}}]}`

	routes := []struct {
		name   string
		method string
		target string
		vals   map[string]string
		body   string
		h      http.HandlerFunc
	}{
		{"GET /steps/{id}", http.MethodGet, "/steps/" + step.StepID,
			map[string]string{"id": step.StepID}, "", handleGetStep},
		{"PUT /steps/{id}", http.MethodPut, "/steps/" + step.StepID,
			map[string]string{"id": step.StepID}, stepBody, handleUpdateStep},
		{"DELETE /steps/{id}", http.MethodDelete, "/steps/" + step.StepID,
			map[string]string{"id": step.StepID}, "", handleDeleteStep},

		{"GET /pipelines/{id}", http.MethodGet, "/pipelines/" + wf.WorkflowID,
			map[string]string{"id": wf.WorkflowID}, "", handleGetWorkflow},
		{"PUT /pipelines/{id}", http.MethodPut, "/pipelines/" + wf.WorkflowID,
			map[string]string{"id": wf.WorkflowID}, wfBody, handleUpdateWorkflow},
		{"DELETE /pipelines/{id}", http.MethodDelete, "/pipelines/" + wf.WorkflowID,
			map[string]string{"id": wf.WorkflowID}, "", handleDeleteWorkflow},

		{"POST /pipelines/{id}/runs", http.MethodPost, "/pipelines/" + wf.WorkflowID + "/runs",
			map[string]string{"id": wf.WorkflowID}, `{}`, handleTriggerRun},

		{"GET /runs/{id}", http.MethodGet, "/runs/" + run.RunID,
			map[string]string{"id": run.RunID}, "", handleGetRun},
		{"DELETE /runs/{id}", http.MethodDelete, "/runs/" + run.RunID,
			map[string]string{"id": run.RunID}, "", handleCancelRun(pool)},
		{"POST /runs/{id}/approve", http.MethodPost, "/runs/" + run.RunID + "/approve",
			map[string]string{"id": run.RunID}, `{}`, handleApproveRun},
		{"POST /runs/{id}/reject", http.MethodPost, "/runs/" + run.RunID + "/reject",
			map[string]string{"id": run.RunID}, `{}`, handleRejectRun},

		{"GET /pipelines/{id}/runs/{run_id}", http.MethodGet,
			"/pipelines/" + wf.WorkflowID + "/runs/" + run.RunID,
			map[string]string{"id": wf.WorkflowID, "run_id": run.RunID}, "", handleGetRun},
		{"DELETE /pipelines/{id}/runs/{run_id}", http.MethodDelete,
			"/pipelines/" + wf.WorkflowID + "/runs/" + run.RunID,
			map[string]string{"id": wf.WorkflowID, "run_id": run.RunID}, "", handleCancelRun(pool)},
		{"POST /pipelines/{id}/runs/{run_id}/approve", http.MethodPost,
			"/pipelines/" + wf.WorkflowID + "/runs/" + run.RunID + "/approve",
			map[string]string{"id": wf.WorkflowID, "run_id": run.RunID}, `{}`, handleApproveRun},
		{"POST /pipelines/{id}/runs/{run_id}/reject", http.MethodPost,
			"/pipelines/" + wf.WorkflowID + "/runs/" + run.RunID + "/reject",
			map[string]string{"id": wf.WorkflowID, "run_id": run.RunID}, `{}`, handleRejectRun},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			var body *strings.Reader
			if rt.body != "" {
				body = strings.NewReader(rt.body)
			} else {
				body = strings.NewReader("")
			}
			r := httptest.NewRequest(rt.method, rt.target, body)
			r.Header.Set("Authorization", "Bearer stranger-tok")
			r.Header.Set("Content-Type", "application/json")
			for k, v := range rt.vals {
				r.SetPathValue(k, v)
			}
			w := httptest.NewRecorder()
			rt.h(w, r)

			// 404 is the preferred answer (it does not confirm the id exists), 403 is
			// acceptable. Anything else — especially a 2xx — is the leak.
			if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
				t.Errorf("a non-owner got %d, want 404 or 403: %s", w.Code, w.Body.String())
			}
			// A denial must also not narrate the record. Leaking the name through an
			// error body is a smaller hole than serving the row, but it is the same
			// hole.
			for _, secret := range []string{step.Name, wf.Name} {
				if secret != "" && strings.Contains(w.Body.String(), secret) {
					t.Errorf("denial body leaked %q: %s", secret, w.Body.String())
				}
			}
		})
	}

	// The victim's rows must be exactly as seeded. A route that answered 404 while
	// still performing the write would pass every check above.
	t.Run("victim rows untouched", func(t *testing.T) {
		gotStep, err := (Step{StepID: step.StepID}).Get(context.Background())
		if err != nil {
			t.Fatalf("step was deleted or is unreadable: %v", err)
		}
		if s := gotStep.(Step); s.Name != step.Name || s.CreatedBy != owner {
			t.Errorf("step mutated: name=%q created_by=%q", s.Name, s.CreatedBy)
		}

		gotWf, err := (Workflow{WorkflowID: wf.WorkflowID}).Get(context.Background())
		if err != nil {
			t.Fatalf("pipeline was deleted or is unreadable: %v", err)
		}
		if f := gotWf.(Workflow); f.Name != wf.Name || f.CreatedBy != owner {
			t.Errorf("pipeline mutated: name=%q created_by=%q", f.Name, f.CreatedBy)
		}

		gotRun, err := (WorkflowRun{RunID: run.RunID}).Get(context.Background())
		if err != nil {
			t.Fatalf("run was deleted or is unreadable: %v", err)
		}
		if rn := gotRun.(WorkflowRun); rn.Status != StatusPending {
			t.Errorf("run status = %q, want %q — a stranger moved someone else's run",
				rn.Status, StatusPending)
		}
	})
}
