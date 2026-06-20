package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

type runnerClassBody struct {
	Name          string `json:"name"`
	MemoryMB      int64  `json:"memory_mb"`
	CPUMillicores int64  `json:"cpu_millicores"`
	PidsLimit     int64  `json:"pids_limit"`
	TmpfsMB       int64  `json:"tmpfs_mb"`
	DiskGB        int64  `json:"disk_gb"`
	Backend       string `json:"backend"`
	Enabled       bool   `json:"enabled"`
	Privileged    bool   `json:"privileged"`
}

func validateRunnerClassBody(b runnerClassBody) error {
	if b.MemoryMB < 64 {
		return fmt.Errorf("memory_mb must be at least 64")
	}
	if b.CPUMillicores < 100 {
		return fmt.Errorf("cpu_millicores must be at least 100")
	}
	if b.PidsLimit < 8 {
		return fmt.Errorf("pids_limit must be at least 8")
	}
	if b.TmpfsMB < 16 {
		return fmt.Errorf("tmpfs_mb must be at least 16")
	}
	if b.DiskGB < 0 {
		return fmt.Errorf("disk_gb must not be negative")
	}
	return nil
}

// backendOrDefault resolves an empty backend name to "default", the legacy
// single-runtime backend, so classes that omit it keep their previous behaviour.
func backendOrDefault(backend string) string {
	if backend == "" {
		return "default"
	}
	return backend
}

// errBackendLookup wraps a non-"not found" failure (e.g. a transient DB error)
// while resolving the backend a privileged runner class targets, so the handler
// returns 500 instead of reporting it as a 400 "backend not found" validation
// error.
var errBackendLookup = errors.New("backend lookup failed")

// validatePrivilegedBackend rejects a privileged runner class that does not target
// a VM-isolated backend. Root + writable rootfs is only safe behind a VM boundary;
// on shared-kernel container backends (docker/kubernetes/runc) it would be a
// host-kernel escape risk. This is the user-facing guard; buildJob enforces the
// same rule at runtime as a second layer (it drops privileged off a non-VM
// backend). A genuine lookup failure is returned wrapped in errBackendLookup so
// the caller can distinguish it from a real validation rejection.
func validatePrivilegedBackend(ctx context.Context, backend string, privileged bool) error {
	if !privileged {
		return nil
	}
	row, err := (RuntimeBackend{Name: backend}).Get(ctx)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("privileged requires an existing VM-isolated backend; backend %q not found", backend)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errBackendLookup, err)
	}
	if b := row.(RuntimeBackend); !isVMIsolatedBackendType(b.Type) {
		return fmt.Errorf("privileged is only allowed on VM-isolated backends (kata, proxmox); backend %q is type %q", backend, b.Type)
	}
	return nil
}

// ── List ─────────────────────────────────────────────────────────────────────

func handleListRunnerClasses(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleListRunnerClasses")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listRunnerClass", "forge/runner-classes")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "list runner classes request", "user_id", userID)

	rows, err := (RunnerClass{}).List(ctx, 0, 0)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list runner classes: db error", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	classes := make([]RunnerClass, len(rows))
	for i, r := range rows {
		classes[i] = r.(RunnerClass)
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "list runner classes: success", "user_id", userID, "count", len(classes))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(classes)
}

// ── Get ──────────────────────────────────────────────────────────────────────

func handleGetRunnerClass(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGetRunnerClass")
	defer span.End()

	name := r.PathValue("name")
	userID, ok := checkGatekeeper(ctx, w, r, "getRunnerClass", "forge/runner-classes/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("runner_class.name", name),
	)
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "get runner class request", "user_id", userID, "name", name)

	row, err := (RunnerClass{Name: name}).Get(ctx)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		span.SetStatus(codes.Error, "runner class not found")
		slog.WarnContext(ctx, "get runner class: not found", "user_id", userID, "name", name)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "get runner class: db error", "user_id", userID, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	rc := row.(RunnerClass)

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "get runner class: success", "user_id", userID, "name", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rc)
}

// ── Create ───────────────────────────────────────────────────────────────────

