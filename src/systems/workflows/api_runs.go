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
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "triggerRun", "workflows/runs")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	wf, err := getWorkflow(ctx, workflowID)
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

func handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetRun")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRun", "workflows/runs/"+id)
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
	if !canAccessRun(run, userID, orgID) {
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

func handleCancelRun(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleCancelRun")
		defer span.End()

		id := r.PathValue("id")
		userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "cancelRun", "workflows/runs/"+id)
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
		if !canAccessRun(run, userID, orgID) {
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

		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "workflow run cancelled", "run_id", id, "user_id", userID)
		w.WriteHeader(http.StatusNoContent)
	}
}
