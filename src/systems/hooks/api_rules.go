package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// fetchWorkflowOrgID calls the workflows internal API to verify the org that
// owns a workflow. Returns the org ID string or an error if the workflow is not
// found or the request fails.
func fetchWorkflowOrgID(ctx context.Context, workflowID string) (string, error) {
	if hooksTriggerKey == "" {
		return "", fmt.Errorf("HOOKS_TRIGGER_KEY not configured")
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(hooksTriggerKey))
	fmt.Fprintf(mac, "hooks-check:%s:%s", workflowID, ts)
	token := hex.EncodeToString(mac.Sum(nil))

	url := workflowsURL + "/internal/workflows/" + workflowID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Hooks-Token", token)
	req.Header.Set("X-Hooks-Timestamp", ts)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("workflow not found")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var result struct {
		OrgID string `json:"org_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.OrgID, nil
}

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
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	Events   []string `json:"events"`
	RefFilter    string            `json:"ref_filter"`
	WorkflowID   string            `json:"workflow_id"`
	// Secret is write-only (never returned in responses). Required on create; cannot be cleared on update.
	// On create: must be a non-empty string — all rules require an HMAC secret.
	// On update: omit the field (JSON null) to leave the existing secret unchanged;
	// send a non-empty string to replace it.
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
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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
	if req.Secret == nil || *req.Secret == "" {
		http.Error(w, "secret is required — webhook rules must have an HMAC secret to prevent unauthenticated triggering", http.StatusBadRequest)
		return
	}
	if wfOrgID, err := fetchWorkflowOrgID(ctx, req.WorkflowID); err != nil {
		slog.Warn("create rule: workflow not found or unreachable", "workflow_id", req.WorkflowID, "error", err)
		http.Error(w, "workflow_id not found", http.StatusUnprocessableEntity)
		return
	} else if wfOrgID != orgID {
		slog.Warn("create rule: cross-org workflow reference", "user_id", userID, "workflow_id", req.WorkflowID, "workflow_org", wfOrgID, "caller_org", orgID)
		http.Error(w, "workflow_id not found", http.StatusUnprocessableEntity)
		return
	}
	if req.InputMapping == nil {
		req.InputMapping = map[string]string{}
	}

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
	}

	if err := rule.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create rule: db error", "error", err)
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
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

	rules, err := listRules(ctx, userID, orgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list rules: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list rules", http.StatusInternalServerError)
		return
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
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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
	if req.Secret != nil && *req.Secret == "" {
		http.Error(w, "secret cannot be cleared — webhook rules must retain an HMAC secret", http.StatusBadRequest)
		return
	}
	if wfOrgID, err := fetchWorkflowOrgID(ctx, req.WorkflowID); err != nil {
		slog.Warn("update rule: workflow not found or unreachable", "rule_id", id, "workflow_id", req.WorkflowID, "error", err)
		http.Error(w, "workflow_id not found", http.StatusUnprocessableEntity)
		return
	} else if wfOrgID != orgID {
		slog.Warn("update rule: cross-org workflow reference", "user_id", userID, "rule_id", id, "workflow_id", req.WorkflowID, "workflow_org", wfOrgID, "caller_org", orgID)
		http.Error(w, "workflow_id not found", http.StatusUnprocessableEntity)
		return
	}
	if req.InputMapping == nil {
		req.InputMapping = map[string]string{}
	}

	// secret update semantics: nil = leave unchanged, non-empty = replace. Clearing ("") is rejected above.
	existing.Name = req.Name
	existing.Repo = req.Repo
	existing.Events = req.Events
	existing.RefFilter = req.RefFilter
	existing.WorkflowID = req.WorkflowID
	existing.InputMapping = req.InputMapping
	if req.Secret != nil {
		existing.Secret = req.Secret
	}

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update rule: db error", "rule_id", id, "user_id", userID, "error", err)
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
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("org.id", orgID),
	)

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

	if err := rule.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("delete rule: db error", "rule_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete rule", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("pipeline rule deleted", "rule_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
