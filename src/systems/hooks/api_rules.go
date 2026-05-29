package main

import (
	"context"
	"encoding/json"
	"errors"
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

// canAccessRule returns true when the caller owns the rule or shares its org.
func canAccessRule(rule PipelineRule, userID, orgID string) bool {
	return rule.CreatedBy == userID || (orgID != "" && rule.OrgID == orgID)
}

// matchesRefFilter returns true when the event ref matches the rule's ref filter.
// An empty filter matches everything. A filter ending in "/*" is a prefix match
// (after stripping the trailing "*"). Otherwise an exact match is required.
func matchesRefFilter(filter, ref string) bool {
	if filter == "" {
		return true
	}
	if strings.HasSuffix(filter, "/*") {
		return strings.HasPrefix(ref, filter[:len(filter)-1])
	}
	return ref == filter
}

type createRuleRequest struct {
	Name         string            `json:"name"`
	Repo         string            `json:"repo"`
	Events       []string          `json:"events"`
	RefFilter    string            `json:"ref_filter"`
	WorkflowID   string            `json:"workflow_id"`
	// Secret is write-only (never returned in responses).
	// On create: omit or set to "" for no secret; set a non-empty string to require HMAC.
	// On update: omit the field (JSON null) to leave the existing secret unchanged;
	// send "" to clear it; send a non-empty string to replace it.
	Secret       *string           `json:"secret"`
	InputMapping map[string]string `json:"input_mapping"`
}

func handleCreateRule(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleCreateRule")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createRule", "hooks/rules")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	var req createRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.SetStatus(codes.Error, "invalid body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	if len(req.Events) == 0 {
		http.Error(w, "events must be non-empty", http.StatusBadRequest)
		return
	}
	if req.WorkflowID == "" {
		http.Error(w, "workflow_id is required", http.StatusBadRequest)
		return
	}
	if req.InputMapping == nil {
		req.InputMapping = map[string]string{}
	}

	now := time.Now().UTC()
	rule := PipelineRule{
		RuleID:       uuid.New().String(),
		Name:         req.Name,
		Repo:         req.Repo,
		Events:       req.Events,
		RefFilter:    req.RefFilter,
		WorkflowID:   req.WorkflowID,
		Secret:       req.Secret,
		InputMapping: req.InputMapping,
		CreatedBy:    userID,
		OrgID:        orgID,
		Active:       true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if result := db.WithContext(ctx).Create(&rule); result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create rule: db error", "error", result.Error)
		http.Error(w, "failed to create rule", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("rule.id", rule.RuleID))
	span.SetStatus(codes.Ok, "")
	slog.Info("pipeline rule created", "rule_id", rule.RuleID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rule) //nolint:errcheck
}

func handleListRules(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleListRules")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listRule", "hooks/rules")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	var rules []PipelineRule
	result := db.WithContext(ctx).
		Where("active=? AND (created_by=? OR (org_id!='' AND org_id=?))", true, userID, orgID).
		Order("created_at DESC").
		Limit(100).
		Find(&rules)
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list rules: db error", "user_id", userID, "error", result.Error)
		http.Error(w, "failed to list rules", http.StatusInternalServerError)
		return
	}
	if rules == nil {
		rules = []PipelineRule{}
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rules) //nolint:errcheck
}

func handleGetRule(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleGetRule")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getRule", "hooks/rules/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	rule, err := getRule(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "rule not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("get rule: db error", "rule_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get rule", http.StatusInternalServerError)
		return
	}
	if !canAccessRule(rule, userID, orgID) {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rule) //nolint:errcheck
}

func handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleUpdateRule")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateRule", "hooks/rules/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	existing, err := getRule(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "rule not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("update rule: fetch error", "rule_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get rule", http.StatusInternalServerError)
		return
	}
	if !canAccessRule(existing, userID, orgID) {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}

	var req createRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.Repo == "" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	if len(req.Events) == 0 {
		http.Error(w, "events must be non-empty", http.StatusBadRequest)
		return
	}
	if req.WorkflowID == "" {
		http.Error(w, "workflow_id is required", http.StatusBadRequest)
		return
	}
	if req.InputMapping == nil {
		req.InputMapping = map[string]string{}
	}

	// secret update semantics: nil = leave unchanged, "" = clear, non-empty = replace.
	// Apply the new field values to the existing record.
	existing.Name = req.Name
	existing.Repo = req.Repo
	existing.Events = req.Events
	existing.RefFilter = req.RefFilter
	existing.WorkflowID = req.WorkflowID
	existing.InputMapping = req.InputMapping
	existing.UpdatedAt = time.Now().UTC()

	if req.Secret != nil {
		existing.Secret = req.Secret
	}

	if result := db.WithContext(ctx).Save(&existing); result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update rule: db error", "rule_id", id, "user_id", userID, "error", result.Error)
		http.Error(w, "failed to update rule", http.StatusInternalServerError)
		return
	}

	rule, err := getRule(ctx, id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db fetch after update failed")
		slog.Error("update rule: fetch after update", "rule_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get updated rule", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	slog.Info("pipeline rule updated", "rule_id", id, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rule) //nolint:errcheck
}

func handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleDeleteRule")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteRule", "hooks/rules/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	rule, err := getRule(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "rule not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete rule: fetch error", "rule_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete rule", http.StatusInternalServerError)
		return
	}
	if !canAccessRule(rule, userID, orgID) {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}

	result := db.WithContext(ctx).Model(&PipelineRule{}).
		Where("rule_id=? AND active=?", id, true).
		Updates(map[string]any{"active": false, "updated_at": time.Now()})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete rule: db error", "rule_id", id, "user_id", userID, "error", result.Error)
		http.Error(w, "failed to delete rule", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("pipeline rule deleted", "rule_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// getRule fetches a single active pipeline rule by ID (without secret).
func getRule(ctx context.Context, id string) (PipelineRule, error) {
	var rule PipelineRule
	result := db.WithContext(ctx).Where("rule_id=? AND active=?", id, true).First(&rule)
	return rule, result.Error
}

// nullableString returns nil for an empty string so the DB column stores NULL.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
