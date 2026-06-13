package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

func canAccessStep(s Step, userID, orgID string) bool {
	return s.CreatedBy == userID || (orgID != "" && s.OrgID == orgID)
}

type createStepRequest struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Action      string         `json:"action"`
	With        map[string]any `json:"with"`
	Timeout     int64          `json:"timeout"`
}

// validateStepRequest checks that required fields are present.
// Catalog-registered actions (e.g. forge/run, tickets/create) are accepted as-is;
// their With fields are validated at execution time once the catalog is consulted.
// Only the http escape-hatch action is validated here since it has fixed required fields.
func validateStepRequest(req createStepRequest) string {
	if req.Name == "" {
		return "name is required"
	}
	if req.Action == "" {
		return "action is required"
	}
	if req.Action == ActionHTTP {
		if withString(req.With, "service") == "" {
			return "http requires with.service"
		}
		if withString(req.With, "path") == "" {
			return "http requires with.path"
		}
	}
	if req.Timeout < 0 || req.Timeout > maxTimeout {
		return fmt.Sprintf("timeout must be between 0 and %d seconds", maxTimeout)
	}
	return ""
}

func handleCreateStep(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleCreateStep")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createStep", "workflows/steps")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	var req createStepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if msg := validateStepRequest(req); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	// Name must be unique per user/org.
	exists, err := stepNameExists(ctx, req.Name, userID, orgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "create step: name check error", "error", err)
		http.Error(w, "failed to create step", http.StatusInternalServerError)
		return
	}
	if exists {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "a step with that name already exists", http.StatusConflict)
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	s := Step{
		StepID:      uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		Action:      req.Action,
		With:        req.With,
		Timeout:     timeout,
		CreatedBy:   userID,
		OrgID:       orgID,
		Active:      true,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	if err := s.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create step: db error", "error", err)
		http.Error(w, "failed to create step", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("step.id", s.StepID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "step created", "step_id", s.StepID, "name", s.Name, "action", s.Action, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(s) //nolint:errcheck
}

func handleListSteps(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleListSteps")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listStep", "workflows/steps")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	steps, err := listSteps(ctx, userID, orgID, r.URL.Query().Get("name"))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list steps: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list steps", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(steps) //nolint:errcheck
}

func handleGetStep(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleGetStep")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getStep", "workflows/steps/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	s, err := getStep(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "step not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to get step", http.StatusInternalServerError)
		return
	}
	if !canAccessStep(s, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "step not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s) //nolint:errcheck
}

func handleUpdateStep(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleUpdateStep")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateStep", "workflows/steps/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	existing, err := getStep(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "step not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "update step: fetch error", "step_id", id, "error", err)
		http.Error(w, "failed to get step", http.StatusInternalServerError)
		return
	}
	if !canAccessStep(existing, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "step not found", http.StatusNotFound)
		return
	}

	var req createStepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if msg := validateStepRequest(req); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	existing.Name = req.Name
	existing.Description = req.Description
	existing.Action = req.Action
	existing.With = req.With
	existing.Timeout = timeout

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update step: db error", "step_id", id, "error", err)
		http.Error(w, "failed to update step", http.StatusInternalServerError)
		return
	}

	s, err := getStep(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db fetch after update failed")
		http.Error(w, "failed to get updated step", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "step updated", "step_id", id, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s) //nolint:errcheck
}

func handleDeleteStep(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("workflows").Start(r.Context(), "handleDeleteStep")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteStep", "workflows/steps/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	s, err := getStep(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "step not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to get step", http.StatusInternalServerError)
		return
	}
	if !canAccessStep(s, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "step not found", http.StatusNotFound)
		return
	}

	if err := s.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete step: db error", "step_id", id, "error", err)
		http.Error(w, "failed to delete step", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "step deleted", "step_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// validateStepRefs confirms every referenced step exists and is accessible to the caller.
func validateStepRefs(ctx context.Context, refs []WorkflowStepRef, userID, orgID string, w http.ResponseWriter) error {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.StepID
	}
	steps, err := getStepsByIDs(ctx, ids)
	if err != nil {
		http.Error(w, "failed to validate steps", http.StatusInternalServerError)
		return err
	}
	found := make(map[string]Step, len(steps))
	for _, s := range steps {
		found[s.StepID] = s
	}
	for i, ref := range refs {
		s, ok := found[ref.StepID]
		if !ok {
			err := fmt.Errorf("step %d: step %q not found", i, ref.StepID)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return err
		}
		if !canAccessStep(s, userID, orgID) {
			err := fmt.Errorf("step %d: step %q not found", i, ref.StepID)
			http.Error(w, err.Error(), http.StatusNotFound)
			return err
		}
	}
	return nil
}

// ── With field helpers ────────────────────────────────────────────────────────

// withString extracts a string value from a With map.
func withString(with map[string]any, key string) string {
	if with == nil {
		return ""
	}
	v, ok := with[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// withStrings extracts a []string value from a With map.
// Accepts either a []string or a []any of strings (as JSON unmarshals arrays).
func withStrings(with map[string]any, key string) []string {
	if with == nil {
		return nil
	}
	v, ok := with[key]
	if !ok {
		return nil
	}
	switch sv := v.(type) {
	case []string:
		return sv
	case []any:
		result := make([]string, 0, len(sv))
		for _, item := range sv {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

// withStringMap extracts a map[string]string from a With map.
func withStringMap(with map[string]any, key string) map[string]string {
	if with == nil {
		return nil
	}
	v, ok := with[key]
	if !ok {
		return nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			result[k] = s
		}
	}
	return result
}

// withInt64 extracts an int64 from a With map (JSON numbers come back as float64).
func withInt64(with map[string]any, key string) int64 {
	if with == nil {
		return 0
	}
	v, ok := with[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// substituteWith applies ${KEY} substitution to all string values in a With map,
// recursing into nested maps and slices.
func substituteWith(with map[string]any, inputs map[string]string) map[string]any {
	if len(inputs) == 0 {
		return with
	}
	result := make(map[string]any, len(with))
	for k, v := range with {
		result[k] = substituteValue(v, inputs)
	}
	return result
}

func substituteValue(v any, inputs map[string]string) any {
	switch sv := v.(type) {
	case string:
		return substitute(sv, inputs)
	case map[string]any:
		return substituteWith(sv, inputs)
	case []any:
		result := make([]any, len(sv))
		for i, item := range sv {
			result[i] = substituteValue(item, inputs)
		}
		return result
	default:
		return v
	}
}
