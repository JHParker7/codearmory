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
	"regexp"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"
)

// envKeyRe matches POSIX-compliant environment variable names.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// blockedEnvKeys is an explicit denylist of names that could redirect interpreter
// execution or dynamic linker behaviour in user containers.
var blockedEnvKeys = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
	"PYTHONSTARTUP": true, "PYTHONPATH": true,
	"NODE_OPTIONS": true, "NODE_PATH": true,
	"RUBYOPT": true, "RUBYLIB": true,
	"PERL5LIB": true, "PERLLIB": true,
	"JAVA_TOOL_OPTIONS": true, "JAVA_OPTIONS": true, "_JAVA_OPTIONS": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true,
}

func validateEnvKeys(env map[string]string) error {
	for k := range env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid env key %q: must match [A-Za-z_][A-Za-z0-9_]*", k)
		}
		if blockedEnvKeys[strings.ToUpper(k)] {
			return fmt.Errorf("env key %q is not permitted", k)
		}
	}
	return nil
}

// allowedImages is nil when ALLOWED_IMAGES is not configured → deny all submissions.
var allowedImages map[string]bool

func initAllowedImages(raw string) {
	if raw == "" {
		allowedImages = nil // deny-all when not configured
		slog.Warn("ALLOWED_IMAGES is not set — all image submissions will be rejected; set ALLOWED_IMAGES to a comma-separated list of permitted images")
		return
	}
	allowedImages = make(map[string]bool)
	for _, img := range splitTrim(raw) {
		allowedImages[img] = true
	}
}

func splitTrim(s string) []string {
	parts := make([]string, 0)
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			parts = append(parts, t)
		}
	}
	return parts
}

// checkGatekeeper calls gatekeeper's /check_permissions endpoint with the Bearer
// token from the incoming request. It returns the user_id and true when the
// caller is authorised; it writes an HTTP error and returns false otherwise.
// Forge calls gatekeeper directly so that auth is enforced even if a compromised
// conductor strips or forges the X-User-ID header.
func checkGatekeeper(ctx context.Context, w http.ResponseWriter, r *http.Request, action, resource string) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}

	body, _ := json.Marshal(map[string]string{
		"service":  "forge",
		"resource": resource,
		"action":   action,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatekeeperURL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		slog.Error("forge: failed to build gatekeeper request", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		slog.Error("forge: gatekeeper check_permissions failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		slog.Error("forge: gatekeeper unavailable", "status", resp.StatusCode)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return "", false
	}

	var result struct {
		Authorized bool   `json:"authorized"`
		UserID     string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Authorized {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}

	return result.UserID, true
}

// ── Submit ────────────────────────────────────────────────────────────────────

func handleSubmit(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleSubmit")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "createExecution", "forge/executions")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.Info("submit execution request", "user_id", userID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var req submitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Image == "" || len(req.Command) == 0 {
		http.Error(w, "image and command are required", http.StatusBadRequest)
		return
	}
	// allowedImages == nil means ALLOWED_IMAGES was not configured: deny all.
	if allowedImages == nil || !allowedImages[req.Image] {
		http.Error(w, "image not allowed", http.StatusBadRequest)
		return
	}
	if req.Timeout <= 0 {
		req.Timeout = defaultTimeout
	}
	if req.Timeout > maxTimeout {
		req.Timeout = maxTimeout
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if err := validateEnvKeys(req.Env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.RunnerClass == "" {
		req.RunnerClass = "standard"
	}
	if _, err := runnerClassSpec(ctx, req.RunnerClass); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	executionID := uuid.New().String()
	span.SetAttributes(
		attribute.String("execution.id", executionID),
		attribute.String("image", req.Image),
		attribute.String("runner_class", req.RunnerClass),
	)

	exec := Execution{
		ExecutionID: executionID,
		UserID:      userID,
		Image:       req.Image,
		Command:     req.Command,
		Env:         req.Env,
		TimeoutSecs: req.Timeout,
		RunnerClass: req.RunnerClass,
		Status:      StatusPending,
	}
	if err := exec.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("submit: insert execution", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	meterSubmit.Add(ctx, 1, metric.WithAttributes(attribute.String("image", req.Image)))
	span.SetStatus(codes.Ok, "")
	slog.Info("execution submitted", "execution_id", executionID, "user_id", userID, "image", req.Image, "runner_class", req.RunnerClass)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"execution_id": executionID})
}

// ── Get ───────────────────────────────────────────────────────────────────────

func handleGet(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGet")
	defer span.End()

	executionID := r.PathValue("id")
	userID, ok := checkGatekeeper(ctx, w, r, "getExecution", "forge/executions/"+executionID)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("execution.id", executionID),
	)
	span.AddEvent("permission.granted")
	slog.Info("get execution request", "user_id", userID, "execution_id", executionID)

	row, err := (Execution{ExecutionID: executionID, UserID: userID}).Get(ctx)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		span.SetStatus(codes.Error, "execution not found")
		slog.Warn("get execution: not found", "user_id", userID, "execution_id", executionID)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	exec := row.(Execution)

	span.SetStatus(codes.Ok, "")
	slog.Info("get execution: success", "user_id", userID, "execution_id", executionID, "status", exec.Status)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(exec)
}

// ── List ──────────────────────────────────────────────────────────────────────

func handleList(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleList")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listExecution", "forge/executions")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.Info("list executions request", "user_id", userID)

	rows, err := (Execution{UserID: userID}).List(ctx, 100, 0)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	executions := make([]Execution, len(rows))
	for i, r := range rows {
		executions[i] = r.(Execution)
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("list executions: success", "user_id", userID, "count", len(executions))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(executions)
}

// ── Cancel ────────────────────────────────────────────────────────────────────

func handleCancel(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCancel")
		defer span.End()

		executionID := r.PathValue("id")
		userID, ok := checkGatekeeper(ctx, w, r, "deleteExecution", "forge/executions/"+executionID)
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.SetAttributes(
			attribute.String("user.id", userID),
			attribute.String("execution.id", executionID),
		)
		span.AddEvent("permission.granted")
		slog.Info("cancel execution request", "user_id", userID, "execution_id", executionID)

		row, err := (Execution{ExecutionID: executionID, UserID: userID}).Get(ctx)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "execution not found")
			slog.Warn("cancel execution: not found", "user_id", userID, "execution_id", executionID)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.Error("cancel execution: db error", "user_id", userID, "execution_id", executionID, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		exec := row.(Execution)

		switch exec.Status {
		case StatusCompleted, StatusFailed, StatusTimedOut, StatusCancelled:
			span.SetStatus(codes.Ok, "")
			slog.Warn("cancel execution: already finished", "user_id", userID, "execution_id", executionID, "status", exec.Status)
			http.Error(w, "execution already finished", http.StatusConflict)
			return
		case StatusPending:
			if _, err := exec.Cancel(ctx); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "db update failed")
				slog.Error("cancel execution: db update failed", "user_id", userID, "execution_id", executionID, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		case StatusRunning:
			pool.Cancel(executionID)
		}

		meterCancel.Add(ctx, 1)
		span.SetStatus(codes.Ok, "")
		slog.Info("execution cancelled", "execution_id", executionID, "user_id", userID)
		w.WriteHeader(http.StatusNoContent)
	}
}

