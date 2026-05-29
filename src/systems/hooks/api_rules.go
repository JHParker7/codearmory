package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
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
		"service":  "hooks",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		slog.Error("hooks: failed to build gatekeeper request", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("hooks: gatekeeper check_permissions failed", "error", err)
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
		slog.Error("hooks: gatekeeper unavailable", "status", resp.StatusCode)
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
	Secret       string            `json:"secret"`
	InputMapping map[string]string `json:"input_mapping"`
}

func handleCreateRule(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("hooks").Start(r.Context(), "handleCreateRule")
	defer span.End()

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "createRule", "hooks/rules")
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

	mappingJSON, err := json.Marshal(req.InputMapping)
	if err != nil {
		slog.Error("create rule: marshal input_mapping", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	rule := PipelineRule{
		RuleID:       uuid.New().String(),
		Name:         req.Name,
		Repo:         req.Repo,
		Events:       req.Events,
		RefFilter:    req.RefFilter,
		WorkflowID:   req.WorkflowID,
		InputMapping: req.InputMapping,
		CreatedBy:    userID,
		OrgID:        orgID,
		Active:       true,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	_, err = db.Exec(ctx,
		`INSERT INTO pipeline_rules
		     (rule_id, name, repo, events, ref_filter, workflow_id, secret, input_mapping, created_by, org_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rule.RuleID, rule.Name, rule.Repo, rule.Events, rule.RefFilter,
		rule.WorkflowID, nullableString(req.Secret), mappingJSON, rule.CreatedBy, rule.OrgID,
	)
	if err != nil {
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

	userID, orgID, ok := checkGatekeeper(ctx, w, r, "listRule", "hooks/rules")
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	rows, err := db.Query(ctx,
		`SELECT rule_id, name, repo, events, ref_filter, workflow_id, input_mapping, created_by, org_id, active, created_at, updated_at
		 FROM pipeline_rules
		 WHERE active = true AND (created_by = $1 OR (org_id != '' AND org_id = $2))
		 ORDER BY created_at DESC LIMIT 100`,
		userID, orgID,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list rules: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list rules", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	rules, err := scanRules(rows)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "scan failed")
		slog.Error("list rules: scan error", "user_id", userID, "error", err)
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "getRule", "hooks/rules/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	rule, err := getRule(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "updateRule", "hooks/rules/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	existing, err := getRule(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "rule not found", http.StatusNotFound)
			return
		}
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

	mappingJSON, err := json.Marshal(req.InputMapping)
	if err != nil {
		slog.Error("update rule: marshal input_mapping", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(ctx,
		`UPDATE pipeline_rules
		 SET name=$1, repo=$2, events=$3, ref_filter=$4, workflow_id=$5,
		     input_mapping=$6, secret=COALESCE(NULLIF($7, ''), secret), updated_at=now()
		 WHERE rule_id=$8 AND active=true`,
		req.Name, req.Repo, req.Events, req.RefFilter, req.WorkflowID,
		mappingJSON, req.Secret, id,
	)
	if err != nil {
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
	userID, orgID, ok := checkGatekeeper(ctx, w, r, "deleteRule", "hooks/rules/"+id)
	if !ok {
		span.SetStatus(codes.Error, "forbidden")
		return
	}

	rule, err := getRule(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
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

	if _, err = db.Exec(ctx,
		`UPDATE pipeline_rules SET active=false, updated_at=now() WHERE rule_id=$1 AND active=true`, id,
	); err != nil {
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

// getRule fetches a single active pipeline rule by ID (without secret).
func getRule(ctx context.Context, id string) (PipelineRule, error) {
	row := db.QueryRow(ctx,
		`SELECT rule_id, name, repo, events, ref_filter, workflow_id, input_mapping, created_by, org_id, active, created_at, updated_at
		 FROM pipeline_rules WHERE rule_id=$1 AND active=true`, id,
	)
	return scanRule(row)
}

func scanRule(row pgx.Row) (PipelineRule, error) {
	var rule PipelineRule
	var mappingJSON []byte
	var refFilter *string
	err := row.Scan(&rule.RuleID, &rule.Name, &rule.Repo, &rule.Events, &refFilter,
		&rule.WorkflowID, &mappingJSON, &rule.CreatedBy, &rule.OrgID, &rule.Active,
		&rule.CreatedAt, &rule.UpdatedAt)
	if err != nil {
		return rule, err
	}
	if refFilter != nil {
		rule.RefFilter = *refFilter
	}
	if err := json.Unmarshal(mappingJSON, &rule.InputMapping); err != nil {
		return rule, err
	}
	return rule, nil
}

func scanRules(rows pgx.Rows) ([]PipelineRule, error) {
	var rules []PipelineRule
	for rows.Next() {
		var rule PipelineRule
		var mappingJSON []byte
		var refFilter *string
		if err := rows.Scan(&rule.RuleID, &rule.Name, &rule.Repo, &rule.Events, &refFilter,
			&rule.WorkflowID, &mappingJSON, &rule.CreatedBy, &rule.OrgID, &rule.Active,
			&rule.CreatedAt, &rule.UpdatedAt); err != nil {
			return nil, err
		}
		if refFilter != nil {
			rule.RefFilter = *refFilter
		}
		if err := json.Unmarshal(mappingJSON, &rule.InputMapping); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	if rules == nil {
		rules = []PipelineRule{}
	}
	return rules, rows.Err()
}

// nullableString returns nil for an empty string so the DB column stores NULL.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
