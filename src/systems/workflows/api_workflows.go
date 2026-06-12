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
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// collectWorkflowPermissions returns the deduplicated set of gatekeeper
// permissions declared by the workflow's step actions in the current catalog.
func collectWorkflowPermissions(steps []WorkflowStep) []PermissionSpec {
	seen := map[string]struct{}{}
	var out []PermissionSpec
	actionCatalogMu.RLock()
	defer actionCatalogMu.RUnlock()
	for _, ws := range steps {
		if ws.Action == ActionHTTP {
			continue // ActionHTTP permissions are runtime-dynamic; can't enumerate statically
		}
		def, ok := actionCatalog[ws.Action]
		if !ok || def.RequiredPermission == nil {
			continue
		}
		p := def.RequiredPermission
		key := p.Service + ":" + p.Action + ":" + p.Resource
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, PermissionSpec{
			Service:  p.Service,
			Action:   p.Action,
			Resource: p.Resource,
		})
	}
	return out
}

// provisionWorkflowRole asks gatekeeper to create a minimal-permission role for
// workflowID. Returns the new role_id, or "" when the key is unconfigured or the
// permission list is empty (runs will use the user's full session permissions).
func provisionWorkflowRole(ctx context.Context, workflowID, userID, orgID string, steps []WorkflowStep) string {
	perms := collectWorkflowPermissions(steps)
	if len(perms) == 0 {
		return ""
	}
	key := gatekeeperKey()
	if key == "" {
		return ""
	}

	payload, _ := json.Marshal(map[string]any{
		"workflow_id":  workflowID,
		"user_id":      userID,
		"org_id":       orgID,
		"permissions":  perms,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gatekeeperURL+"/internal/workflow-roles", bytes.NewReader(payload))
	if err != nil {
		slog.Warn("provisionWorkflowRole: build request", "workflow_id", workflowID, "error", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Key", "workflows:"+key)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("provisionWorkflowRole: request failed", "workflow_id", workflowID, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		slog.Warn("provisionWorkflowRole: unexpected status", "workflow_id", workflowID,
			"status", resp.StatusCode, "body", strings.TrimSpace(string(raw)))
		return ""
	}
	var result struct {
		RoleID string `json:"role_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		slog.Warn("provisionWorkflowRole: decode response", "workflow_id", workflowID, "error", err)
		return ""
	}
	return result.RoleID
}

// deleteWorkflowRole removes the role that was provisioned at workflow creation.
// Failures are logged but never propagated — a missing cleanup is not fatal.
func deleteWorkflowRole(ctx context.Context, roleID string) {
	if roleID == "" {
		return
	}
	key := gatekeeperKey()
	if key == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		gatekeeperURL+"/internal/workflow-roles/"+roleID, nil)
	if err != nil {
		slog.Warn("deleteWorkflowRole: build request", "role_id", roleID, "error", err)
		return
	}
	req.Header.Set("X-Service-Key", "workflows:"+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("deleteWorkflowRole: request failed", "role_id", roleID, "error", err)
		return
	}
	resp.Body.Close()
}

var validMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// handleListActions returns the current in-memory action catalog loaded from the registry.
func handleListActions(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleListActions")
	defer span.End()

	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listAction", "workflows/actions"); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createWorkflow", "workflows/pipelines")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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
	if len(req.Steps) == 0 {
		http.Error(w, "at least one step is required", http.StatusBadRequest)
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

	steps, err := enrichStepRefs(ctx, refs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "enrich steps failed")
		slog.Error("create workflow: enrich steps", "error", err)
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	}
	wf.Steps = steps

	// Provision a scoped service role before persisting so the role_id is stored atomically.
	wf.RoleID = provisionWorkflowRole(ctx, wf.WorkflowID, userID, orgID, wf.Steps)

	if err := wf.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create workflow: db error", "error", err)
		deleteWorkflowRole(ctx, wf.RoleID)
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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listWorkflow", "workflows/pipelines")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getWorkflow", "workflows/pipelines/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	wf, err := getWorkflow(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
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
		span.SetStatus(codes.Ok, "")
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
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateWorkflow", "workflows/pipelines/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	existing, err := getWorkflow(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("update workflow: fetch error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get workflow", http.StatusInternalServerError)
		return
	}
	if !canAccessWorkflow(existing, userID, orgID) {
		span.SetStatus(codes.Ok, "")
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
	if len(req.Steps) == 0 {
		http.Error(w, "at least one step is required", http.StatusBadRequest)
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
	newSteps, err := enrichStepRefs(ctx, refs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "enrich steps failed")
		slog.Error("update workflow: enrich steps", "workflow_id", id, "error", err)
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	}

	oldRoleID := existing.RoleID
	existing.Name = req.Name
	existing.Description = req.Description
	existing.StepRefs = refs
	existing.Steps = newSteps
	existing.UpdatedAt = time.Now().UTC()

	// Re-provision the role with the updated step set.
	existing.RoleID = provisionWorkflowRole(ctx, id, userID, orgID, newSteps)

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update workflow: db error", "workflow_id", id, "user_id", userID, "error", err)
		deleteWorkflowRole(ctx, existing.RoleID)
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	}
	// Old role is now superseded; clean it up after the DB write succeeds.
	deleteWorkflowRole(ctx, oldRoleID)

	span.SetStatus(codes.Ok, "")
	slog.Info("workflow updated", "workflow_id", id, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing) //nolint:errcheck
}

func handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleDeleteWorkflow")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteWorkflow", "workflows/pipelines/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	wf, err := getWorkflow(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
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
		span.SetStatus(codes.Ok, "")
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	roleID := wf.RoleID
	if err := wf.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete workflow: db error", "workflow_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete workflow", http.StatusInternalServerError)
		return
	}
	deleteWorkflowRole(ctx, roleID)

	span.SetStatus(codes.Ok, "")
	slog.Info("workflow deleted", "workflow_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
