package main

import (
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
	Enabled       bool   `json:"enabled"`
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
	slog.Info("list runner classes request", "user_id", userID)

	var classes []RunnerClass
	if err := connect().WithContext(ctx).Order("name").Find(&classes).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("list runner classes: db error", "user_id", userID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if classes == nil {
		classes = []RunnerClass{}
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("list runner classes: success", "user_id", userID, "count", len(classes))
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
	slog.Info("get runner class request", "user_id", userID, "name", name)

	var rc RunnerClass
	if err := connect().WithContext(ctx).Where("name = ?", name).First(&rc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Error, "runner class not found")
			slog.Warn("get runner class: not found", "user_id", userID, "name", name)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.Error("get runner class: db error", "user_id", userID, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("get runner class: success", "user_id", userID, "name", name)
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
	slog.Info("create runner class request", "user_id", userID)

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

	rc := RunnerClass{
		Name:          b.Name,
		MemoryMB:      b.MemoryMB,
		CPUMillicores: b.CPUMillicores,
		PidsLimit:     b.PidsLimit,
		TmpfsMB:       b.TmpfsMB,
		Enabled:       b.Enabled,
	}
	if err := connect().WithContext(ctx).Create(&rc).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			span.SetStatus(codes.Ok, "")
			slog.Warn("create runner class: already exists", "user_id", userID, "name", b.Name)
			http.Error(w, "runner class already exists", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create runner class: db error", "user_id", userID, "name", b.Name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("create runner class: success", "user_id", userID, "name", rc.Name)
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
	slog.Info("update runner class request", "user_id", userID, "name", name)

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

	result := connect().WithContext(ctx).Model(&RunnerClass{}).Where("name = ?", name).Updates(map[string]any{
		"memory_mb":      b.MemoryMB,
		"cpu_millicores": b.CPUMillicores,
		"pids_limit":     b.PidsLimit,
		"tmpfs_mb":       b.TmpfsMB,
		"enabled":        b.Enabled,
	})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db update failed")
		slog.Error("update runner class: db error", "user_id", userID, "name", name, "error", result.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result.RowsAffected == 0 {
		span.SetStatus(codes.Error, "runner class not found")
		slog.Warn("update runner class: not found", "user_id", userID, "name", name)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("update runner class: success", "user_id", userID, "name", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RunnerClass{
		Name: name, MemoryMB: b.MemoryMB, CPUMillicores: b.CPUMillicores,
		PidsLimit: b.PidsLimit, TmpfsMB: b.TmpfsMB, Enabled: b.Enabled,
	})
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
	slog.Info("delete runner class request", "user_id", userID, "name", name)

	result := connect().WithContext(ctx).Where("name = ?", name).Delete(&RunnerClass{})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, "db delete failed")
		slog.Error("delete runner class: db error", "user_id", userID, "name", name, "error", result.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result.RowsAffected == 0 {
		span.SetStatus(codes.Error, "runner class not found")
		slog.Warn("delete runner class: not found", "user_id", userID, "name", name)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("delete runner class: success", "user_id", userID, "name", name)
	w.WriteHeader(http.StatusNoContent)
}
