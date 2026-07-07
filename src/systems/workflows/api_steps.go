package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	if msg := validateResourceName(req.Name); msg != "" {
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

	s, err := resolveStepRef(ctx, id, userID, orgID)
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

	existing, err := resolveStepRef(ctx, id, userID, orgID)
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
	// Validate the name charset only on an actual rename, so editing a step whose
	// name predates this rule (e.g. legacy spaces) isn't blocked unless it's changed.
	if req.Name != existing.Name {
		if msg := validateResourceName(req.Name); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	}
	// A rename must not collide with another of the caller's steps (the name is a
	// resource identifier); keeping its own name is allowed (excludes existing.StepID).
	if conflict, cerr := stepNameConflict(ctx, req.Name, existing.StepID, userID, orgID); cerr != nil {
		span.RecordError(cerr)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to update step", http.StatusInternalServerError)
		return
	} else if conflict {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "a step with that name already exists", http.StatusConflict)
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

	s, err := getStep(ctx, existing.StepID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db fetch after update failed")
		http.Error(w, "failed to get updated step", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "step updated", "step_id", existing.StepID, "user_id", userID)
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

	s, err := resolveStepRef(ctx, id, userID, orgID)
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

// validateStepRefShape checks a single step reference's structural invariants
// without touching the database. A ref is exactly one of: an inline approval gate,
// an inline step (Action set, definition on the ref), or a reference to a stored
// step (StepID set) — each optionally with a parallel group or matrix. Returns a
// user-facing message or "".
func validateStepRefShape(i int, ref WorkflowStepRef) string {
	// A per-occurrence name (if given) becomes a ${steps.<name>.output} key, so it
	// must be a clean single-segment name like a step name.
	if ref.Name != "" {
		if msg := validateResourceName(ref.Name); msg != "" {
			return fmt.Sprintf("step %d: name %s", i, msg)
		}
	}
	if ref.Approval != nil {
		if ref.StepID != "" || ref.Action != "" {
			return fmt.Sprintf("step %d: cannot be both a step and an approval gate", i)
		}
		if ref.ParallelGroup != nil || ref.Matrix != nil {
			return fmt.Sprintf("step %d: an approval gate cannot have a parallel_group or matrix", i)
		}
		return ""
	}
	// An inline step carries its whole definition on the ref (no StepID). Validate it
	// the same way createStep validates a stored step, so both authoring paths accept
	// the same actions; the parallel/matrix checks below then apply to it too.
	if ref.Action != "" {
		if ref.StepID != "" {
			return fmt.Sprintf("step %d: cannot be both a stored-step reference and an inline step", i)
		}
		if ref.Name == "" {
			return fmt.Sprintf("step %d: an inline step requires a name", i)
		}
		if ref.Action == ActionApproval {
			return fmt.Sprintf("step %d: use an approval gate rather than an inline %q step", i, ActionApproval)
		}
		if ref.Action == ActionHTTP {
			if withString(ref.With, "service") == "" {
				return fmt.Sprintf("step %d: http requires with.service", i)
			}
			if withString(ref.With, "path") == "" {
				return fmt.Sprintf("step %d: http requires with.path", i)
			}
		}
		if ref.Timeout < 0 || ref.Timeout > maxTimeout {
			return fmt.Sprintf("step %d: timeout must be between 0 and %d seconds", i, maxTimeout)
		}
	} else if ref.StepID == "" {
		return fmt.Sprintf("step %d: step_id or action is required", i)
	}
	if ref.ParallelGroup != nil && *ref.ParallelGroup < 0 {
		return fmt.Sprintf("step %d: parallel_group must be non-negative", i)
	}
	if ref.Matrix != nil {
		if ref.ParallelGroup != nil {
			return fmt.Sprintf("step %d: matrix and parallel_group are mutually exclusive", i)
		}
		if msg := validateMatrix(ref.Matrix); msg != "" {
			return fmt.Sprintf("step %d: %s", i, msg)
		}
	}
	return ""
}

// validateMatrix checks a matrix config: a var name usable as a ${matrix.<var>}
// key, and exactly one source of values (a literal list or a values_from ref).
func validateMatrix(m *MatrixConfig) string {
	if m.Var == "" {
		return "matrix.var is required"
	}
	if strings.ContainsAny(m.Var, " \t\n\r${}") {
		return "matrix.var must not contain whitespace or ${} characters"
	}
	hasValues := len(m.Values) > 0
	hasFrom := strings.TrimSpace(m.ValuesFrom) != ""
	if hasValues == hasFrom {
		return "matrix requires exactly one of values or values_from"
	}
	if hasValues && len(m.Values) > maxMatrixValues {
		return fmt.Sprintf("matrix has %d values, exceeding the limit of %d", len(m.Values), maxMatrixValues)
	}
	if m.MaxConcurrent < 0 {
		return "matrix.max_concurrent must not be negative"
	}
	return ""
}

// validateStepRefs confirms every stored-step reference exists and is accessible to
// the caller. Inline steps and approval gates carry no step_id, so they are skipped
// here (their shape is checked in validateStepRefShape); their names still take part
// in the per-pipeline uniqueness check below.
func validateStepRefs(ctx context.Context, refs []WorkflowStepRef, userID, orgID string, w http.ResponseWriter) error {
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Approval == nil && r.StepID != "" {
			ids = append(ids, r.StepID)
		}
	}
	if len(ids) == 0 {
		return nil
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
	// A step's effective name (per-occurrence override, else the step definition's
	// name) is its ${steps.<name>.output} key and its step-run label, so two blocks
	// sharing one name silently collide in the run's output map — the later one
	// shadows the earlier, and any output reference resolves against the wrong entry
	// (or not at all). Require names to be unique across the pipeline so wiring is
	// unambiguous. Approval gates carry no override name here and produce no output,
	// so an empty name never collides.
	seenNames := make(map[string]int, len(refs))
	for i, ref := range refs {
		name := ref.Name
		// Only a stored-step reference needs the existence/access lookup. An inline step
		// (Approval nil, StepID empty) carries its own name via ref.Name — validated
		// non-empty in validateStepRefShape — and a gate has no name here.
		if ref.Approval == nil && ref.StepID != "" {
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
			// A manual-approval gate is a single pause point, not a fan-out, so a matrix
			// over it is meaningless — reject it rather than spawn N parallel gates.
			if ref.Matrix != nil && s.Action == ActionApproval {
				err := fmt.Errorf("step %d: an approval step cannot use a matrix", i)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return err
			}
			if name == "" {
				name = s.Name
			}
		}
		if name != "" {
			if first, dup := seenNames[name]; dup {
				err := fmt.Errorf("step %d: duplicate step name %q (already used by step %d) — each step in a workflow must have a unique name; give this block a per-occurrence name", i, name, first)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return err
			}
			seenNames[name] = i
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

// substituteWith / substituteValue live in substitution.go alongside the step
// inputs/outputs templating engine.
