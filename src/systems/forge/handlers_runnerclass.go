package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
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

	if _, ok := checkGatekeeper(ctx, w, r, "listRunnerClass", "forge/runner-classes"); !ok {
		return
	}

	var classes []RunnerClass
	if err := connectRC().WithContext(ctx).Order("name").Find(&classes).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if classes == nil {
		classes = []RunnerClass{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(classes)
}

// ── Get ──────────────────────────────────────────────────────────────────────

func handleGetRunnerClass(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGetRunnerClass")
	defer span.End()

	name := r.PathValue("name")
	if _, ok := checkGatekeeper(ctx, w, r, "getRunnerClass", "forge/runner-classes/"+name); !ok {
		return
	}

	var rc RunnerClass
	if err := connectRC().WithContext(ctx).Where("name = ?", name).First(&rc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rc)
}

// ── Create ───────────────────────────────────────────────────────────────────

func handleCreateRunnerClass(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCreateRunnerClass")
	defer span.End()

	if _, ok := checkGatekeeper(ctx, w, r, "createRunnerClass", "forge/runner-classes"); !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var b runnerClassBody
	if err := json.Unmarshal(body, &b); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if b.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := validateRunnerClassBody(b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rc := RunnerClass{
		Name:          b.Name,
		MemoryMB:      b.MemoryMB,
		CPUMillicores: b.CPUMillicores,
		PidsLimit:     b.PidsLimit,
		TmpfsMB:       b.TmpfsMB,
		Enabled:       b.Enabled,
	}
	if err := connectRC().WithContext(ctx).Create(&rc).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			http.Error(w, "runner class already exists", http.StatusConflict)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Error("create runner class: insert", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("runner class created", "name", rc.Name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rc)
}

// ── Update ───────────────────────────────────────────────────────────────────

func handleUpdateRunnerClass(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleUpdateRunnerClass")
	defer span.End()

	name := r.PathValue("name")
	if _, ok := checkGatekeeper(ctx, w, r, "updateRunnerClass", "forge/runner-classes/"+name); !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil || !json.Valid(body) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var b runnerClassBody
	if err := json.Unmarshal(body, &b); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := validateRunnerClassBody(b); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result := connectRC().WithContext(ctx).Model(&RunnerClass{}).Where("name = ?", name).Updates(map[string]any{
		"memory_mb":      b.MemoryMB,
		"cpu_millicores": b.CPUMillicores,
		"pids_limit":     b.PidsLimit,
		"tmpfs_mb":       b.TmpfsMB,
		"enabled":        b.Enabled,
	})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		slog.Error("update runner class", "name", name, "error", result.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result.RowsAffected == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	slog.Info("runner class updated", "name", name)
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
	if _, ok := checkGatekeeper(ctx, w, r, "deleteRunnerClass", "forge/runner-classes/"+name); !ok {
		return
	}

	result := connectRC().WithContext(ctx).Where("name = ?", name).Delete(&RunnerClass{})
	if result.Error != nil {
		span.RecordError(result.Error)
		span.SetStatus(codes.Error, result.Error.Error())
		slog.Error("delete runner class", "name", name, "error", result.Error)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if result.RowsAffected == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	slog.Info("runner class deleted", "name", name)
	w.WriteHeader(http.StatusNoContent)
}
