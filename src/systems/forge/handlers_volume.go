package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// volumeRuntime resolves the backend to a runtime that can materialise a shared
// volume. A backend that cannot materialise one yields a clear error rather than a
// confusing type panic.
func volumeRuntime(ctx context.Context, reg *runtimeRegistry, backend string) (VolumeRuntime, error) {
	rt, err := reg.Get(ctx, backend)
	if err != nil {
		return nil, err
	}
	vr, ok := rt.(VolumeRuntime)
	if !ok {
		return nil, fmt.Errorf("backend %q does not support shared volumes", backend)
	}
	return vr, nil
}

// ── Create ───────────────────────────────────────────────────────────────────

// handleCreateVolume provisions a shared workspace volume for a workflow. It
// enforces the admin's per-workflow total-size cap, creates the backend resource
// (PVC / docker volume), and records the accounting row. Creating the same
// (workflow_id, name) twice is idempotent — the resource name is deterministic — so
// a retried step does not fail.
func handleCreateVolume(reg *runtimeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleCreateVolume")
		defer span.End()

		userID, orgID, ok := checkGatekeeperOrg(ctx, w, r, "createVolume", "forge/volumes")
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.SetAttributes(attribute.String("user.id", userID))
		span.AddEvent("permission.granted")

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil || !json.Valid(body) {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var req createVolumeRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		resourceName, err := normalizeCreateVolume(&req)
		if err != nil {
			span.SetStatus(codes.Error, "validation failed")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Pick the backend the volume lives on: an explicit backend wins, else the
		// named runner class's backend, else "default" — so the volume lands on the
		// same runtime as the steps that will attach it.
		backend := req.Backend
		if backend == "" && req.RunnerClass != "" {
			rc, rcErr := runnerClassSpec(ctx, req.RunnerClass)
			if rcErr != nil {
				http.Error(w, rcErr.Error(), http.StatusBadRequest)
				return
			}
			backend = rc.Backend
		}
		if backend == "" {
			backend = "default"
		}
		span.SetAttributes(
			attribute.String("volume.resource_name", resourceName),
			attribute.String("volume.workflow_id", req.WorkflowID),
			attribute.String("backend", backend),
		)

		vr, err := volumeRuntime(ctx, reg, backend)
		if err != nil {
			span.SetStatus(codes.Error, "backend does not support volumes")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Idempotent: a repeat create of the same (workflow_id, name) returns the
		// existing row (re-ensuring its backend resource exists) without re-checking
		// the cap — so a retried create-volume step never fails or double-counts.
		if existing, gerr := getActiveVolume(ctx, req.WorkflowID, req.Name); gerr == nil {
			if cerr := vr.CreateVolume(ctx, VolumeSpec{ResourceName: existing.ResourceName, SizeMB: existing.SizeMB, Medium: existing.Medium}); cerr != nil {
				span.RecordError(cerr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			span.SetStatus(codes.Ok, "")
			slog.InfoContext(ctx, "volume already exists (idempotent create)", "user_id", userID, "resource_name", existing.ResourceName, "workflow_id", req.WorkflowID)
			writeJSON(w, http.StatusOK, existing)
			return
		}

		// New volume: enforce the per-workflow total-size cap. Best-effort accounting —
		// a tiny race between two concurrent creates for the same workflow could let
		// the total marginally exceed the cap, but each single volume is still bounded
		// and workflow steps create volumes sequentially, so the window is negligible.
		used, err := sumActiveWorkflowVolumeMB(ctx, req.WorkflowID)
		if err != nil {
			span.RecordError(err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if used+req.SizeMB > maxWorkflowVolumeMB {
			span.SetStatus(codes.Error, "workflow volume cap exceeded")
			slog.WarnContext(ctx, "create volume: workflow cap exceeded", "user_id", userID, "workflow_id", req.WorkflowID, "used_mb", used, "requested_mb", req.SizeMB, "cap_mb", maxWorkflowVolumeMB)
			http.Error(w, fmt.Sprintf("workflow volume limit exceeded: %d MB used + %d MB requested > %d MB cap", used, req.SizeMB, maxWorkflowVolumeMB), http.StatusConflict)
			return
		}

		if err := vr.CreateVolume(ctx, VolumeSpec{ResourceName: resourceName, SizeMB: req.SizeMB, Medium: req.Medium}); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "create backend volume failed")
			slog.ErrorContext(ctx, "create volume: backend error", "user_id", userID, "resource_name", resourceName, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		vol := Volume{
			ResourceName: resourceName,
			WorkflowID:   req.WorkflowID,
			Name:         req.Name,
			UserID:       userID,
			OrgID:        orgID,
			Backend:      backend,
			SizeMB:       req.SizeMB,
			Medium:       req.Medium,
			MountPath:    req.MountPath,
			Status:       volumeStatusActive,
		}
		if err := vol.Add(ctx); err != nil {
			// Lost a race with a concurrent identical create (deterministic resource
			// name → primary-key conflict)? Return the row that won. GORM here is not
			// configured with TranslateError, so match on the row now existing rather
			// than on a specific error type.
			if existing, gerr := getActiveVolume(ctx, req.WorkflowID, req.Name); gerr == nil {
				writeJSON(w, http.StatusOK, existing)
				return
			}
			// Not a race: a soft-deleted row is still holding this deterministic
			// resource_name (the reaper removed the backend resource but kept the row),
			// which blocks the insert forever. Revive it so a reused volume name — a
			// cache shared across runs — can be recreated instead of being permanently
			// wedged. See reviveDeletedVolume.
			if revived, rerr := reviveDeletedVolume(ctx, vol); rerr == nil {
				span.SetStatus(codes.Ok, "")
				slog.InfoContext(ctx, "volume revived (reused name after reap)", "user_id", userID, "resource_name", resourceName, "workflow_id", req.WorkflowID)
				writeJSON(w, http.StatusCreated, revived)
				return
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "db insert failed")
			slog.ErrorContext(ctx, "create volume: db error", "user_id", userID, "resource_name", resourceName, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "volume created", "user_id", userID, "resource_name", resourceName, "workflow_id", req.WorkflowID, "size_mb", req.SizeMB, "backend", backend)
		writeJSON(w, http.StatusCreated, vol)
	}
}

// ── Status ───────────────────────────────────────────────────────────────────

// handleGetVolume reports a shared volume's provisioning readiness (provisioning |
// ready | failed). It backs the forge/create-volume async poll: the workflows worker
// polls it to "ready" before the next step mounts the volume, so slow dynamic
// provisioning completes outside the mounting step's command timeout.
func handleGetVolume(reg *runtimeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleGetVolume")
		defer span.End()

		resourceName := r.PathValue("id")
		userID, ok := checkGatekeeper(ctx, w, r, "getVolume", "forge/volumes/"+resourceName)
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		span.SetAttributes(
			attribute.String("user.id", userID),
			attribute.String("volume.resource_name", resourceName),
		)
		span.AddEvent("permission.granted")

		row, err := (Volume{ResourceName: resourceName}).Get(ctx)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			span.RecordError(err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		vol := row.(Volume)
		if vol.UserID != userID {
			// Do not disclose the existence of another caller's volume.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		vr, err := volumeRuntime(ctx, reg, vol.Backend)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state, detail, err := vr.VolumeStatus(ctx, vol.ResourceName)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "backend status error")
			slog.ErrorContext(ctx, "get volume status: backend error", "user_id", userID, "resource_name", resourceName, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		span.SetStatus(codes.Ok, "")
		writeJSON(w, http.StatusOK, volumeStatusResponse{ResourceName: vol.ResourceName, Status: state, Detail: detail})
	}
}

// ── List ─────────────────────────────────────────────────────────────────────

// handleListVolumes lists the active volumes for the workflow_id query param (all
// active volumes when omitted), scoped to the caller.
func handleListVolumes(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("forge").Start(r.Context(), "handleListVolumes")
	defer span.End()

	userID, ok := checkGatekeeper(ctx, w, r, "listVolume", "forge/volumes")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID))
	span.AddEvent("permission.granted")

	rows, err := (Volume{WorkflowID: r.URL.Query().Get("workflow_id")}).List(ctx, 0, 0)
	if err != nil {
		span.RecordError(err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	volumes := make([]Volume, 0, len(rows))
	for _, row := range rows {
		if v := row.(Volume); v.UserID == userID {
			volumes = append(volumes, v)
		}
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, volumes)
}

// ── Delete ───────────────────────────────────────────────────────────────────

// handleDeleteVolumes tears down volumes. With ?workflow_id=<id> it removes every
// active volume for that workflow (the run-teardown path); adding &name=<name>
// removes just that one. Deletion is scoped to the caller and idempotent — an
// already-gone volume is a no-op — so a retried teardown always succeeds.
func handleDeleteVolumes(reg *runtimeRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("forge").Start(r.Context(), "handleDeleteVolumes")
		defer span.End()

		userID, ok := checkGatekeeper(ctx, w, r, "deleteVolume", "forge/volumes")
		if !ok {
			span.SetStatus(codes.Ok, "")
			return
		}
		workflowID := r.URL.Query().Get("workflow_id")
		if workflowID == "" {
			http.Error(w, "workflow_id is required", http.StatusBadRequest)
			return
		}
		span.SetAttributes(
			attribute.String("user.id", userID),
			attribute.String("volume.workflow_id", workflowID),
		)
		span.AddEvent("permission.granted")

		rows, err := (Volume{WorkflowID: workflowID}).List(ctx, 0, 0)
		if err != nil {
			span.RecordError(err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		name := r.URL.Query().Get("name")
		deleted := 0
		for _, row := range rows {
			v := row.(Volume)
			if v.UserID != userID || (name != "" && v.Name != name) {
				continue
			}
			if err := deleteOneVolume(ctx, reg, v); err != nil {
				span.RecordError(err)
				slog.ErrorContext(ctx, "delete volume: failed", "user_id", userID, "resource_name", v.ResourceName, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			deleted++
		}

		span.SetStatus(codes.Ok, "")
		slog.InfoContext(ctx, "volumes deleted", "user_id", userID, "workflow_id", workflowID, "count", deleted)
		writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
	}
}

// deleteOneVolume removes a volume's backend resource then marks its row deleted.
// The backend delete is idempotent, and the row flip stops it counting against the
// cap; ordering (resource first, row second) means a crash in between leaves the
// row for the age reaper rather than orphaning storage silently.
func deleteOneVolume(ctx context.Context, reg *runtimeRegistry, v Volume) error {
	vr, err := volumeRuntime(ctx, reg, v.Backend)
	if err != nil {
		return err
	}
	if err := vr.DeleteVolume(ctx, v.ResourceName); err != nil {
		return err
	}
	return markVolumeDeleted(ctx, v.ResourceName)
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// startVolumeReaper periodically removes volumes older than volumeMaxAge — the
// safety net for a workflow that crashed before deleting its volumes (a normal run
// tears them down explicitly). The age is set comfortably above the longest possible
// run, so a live run's volume is never reaped. It runs until ctx is cancelled.
func startVolumeReaper(ctx context.Context, reg *runtimeRegistry) {
	if volumeMaxAge <= 0 {
		return
	}
	interval := max(volumeMaxAge/4, time.Minute)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reapOrphanVolumes(ctx, reg)
		}
	}
}

// reapOrphanVolumes deletes every active volume older than volumeMaxAge.
func reapOrphanVolumes(ctx context.Context, reg *runtimeRegistry) {
	cutoff := time.Now().Add(-volumeMaxAge)
	vols, err := listReapableVolumes(ctx, cutoff)
	if err != nil {
		slog.ErrorContext(ctx, "volume reaper: list failed", "error", err)
		return
	}
	for _, v := range vols {
		if err := deleteOneVolume(ctx, reg, v); err != nil {
			slog.WarnContext(ctx, "volume reaper: delete failed", "resource_name", v.ResourceName, "error", err)
			continue
		}
		slog.InfoContext(ctx, "volume reaper: removed orphan", "resource_name", v.ResourceName, "workflow_id", v.WorkflowID, "age_secs", int(time.Since(v.CreatedAt).Seconds()))
	}
}
