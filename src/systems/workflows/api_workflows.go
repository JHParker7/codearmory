package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// checkGatekeeper calls gatekeeper's /check_permissions endpoint using the
// caller's Bearer token. Returns the user_id, org_id and true when authorised.
// org_id is empty when the user has no org.
func checkGatekeeper(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (userID, orgID string, ok bool) {
	token, hasBearerPrefix := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !hasBearerPrefix || token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}

	body, _ := json.Marshal(map[string]string{
		"service":  "workflows",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		slog.Error("workflows: failed to build gatekeeper request", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("workflows: gatekeeper check_permissions failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		slog.Error("workflows: gatekeeper unavailable", "status", resp.StatusCode)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return "", "", false
	}

	var result struct {
		Authorized bool    `json:"authorized"`
		UserID     string  `json:"user_id"`
		OrgID      *string `json:"org_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Authorized {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", false
	}
	org := ""
	if result.OrgID != nil {
		org = *result.OrgID
	}
	return result.UserID, org, true
}

// canAccessWorkflow returns true when the caller owns the workflow or shares its org.
func canAccessWorkflow(wf Workflow, userID, orgID string) bool {
	return wf.CreatedBy == userID || (orgID != "" && wf.OrgID == orgID)
}

// bearerToken extracts the raw Bearer token from the request without validation.
func bearerToken(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

var validMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

type createWorkflowRequest struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Steps       []WorkflowStep `json:"steps"`
}

// validateSteps normalises and validates the step list, returning an error
// message ready to send as an HTTP 400 body, or "" on success.
func validateSteps(steps []WorkflowStep) ([]WorkflowStep, string) {
	if len(steps) == 0 {
		return nil, "at least one step is required"
	}
	if len(steps) > maxSteps {
		return nil, fmt.Sprintf("maximum %d steps allowed", maxSteps)
	}
	out := make([]WorkflowStep, len(steps))
	copy(out, steps)
	for i, s := range out {
		if s.Service == "" {
			return nil, fmt.Sprintf("step %d: service is required", i)
		}
		if s.Path == "" || !strings.HasPrefix(s.Path, "/") {
			return nil, fmt.Sprintf("step %d: path must be set and start with '/'", i)
		}
		method := strings.ToUpper(s.Method)
		if method == "" {
			method = "POST"
		}
		if !slices.Contains(validMethods, method) {
			return nil, fmt.Sprintf("step %d: method %q is not allowed", i, s.Method)
		}
		out[i].Method = method
		if s.Name == "" {
			out[i].Name = fmt.Sprintf("step-%d", i)
		}
		if s.TimeoutSecs <= 0 {
			out[i].TimeoutSecs = defaultTimeout
		} else if s.TimeoutSecs > maxTimeout {
			out[i].TimeoutSecs = maxTimeout
		}
		if out[i].Headers == nil {
			out[i].Headers = map[string]string{}
		}
	}
	return out, ""
}

func handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleCreateWorkflow")
	defer span.End()

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "createWorkflow", "workflows/workflows")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "invalid body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	steps, errMsg := validateSteps(req.Steps)
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		slog.Error("create workflow: marshal steps", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	wf := Workflow{
		WorkflowID:  uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		CreatedBy:   userID,
		OrgID:       orgID,
		Steps:       steps,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Active:      true,
	}

	_, err = db.Exec(ctx,
		`INSERT INTO workflows (workflow_id, name, description, created_by, org_id, steps)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		wf.WorkflowID, wf.Name, wf.Description, wf.CreatedBy, wf.OrgID, stepsJSON,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create workflow: db error", "error", err)
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("workflow.id", wf.WorkflowID))
	span.SetStatus(codes.Ok, "")
	slog.Info("workflow created", "workflow_id", wf.WorkflowID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(wf) //nolint:errcheck
}

func handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleListWorkflows")
	defer span.End()

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "listWorkflow", "workflows/workflows")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	rows, err := db.Query(ctx,
		`SELECT workflow_id, name, description, created_by, org_id, steps, created_at, updated_at, active
		 FROM workflows
		 WHERE active = true AND (created_by = $1 OR (org_id != '' AND org_id = $2))
		 ORDER BY created_at DESC LIMIT 100`,
		userID, orgID,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list workflows: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list workflows", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	wfs, err := scanWorkflows(rows)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scan failed")
		slog.Error("list workflows: scan error", "user_id", userID, "error", err)
		http.Error(w, "failed to list workflows", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wfs) //nolint:errcheck
}

func handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "getWorkflow", "workflows/workflows/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	wf, err := getWorkflow(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("get workflow: db error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(wf, userID, orgID) {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wf) //nolint:errcheck
}

func handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleUpdateWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "updateWorkflow", "workflows/workflows/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	existing, err := getWorkflow(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		slog.Error("update workflow: fetch error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(existing, userID, orgID) {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	steps, errMsg := validateSteps(req.Steps)
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		slog.Error("update workflow: marshal steps", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(ctx,
		`UPDATE workflows SET name=$1, description=$2, steps=$3, updated_at=now()
		 WHERE workflow_id=$4 AND active=true`,
		req.Name, req.Description, stepsJSON, id,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update workflow: db error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	}

	wf, err := getWorkflow(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db fetch after update failed")
		slog.Error("update workflow: fetch after update", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get updated workflow", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.Info("workflow updated", "workflow_id", id, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wf) //nolint:errcheck
}

func handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleDeleteWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "deleteWorkflow", "workflows/workflows/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	wf, err := getWorkflow(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete workflow: fetch error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(wf, userID, orgID) {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	if _, err = db.Exec(ctx,
		`UPDATE workflows SET active=false, updated_at=now() WHERE workflow_id=$1 AND active=true`, id,
	); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete workflow: db error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete workflow", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("workflow deleted", "workflow_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// getWorkflow fetches a single active workflow by ID.
func getWorkflow(ctx context.Context, id string) (Workflow, error) {
	row := db.QueryRow(ctx,
		`SELECT workflow_id, name, description, created_by, org_id, steps, created_at, updated_at, active
		 FROM workflows WHERE workflow_id=$1 AND active=true`, id,
	)
	return scanWorkflow(row)
}

func scanWorkflow(row pgx.Row) (Workflow, error) {
	var wf Workflow
	var stepsJSON []byte
	err := row.Scan(&wf.WorkflowID, &wf.Name, &wf.Description, &wf.CreatedBy,
		&wf.OrgID, &stepsJSON, &wf.CreatedAt, &wf.UpdatedAt, &wf.Active)
	if err != nil {
		return wf, err
	}
	if err := json.Unmarshal(stepsJSON, &wf.Steps); err != nil {
		return wf, err
	}
	return wf, nil
}

func scanWorkflows(rows pgx.Rows) ([]Workflow, error) {
	var wfs []Workflow
	for rows.Next() {
		var wf Workflow
		var stepsJSON []byte
		if err := rows.Scan(&wf.WorkflowID, &wf.Name, &wf.Description, &wf.CreatedBy,
			&wf.OrgID, &stepsJSON, &wf.CreatedAt, &wf.UpdatedAt, &wf.Active); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(stepsJSON, &wf.Steps); err != nil {
			return nil, err
		}
		wfs = append(wfs, wf)
	}
	if wfs == nil {
		wfs = []Workflow{}
	}
	return wfs, rows.Err()
}
