package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createWorkflow", "workflows/workflows")
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

	if err := db.WithContext(ctx).Create(&wf).Error; err != nil {
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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listWorkflow", "workflows/workflows")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	var wfs []Workflow
	if err := db.WithContext(ctx).
		Where("active=? AND (created_by=? OR (org_id!='' AND org_id=?))", true, userID, orgID).
		Order("created_at desc").
		Limit(100).
		Find(&wfs).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list workflows: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list workflows", http.StatusInternalServerError)
		return
	}
	if wfs == nil {
		wfs = []Workflow{}
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wfs) //nolint:errcheck
}

func handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getWorkflow", "workflows/workflows/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	wf, err := getWorkflow(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
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
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateWorkflow", "workflows/workflows/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	existing, err := getWorkflow(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
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

	if err := db.WithContext(ctx).Model(&Workflow{}).
		Where("workflow_id=? AND active=?", id, true).
		Updates(map[string]any{
			"name":        req.Name,
			"description": req.Description,
			"steps":       string(stepsJSON),
			"updated_at":  time.Now().UTC(),
		}).Error; err != nil {
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
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteWorkflow", "workflows/workflows/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	wf, err := getWorkflow(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
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

	result := db.WithContext(ctx).Model(&Workflow{}).
		Where("workflow_id=? AND active=?", id, true).
		Updates(map[string]any{"active": false, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete workflow: db error", "workflow_id", id, "user_id", userID, "error", result.Error)
		http.Error(w, "failed to delete workflow", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("workflow deleted", "workflow_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// getWorkflow fetches a single active workflow by ID.
func getWorkflow(ctx context.Context, id string) (Workflow, error) {
	var wf Workflow
	result := db.WithContext(ctx).Where("workflow_id=? AND active=?", id, true).First(&wf)
	return wf, result.Error
}
