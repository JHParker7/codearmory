package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

var validMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// handleListActions returns the current in-memory action catalog loaded from the registry.
func handleListActions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := gatekeeperClient.CheckPermissions(r.Context(), w, r, "listAction", "workflows/actions"); !ok {
		return
	}

	actionCatalogMu.RLock()
	catalog := make([]ActionDef, 0, len(actionCatalog))
	for _, def := range actionCatalog {
		catalog = append(catalog, def)
	}
	actionCatalogMu.RUnlock()

	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Name < catalog[j].Name })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(catalog) //nolint:errcheck
}

func canAccessWorkflow(wf Workflow, userID, orgID string) bool {
	return wf.CreatedBy == userID || (orgID != "" && wf.OrgID == orgID)
}

func bearerToken(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

type createWorkflowRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Steps       []WorkflowStepRef `json:"steps,omitempty"`
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
	if len(req.Steps) > maxSteps {
		http.Error(w, fmt.Sprintf("maximum %d steps allowed", maxSteps), http.StatusBadRequest)
		return
	}
	for i, ref := range req.Steps {
		if ref.StepID == "" {
			http.Error(w, fmt.Sprintf("step %d: step_id is required", i), http.StatusBadRequest)
			return
		}
		if ref.ParallelGroup != nil && *ref.ParallelGroup < 0 {
			http.Error(w, fmt.Sprintf("step %d: parallel_group must be non-negative", i), http.StatusBadRequest)
			return
		}
	}

	// Validate all referenced steps exist and are accessible.
	if err := validateStepRefs(ctx, req.Steps, userID, orgID, w); err != nil {
		return // response already written
	}

	refs := req.Steps
	if refs == nil {
		refs = []WorkflowStepRef{}
	}
	wf := Workflow{
		WorkflowID:  uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		CreatedBy:   userID,
		OrgID:       orgID,
		Active:      true,
		StepRefs:    refs,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	if err := wf.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create workflow: db error", "error", err)
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	}

	steps, err := enrichStepRefs(ctx, wf.StepRefs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "enrich steps failed")
		slog.Error("create workflow: enrich steps", "error", err)
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	}
	wf.Steps = steps

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

	wfs, err := listWorkflows(ctx, userID, orgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list workflows: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list workflows", http.StatusInternalServerError)
		return
	}
	for i := range wfs {
		wfs[i].Steps = []WorkflowStep{}
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
	if len(req.Steps) > maxSteps {
		http.Error(w, fmt.Sprintf("maximum %d steps allowed", maxSteps), http.StatusBadRequest)
		return
	}
	for i, ref := range req.Steps {
		if ref.StepID == "" {
			http.Error(w, fmt.Sprintf("step %d: step_id is required", i), http.StatusBadRequest)
			return
		}
		if ref.ParallelGroup != nil && *ref.ParallelGroup < 0 {
			http.Error(w, fmt.Sprintf("step %d: parallel_group must be non-negative", i), http.StatusBadRequest)
			return
		}
	}
	if err := validateStepRefs(ctx, req.Steps, userID, orgID, w); err != nil {
		return
	}

	refs := req.Steps
	if refs == nil {
		refs = []WorkflowStepRef{}
	}
	existing.Name = req.Name
	existing.Description = req.Description
	existing.StepRefs = refs

	if err := existing.Update(ctx); err != nil {
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

	if err := wf.Remove(ctx); err != nil {
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