func handleCreateRunnerClass(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCreateRunnerClass")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "createRunnerClass", "forge/runner-classes")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "create runner class request", "user_id", userID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var b runnerClassBody
	if err := json.Unmarshal(body, &b); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if b.Name == "" {
		span.SetStatus(codes.Error, "name required")
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := validateRunnerClassBody(b); err != nil {
		span.SetStatus(codes.Error, "validation failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	span.SetAttributes(attribute.String("runner_class.name", b.Name))

	if err := validatePrivilegedBackend(ctx, backendOrDefault(b.Backend), b.Privileged); err != nil {
		if errors.Is(err, errBackendLookup) {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db query failed")
			slog.ErrorContext(ctx, "create runner class: backend lookup error", "user_id", userID, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Error, "validation failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rc := RunnerClass{
		Name:          b.Name,
		MemoryMB:      b.MemoryMB,
		CPUMillicores: b.CPUMillicores,
		PidsLimit:     b.PidsLimit,
		TmpfsMB:       b.TmpfsMB,
		DiskGB:        b.DiskGB,
		Backend:       backendOrDefault(b.Backend),
		Enabled:       b.Enabled,
		Privileged:    b.Privileged,
	}
	if err := rc.Add(ctx); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			span.SetStatus(codes.Ok, "")
			slog.WarnContext(ctx, "create runner class: already exists", "user_id", userID, "name", b.Name)
			http.Error(w, "runner class already exists", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create runner class: db error", "user_id", userID, "name", b.Name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "create runner class: success", "user_id", userID, "name", rc.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rc)
}

// ── Update ───────────────────────────────────────────────────────────────────

func handleUpdateRunnerClass(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleUpdateRunnerClass")
	defer span.End()

	name := r.PathValue("name")
	userID, ok := checkGatekeeper(ctx, w, r, "updateRunnerClass", "forge/runner-classes/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("runner_class.name", name),
	)
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "update runner class request", "user_id", userID, "name", name)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var b runnerClassBody
	if err := json.Unmarshal(body, &b); err != nil {
		span.SetStatus(codes.Error, "invalid request body")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := validateRunnerClassBody(b); err != nil {
		span.SetStatus(codes.Error, "validation failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validatePrivilegedBackend(ctx, backendOrDefault(b.Backend), b.Privileged); err != nil {
		if errors.Is(err, errBackendLookup) {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db query failed")
			slog.ErrorContext(ctx, "update runner class: backend lookup error", "user_id", userID, "name", name, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Error, "validation failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rc := RunnerClass{
		Name: name, MemoryMB: b.MemoryMB, CPUMillicores: b.CPUMillicores,
		PidsLimit: b.PidsLimit, TmpfsMB: b.TmpfsMB, DiskGB: b.DiskGB,
		Backend: backendOrDefault(b.Backend), Enabled: b.Enabled,
		Privileged: b.Privileged,
	}
	if err := rc.Update(ctx); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "runner class not found")
			slog.WarnContext(ctx, "update runner class: not found", "user_id", userID, "name", name)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update runner class: db error", "user_id", userID, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "update runner class: success", "user_id", userID, "name", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rc)
}

// ── Delete ───────────────────────────────────────────────────────────────────

func handleDeleteRunnerClass(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleDeleteRunnerClass")
	defer span.End()

	name := r.PathValue("name")
	userID, ok := checkGatekeeper(ctx, w, r, "deleteRunnerClass", "forge/runner-classes/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(
		attribute.String("user.id", userID),
		attribute.String("runner_class.name", name),
	)
	span.AddEvent("permission.granted")
	slog.InfoContext(ctx, "delete runner class request", "user_id", userID, "name", name)

	if err := (RunnerClass{Name: name}).Remove(ctx); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "runner class not found")
			slog.WarnContext(ctx, "delete runner class: not found", "user_id", userID, "name", name)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db delete failed")
		slog.ErrorContext(ctx, "delete runner class: db error", "user_id", userID, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "delete runner class: success", "user_id", userID, "name", name)
	w.WriteHeader(http.StatusNoContent)
}
