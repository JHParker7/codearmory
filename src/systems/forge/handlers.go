package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
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
		if blockedEnvKeys[k] {
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
		return
	}
	allowedImages = make(map[string]bool)
	for _, img := range splitTrim(raw) {
		if img != "" {
			allowedImages[img] = true
		}
	}
}

func splitTrim(s string) []string {
	parts := make([]string, 0)
	for _, p := range splitComma(s) {
		if t := trimSpace(p); t != "" {
			parts = append(parts, t)
		}
	}
	return parts
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

var conductorForwardKey = os.Getenv("CONDUCTOR_FORWARD_KEY")

// extractUserID reads the X-User-ID header injected by Conductor after Gatekeeper
// has authenticated and authorised the request. When CONDUCTOR_FORWARD_KEY is set,
// the accompanying HMAC token is verified to ensure the header was set by Conductor
// and not injected by another service on the internal network.
func extractUserID(r *http.Request) (string, bool) {
	id := r.Header.Get("X-User-ID")
	if id == "" {
		return "", false
	}
	if conductorForwardKey != "" {
		tok := r.Header.Get("X-Conductor-Token")
		ts := r.Header.Get("X-Conductor-Timestamp")
		if tok == "" || ts == "" {
			slog.Warn("forge: missing X-Conductor-Token or X-Conductor-Timestamp")
			return "", false
		}
		tsInt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || abs(time.Now().Unix()-tsInt) > 30 {
			slog.Warn("forge: X-Conductor-Timestamp out of window or invalid")
			return "", false
		}
		mac := hmac.New(sha256.New, []byte(conductorForwardKey))
		fmt.Fprintf(mac, "conductor:%s:%s", id, ts)
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(tok), []byte(expected)) {
			slog.Warn("forge: X-Conductor-Token HMAC mismatch")
			return "", false
		}
	}
	return id, true
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ── Submit ────────────────────────────────────────────────────────────────────

func handleSubmit(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleSubmit")
		defer span.End()

		userID, ok := extractUserID(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

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

		cmdJSON, _ := json.Marshal(req.Command)
		envJSON, _ := json.Marshal(req.Env)
		executionID := uuid.New().String()

		_, err = db.Exec(ctx,
			`INSERT INTO executions (execution_id, user_id, image, command, env, timeout_secs)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			executionID, userID, req.Image, cmdJSON, envJSON, req.Timeout,
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.Error("submit: insert execution", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		meterSubmit.Add(ctx, 1, metric.WithAttributes(attribute.String("image", req.Image)))
		span.SetStatus(codes.Ok, "")
		slog.Info("execution submitted", "execution_id", executionID, "user_id", userID, "image", req.Image)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"execution_id": executionID})
	}
}

// ── Get ───────────────────────────────────────────────────────────────────────

func handleGet(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGet")
	defer span.End()

	userID, ok := extractUserID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	executionID := r.PathValue("id")

	exec, err := getExecution(ctx, executionID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(exec)
}

// ── List ──────────────────────────────────────────────────────────────────────

func handleList(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleList")
	defer span.End()

	userID, ok := extractUserID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := db.Query(ctx,
		`SELECT execution_id, user_id, image, status, exit_code, created_at, started_at, ended_at
		 FROM executions
		 WHERE user_id = $1
		 ORDER BY created_at DESC
		 LIMIT 100`,
		userID,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	executions := []Execution{}
	for rows.Next() {
		var e Execution
		if err := rows.Scan(&e.ExecutionID, &e.UserID, &e.Image, &e.Status, &e.ExitCode, &e.CreatedAt, &e.StartedAt, &e.EndedAt); err != nil {
			continue
		}
		executions = append(executions, e)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(executions)
}

// ── Cancel ────────────────────────────────────────────────────────────────────

func handleCancel(pool *WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCancel")
		defer span.End()

		userID, ok := extractUserID(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		executionID := r.PathValue("id")

		exec, err := getExecution(ctx, executionID, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		switch exec.Status {
		case StatusCompleted, StatusFailed, StatusTimedOut, StatusCancelled:
			http.Error(w, "execution already finished", http.StatusConflict)
			return
		case StatusPending:
			_, err = db.Exec(ctx,
				`UPDATE executions SET status = 'cancelled', ended_at = now() WHERE execution_id = $1 AND status = 'pending'`,
				executionID,
			)
			if err != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		case StatusRunning:
			pool.Cancel(executionID)
		}

		meterCancel.Add(ctx, 1)
		slog.Info("execution cancelled", "execution_id", executionID, "user_id", userID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// ── Shared ────────────────────────────────────────────────────────────────────

func getExecution(ctx context.Context, executionID, userID string) (Execution, error) {
	var e Execution
	err := db.QueryRow(ctx,
		`SELECT execution_id, user_id, image, status, exit_code, stdout, stderr,
		        created_at, started_at, ended_at
		 FROM executions
		 WHERE execution_id = $1 AND user_id = $2`,
		executionID, userID,
	).Scan(&e.ExecutionID, &e.UserID, &e.Image, &e.Status, &e.ExitCode,
		&e.Stdout, &e.Stderr, &e.CreatedAt, &e.StartedAt, &e.EndedAt)
	return e, err
}
