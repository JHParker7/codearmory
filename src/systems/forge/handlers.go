package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

var allowedImages map[string]bool

func initAllowedImages(raw string) {
	allowedImages = make(map[string]bool)
	for _, img := range strings.Split(raw, ",") {
		if img = strings.TrimSpace(img); img != "" {
			allowedImages[img] = true
		}
	}
}

// extractUserID decodes the JWT payload (without signature verification — that
// is already done by Conductor via Gatekeeper) and returns the sub claim.
func extractUserID(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", false
	}
	return claims.Sub, true
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
		if len(allowedImages) > 0 && !allowedImages[req.Image] {
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
