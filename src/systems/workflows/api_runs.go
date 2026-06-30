package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

// createRunToken asks gatekeeper to mint a short-lived session JWT for userID.
// When roleID is non-empty the token is scoped to only the permissions in that
// role (the workflow's minimal service role).
func createRunToken(ctx context.Context, userID, roleID string) (token, sessionID string, err error) {
	key := gatekeeperKey()
	if key == "" {
		return "", "", fmt.Errorf("gatekeeper service key not available")
	}
	payload := map[string]any{"user_id": userID}
	if roleID != "" {
		payload["role_id"] = roleID
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gatekeeperURL+"/internal/run-tokens", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "workflows:"+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("gatekeeper returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result struct {
		Token     string `json:"token"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}
	return result.Token, result.SessionID, nil
}

// resolveUsername turns a user UUID into its human-readable username for audit
// lines (e.g. the approval gate's "approved by alice"). It forwards the caller's
// own bearer to gatekeeper's GET /users/{id}: the default grant lets every user
// read their own record, which is exactly the self-lookup the approval path needs.
// Fails open to the UUID on any error so an audit line is never lost.
func resolveUsername(ctx context.Context, bearer, userID string) string {
	if bearer == "" || userID == "" {
		return userID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatekeeperURL+"/users/"+userID, nil)
	if err != nil {
		return userID
	}
	req.Header.Set("Authorization", bearer)
	resp, err := httpClient.Do(req)
	if err != nil {
		return userID
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return userID
	}
	var u struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil || u.Username == "" {
		return userID
	}
	return u.Username
}

// revokeRunToken revokes the gatekeeper session associated with a run. Failures
// are logged but never propagate — a missing revocation is better than a failed run.
func revokeRunToken(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	key := gatekeeperKey()
	if key == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		gatekeeperURL+"/internal/run-tokens/"+sessionID, nil)
	if err != nil {
		slog.WarnContext(ctx, "revokeRunToken: build request failed", "session_id", sessionID, "error", err)
		return
	}
	req.Header.Set("X-Service-Key", "workflows:"+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "revokeRunToken: request failed", "session_id", sessionID, "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		slog.WarnContext(ctx, "revokeRunToken: unexpected status", "session_id", sessionID, "status", resp.StatusCode)
	}
}

type triggerRunRequest struct {
	// Inputs are extra env vars injected into every step. Step-level env takes precedence.
	Inputs map[string]string `json:"inputs"`
}

// canAccessRun returns true when the caller may read the run:
// the caller triggered it, or they share the same org.
func canAccessRun(run WorkflowRun, userID, orgID string) bool {
	return run.TriggeredBy == userID || (orgID != "" && run.OrgID == orgID)
}

func handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleTriggerRun")
	defer span.End()

	workflowID := r.PathValue("id")
	// Run resources are namespaced under the workflow ref so access can be granted
	// per workflow (e.g. trigger runs of "deploy-prod"). The default grant
	// {username}/workflows/runs/* still covers it.
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "triggerRun", "workflows/runs/"+workflowID)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	wf, err := resolveWorkflowRef(ctx, workflowID, userID, orgID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "trigger run: get workflow", "workflow_id", workflowID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(wf, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	if len(wf.StepRefs) == 0 {
		http.Error(w, "pipeline has no steps", http.StatusBadRequest)
		return
	}
	// wf.Steps is enriched by getWorkflow; any ref whose step was deleted will be absent.
	if len(wf.Steps) < len(wf.StepRefs) {
		http.Error(w, "one or more referenced steps no longer exist", http.StatusBadRequest)
		return
	}

	var req triggerRunRequest
	if r.ContentLength != 0 {
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
	}
	if req.Inputs == nil {
		req.Inputs = map[string]string{}
	}

	// Heal a stale scoped role. The run role is provisioned once at create/update
	// and reused for every run, so a workflow created before a change to the
	// permission-derivation logic keeps a role missing newer permissions — e.g.
	// the async-poll read grant — and every run hangs polling a forbidden status.
	// Re-provision (using the workflow owner/org, not the triggerer) when the
	// stored role predates the current derivation version. Only commit the bump
	// when we have a usable role or the workflow legitimately needs none, so a
	// transient gatekeeper error retries next trigger instead of locking in an
	// empty role.
	if wf.RolePermsVersion < workflowRolePermsVersion {
		oldRole := wf.RoleID
		newRole := provisionWorkflowRole(ctx, wf.WorkflowID, wf.CreatedBy, wf.OrgID, wf.Steps)
		if newRole != "" || len(collectWorkflowPermissions(wf.Steps)) == 0 {
			wf.RoleID = newRole
			wf.RolePermsVersion = workflowRolePermsVersion
			if err := wf.Update(ctx); err != nil {
				slog.WarnContext(ctx, "trigger run: persist re-provisioned role", "workflow_id", wf.WorkflowID, "error", err)
			} else {
				slog.InfoContext(ctx, "trigger run: healed stale workflow role", "workflow_id", wf.WorkflowID, "role_id", newRole)
				if oldRole != "" && oldRole != newRole {
					deleteWorkflowRole(ctx, oldRole)
				}
			}
		} else {
			slog.WarnContext(ctx, "trigger run: role re-provision returned empty, using existing role", "workflow_id", wf.WorkflowID)
		}
	}

	runToken, sessionID, err := createRunToken(ctx, userID, wf.RoleID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "run token creation failed")
		slog.ErrorContext(ctx, "trigger run: failed to create run token", "workflow_id", workflowID, "user_id", userID, "error", err)
		http.Error(w, "failed to provision run credentials", http.StatusInternalServerError)
		return
	}
	encToken, err := encryptToken(runToken)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "run token encryption failed")
		slog.ErrorContext(ctx, "trigger run: failed to encrypt run token", "workflow_id", workflowID, "user_id", userID, "error", err)
		revokeRunToken(context.Background(), sessionID)
		http.Error(w, "failed to provision run credentials", http.StatusInternalServerError)
		return
	}

	run := WorkflowRun{
		RunID:        uuid.New().String(),
		WorkflowID:   wf.WorkflowID,
		TriggeredBy:  userID,
		OrgID:        orgID,
		Project:      wf.Project,
		Status:       StatusPending,
		Inputs:       req.Inputs,
		Token:        encToken,
		RunSessionID: sessionID,
		StepRuns:     []WorkflowStepRun{},
		CreatedAt:    time.Now().UTC(),
	}

	if err := run.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "trigger run: db error", "workflow_id", workflowID, "user_id", userID, "error", err)
		revokeRunToken(context.Background(), sessionID)
		http.Error(w, "failed to trigger run", http.StatusInternalServerError)
		return
	}

	meterRunsTriggered.Add(ctx, 1, metric.WithAttributes(attribute.String("workflow.id", wf.WorkflowID)))
	span.SetAttributes(attribute.String("run.id", run.RunID), attribute.String("workflow.id", wf.WorkflowID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "workflow run triggered", "run_id", run.RunID, "workflow_id", wf.WorkflowID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}

func handleListRuns(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleListRuns")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRun", "workflows/runs")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	runs, err := listRuns(ctx, userID, orgID, r.URL.Query().Get("workflow_id"))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list runs: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list runs", http.StatusInternalServerError)
		return
	}
	for i := range runs {
		runs[i].StepRuns = []WorkflowStepRun{}
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runs) //nolint:errcheck
}

// runRefAndResource extracts the run id and its RBAC resource from either the
// flat route (/runs/{id}) or the workflow-namespaced route
// (/pipelines/{id}/runs/{run_id}). On the nested route {id} is the workflow ref,
// so the resource is workflows/runs/<workflow_ref>/<run_id>; on the flat route it
// is the un-namespaced workflows/runs/<run_id>. nestedWorkflowRef is "" for the
// flat route.
func runRefAndResource(r *http.Request) (runID, resource, nestedWorkflowRef string) {
	if rid := r.PathValue("run_id"); rid != "" {
		ref := r.PathValue("id")
		return rid, "workflows/runs/" + ref + "/" + rid, ref
	}
	id := r.PathValue("id")
	return id, "workflows/runs/" + id, ""
}

// runMatchesWorkflowRef confirms a run belongs to the workflow named by a nested
// route's {id} ref, so /pipelines/deploy-prod/runs/<id> can't surface a run from
// a different workflow. A no-op on the flat route (ref == "").
func runMatchesWorkflowRef(ctx context.Context, ref string, run WorkflowRun, userID, orgID string) bool {
	if ref == "" {
		return true
	}
	wf, err := resolveWorkflowRef(ctx, ref, userID, orgID)
	return err == nil && wf.WorkflowID == run.WorkflowID
}

func handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetRun")
	defer span.End()

	id, resource, wfRef := runRefAndResource(r)
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRun", resource)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	run, err := getRun(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "get run: db error", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get run", http.StatusInternalServerError)
		return
	}
	if !canAccessRun(run, userID, orgID) || !runMatchesWorkflowRef(ctx, wfRef, run, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	stepRuns, err := getStepRuns(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query step runs failed")
		slog.ErrorContext(ctx, "get run: step runs query", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get run steps", http.StatusInternalServerError)
		return
	}
	run.StepRuns = stepRuns

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}

type approvalDecisionRequest struct {
	// Comment is an optional note recorded in the gate's audit line.
	Comment string `json:"comment"`
}

// handleApproveRun resumes a run paused on a manual-approval gate.
func handleApproveRun(w http.ResponseWriter, r *http.Request) { approvalDecision(w, r, true) }

// handleRejectRun fails a run paused on a manual-approval gate.
func handleRejectRun(w http.ResponseWriter, r *http.Request) { approvalDecision(w, r, false) }

// approvalDecision is the shared approve/reject path. Both require the same
// approveRun permission (anyone who may approve may also reject) and act only on a
// run currently in awaiting_approval. On approve the run's token is re-minted —
// a long pause may have outlived the original — and the run is re-queued (→
// pending) so a worker resumes it where it paused; on reject the run is failed.
func approvalDecision(w http.ResponseWriter, r *http.Request, approve bool) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "approvalDecision")
	defer span.End()

	id, resource, wfRef := runRefAndResource(r)
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "approveRun", resource)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
		attribute.Bool("approval.approve", approve),
	)

	run, err := getRun(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "approval: get run", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get run", http.StatusInternalServerError)
		return
	}
	if !canAccessRun(run, userID, orgID) || !runMatchesWorkflowRef(ctx, wfRef, run, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if run.Status != StatusAwaitingApproval {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "run is not awaiting approval", http.StatusConflict)
		return
	}

	sr, err := approvalStepRun(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "run is not awaiting approval", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to load approval gate", http.StatusInternalServerError)
		return
	}

	// The workflow supplies the scoped run role (needed to re-mint the token) and
	// the gate step's optional approver allow-list.
	wf, err := getWorkflow(ctx, run.WorkflowID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "cannot decide: workflow no longer exists", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if sr.StepIndex >= 0 && sr.StepIndex < len(wf.Steps) {
		if approvers := withStrings(wf.Steps[sr.StepIndex].With, "approvers"); len(approvers) > 0 && !slices.Contains(approvers, userID) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "you are not an approver for this step", http.StatusForbidden)
			return
		}
	}

	var req approvalDecisionRequest
	if r.ContentLength != 0 {
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
	}
	verb := "approved"
	if !approve {
		verb = "rejected"
	}
	decision := verb + " by " + resolveUsername(ctx, r.Header.Get("Authorization"), userID)
	if c := strings.TrimSpace(req.Comment); c != "" {
		decision += ": " + c
	}

	if !approve {
		if err := rejectAfterApproval(ctx, id, sr.StepRunID, decision); err != nil {
			if errors.Is(err, errRunNotAwaiting) {
				http.Error(w, "run is not awaiting approval", http.StatusConflict)
				return
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "db error")
			slog.ErrorContext(ctx, "approval: reject run", "run_id", id, "user_id", userID, "error", err)
			http.Error(w, "failed to reject run", http.StatusInternalServerError)
			return
		}
		revokeRunToken(context.Background(), run.RunSessionID)
		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "workflow run rejected", "run_id", id, "user_id", userID)
		writeRunWithSteps(ctx, w, id)
		return
	}

	// Re-mint the run token before resuming so a pause longer than the token TTL
	// can't resume on an expired credential. Attribute it to the original
	// triggerer (not the approver) with the workflow's scoped role.
	newToken, newSID, err := createRunToken(ctx, run.TriggeredBy, wf.RoleID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "run token creation failed")
		slog.ErrorContext(ctx, "approval: create run token", "run_id", id, "error", err)
		http.Error(w, "failed to provision run credentials", http.StatusInternalServerError)
		return
	}
	encNew, err := encryptToken(newToken)
	if err != nil {
		revokeRunToken(context.Background(), newSID)
		span.RecordError(err)
		span.SetStatus(codes.Error, "run token encryption failed")
		http.Error(w, "failed to provision run credentials", http.StatusInternalServerError)
		return
	}
	if err := (WorkflowRun{RunID: id}).UpdateToken(ctx, encNew, newSID); err != nil {
		revokeRunToken(context.Background(), newSID)
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		http.Error(w, "failed to provision run credentials", http.StatusInternalServerError)
		return
	}
	if err := resumeAfterApproval(ctx, id, sr.StepRunID, decision); err != nil {
		revokeRunToken(context.Background(), newSID)
		if errors.Is(err, errRunNotAwaiting) {
			http.Error(w, "run is not awaiting approval", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "approval: resume run", "run_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to approve run", http.StatusInternalServerError)
		return
	}
	// The new token is live and the run re-queued; retire the old session.
	revokeRunToken(context.Background(), run.RunSessionID)
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "workflow run approved", "run_id", id, "user_id", userID)
	writeRunWithSteps(ctx, w, id)
}

// writeRunWithSteps responds with the run and its step runs after a decision, so
// the caller immediately sees the new status and the recorded gate outcome.
func writeRunWithSteps(ctx context.Context, w http.ResponseWriter, id string) {
	run, err := getRun(ctx, id)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	run.StepRuns, _ = getStepRuns(ctx, id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run) //nolint:errcheck
}

func handleCancelRun(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleCancelRun")
		defer span.End()

		id, resource, wfRef := runRefAndResource(r)
		userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "cancelRun", resource)
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.AddEvent("permission.granted")
		span.SetAttributes(
			attribute.String("user.id", userID),
			attribute.String("org.id", orgID),
		)

		run, err := getRun(ctx, id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				span.SetStatus(codes.Ok, "")
				http.Error(w, "run not found", http.StatusNotFound)
				return
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "db error")
			slog.ErrorContext(ctx, "cancel run: get run", "run_id", id, "user_id", userID, "error", err)
			http.Error(w, "failed to get run", http.StatusInternalServerError)
			return
		}
		if !canAccessRun(run, userID, orgID) || !runMatchesWorkflowRef(ctx, wfRef, run, userID, orgID) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}

		affected, err := cancelRun(ctx, id)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db update failed")
			slog.ErrorContext(ctx, "cancel run: db update", "run_id", id, "user_id", userID, "error", err)
			http.Error(w, "failed to cancel run", http.StatusInternalServerError)
			return
		}
		if affected == 0 {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "run is not in a cancellable state", http.StatusConflict)
			return
		}
		// Signal the worker goroutine to stop early; the DB write above is the
		// authoritative cancellation. This is best-effort — the worker may have
		// already finished before the signal arrives.
		pool.Cancel(id)
		// A paused (awaiting_approval) run has no live worker to revoke its run
		// token on exit, so do it here; for pending/running runs the worker does.
		if run.Status == StatusAwaitingApproval {
			revokeRunToken(context.Background(), run.RunSessionID)
		}

		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "workflow run cancelled", "run_id", id, "user_id", userID)
		w.WriteHeader(http.StatusNoContent)
	}
}
