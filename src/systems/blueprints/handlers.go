package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// ── State handlers ────────────────────────────────────────────────────────────

func handleGetState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "getState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "getState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	if cached, ok := stateCacheGet(ctx, workspaceKey); ok {
		slog.Info("state cache hit", "workspace", workspaceKey)
		// The cache stores ciphertext so Redis never holds plaintext state.
		if plaintext, err := decrypt(cached); err == nil {
			span.SetStatus(codes.Ok, "")
			meterGetState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "found")))
			w.Header().Set("Content-Type", "application/json")
			w.Write(plaintext)
			return
		}
		// Decrypt failure means a stale or corrupt cache entry; evict and re-read.
		stateCacheDel(ctx, workspaceKey)
		slog.Warn("state cache: decrypt failed, evicting", "workspace", workspaceKey)
	}

	row, err := (State{Workspace: workspaceKey}).Get(ctx)
	if isDbNotFound(err) {
		slog.Info("state not found", "workspace", workspaceKey)
		span.SetStatus(codes.Ok, "")
		meterGetState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "not_found")))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state read failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := row.(State).Data

	plaintext, err := decrypt(data)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state decrypt failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	stateCacheSet(ctx, workspaceKey, data)
	slog.Info("state retrieved", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterGetState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "found")))
	w.Header().Set("Content-Type", "application/json")
	w.Write(plaintext)
}

func handleUpdateState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "updateState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "updateState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	plaintext, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body, err := encrypt(plaintext)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state encrypt failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	lockID := r.URL.Query().Get("ID")

	existingLock, upsertErr := (State{Workspace: workspaceKey}).UpsertAtomic(ctx, body, lockID)
	if upsertErr != nil {
		if errors.Is(upsertErr, ErrWorkspaceLocked) || errors.Is(upsertErr, ErrLockIDMismatch) {
			slog.Warn("state update rejected: lock conflict", "workspace", workspaceKey)
			span.SetStatus(codes.Error, "locked")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(existingLock)) //nolint:errcheck
			return
		}
		span.RecordError(upsertErr)
		span.SetStatus(codes.Error, upsertErr.Error())
		slog.Error("state update failed", "workspace", workspaceKey, "error", upsertErr)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	stateCacheDel(ctx, workspaceKey)
	slog.Info("state updated", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterUpdateState.Add(ctx, 1)
	w.WriteHeader(http.StatusOK)
}

func handleDeleteState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "deleteState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "deleteState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	if err := (State{Workspace: workspaceKey}).Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("state delete failed", "workspace", workspaceKey, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	stateCacheDel(ctx, workspaceKey)
	slog.Info("state deleted", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterDeleteState.Add(ctx, 1)
	w.WriteHeader(http.StatusOK)
}

func handleLockState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "lockState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "lockState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !json.Valid(body) {
		span.SetStatus(codes.Error, "invalid lock data")
		http.Error(w, "bad request: lock data must be valid JSON", http.StatusBadRequest)
		return
	}

	existingLock, lockErr := (StateLock{Workspace: workspaceKey}).LockAtomic(ctx, string(body))
	if lockErr != nil {
		if errors.Is(lockErr, ErrAlreadyLocked) {
			slog.Warn("lock conflict", "workspace", workspaceKey)
			span.SetStatus(codes.Error, "lock conflict")
			meterLockState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "conflict")))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusLocked)
			w.Write([]byte(existingLock)) //nolint:errcheck
			return
		}
		span.RecordError(lockErr)
		span.SetStatus(codes.Error, lockErr.Error())
		slog.Error("lock insert failed", "workspace", workspaceKey, "error", lockErr)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("state locked", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterLockState.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "ok")))
	w.WriteHeader(http.StatusOK)
}

func handleUnlockState(w http.ResponseWriter, r *http.Request, workspaceKey string) {
	ctx, span := otel.Tracer("blueprints").Start(r.Context(), "unlockState")
	defer span.End()
	span.SetAttributes(attribute.String("workspace", workspaceKey))

	if !requireWorkspaceAuth(ctx, w, r, workspaceKey, "unlockState") {
		span.SetStatus(codes.Error, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var reqID string
	if len(body) > 0 {
		var reqData map[string]any
		if json.Unmarshal(body, &reqData) == nil {
			reqID, _ = reqData["ID"].(string)
		}
	}

	if err := (StateLock{Workspace: workspaceKey}).UnlockAtomic(ctx, reqID); err != nil {
		if errors.Is(err, ErrLockIDMismatch) {
			slog.Warn("unlock rejected: lock id mismatch", "workspace", workspaceKey)
			span.SetStatus(codes.Error, "lock id mismatch")
			http.Error(w, "lock ID mismatch", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("state unlocked", "workspace", workspaceKey)
	span.SetStatus(codes.Ok, "")
	meterUnlockState.Add(ctx, 1)
	w.WriteHeader(http.StatusOK)
}

// ── Route helpers ─────────────────────────────────────────────────────────────

func userKey(r *http.Request) (string, string) {
	u, w := r.PathValue("username"), r.PathValue("workspace")
	return u + "/" + w, "states/" + u + "/" + w
}

// lockUnlock dispatches LOCK/UNLOCK custom methods to their handlers.
// Go's ServeMux only accepts standard HTTP methods as route prefixes, so LOCK
// and UNLOCK (WebDAV/Terraform protocol) must be caught by a method-agnostic
// pattern and dispatched manually here.
func lockUnlock(keyFn func(*http.Request) (string, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k, _ := keyFn(r)
		switch r.Method {
		case "LOCK":
			handleLockState(w, r, k)
		case "UNLOCK":
			handleUnlockState(w, r, k)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
