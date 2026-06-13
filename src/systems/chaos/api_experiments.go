package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

func handleCreateExperiment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("chaos").Start(r.Context(), "handleCreateExperiment")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createExperiment", "chaos/experiments")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.AddEvent("permission.granted")

	var req createExperimentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.OutpostID == "" {
		http.Error(w, "outpost_id is required", http.StatusBadRequest)
		return
	}
	et, ok := lookupExperimentType(req.ExperimentType)
	if !ok {
		http.Error(w, "unknown experiment_type", http.StatusBadRequest)
		return
	}
	if req.TargetAppLabel == "" {
		http.Error(w, "target_app_label is required (e.g. app.kubernetes.io/component=conductor)", http.StatusBadRequest)
		return
	}
	targetNS := req.TargetAppNS
	if targetNS == "" {
		http.Error(w, "target_app_ns is required", http.StatusBadRequest)
		return
	}
	targetKind := req.TargetAppKind
	if targetKind == "" {
		targetKind = "deployment"
	}
	// Merge caller params over the type defaults so unspecified knobs keep sane values.
	params := map[string]string{}
	for k, v := range et.Params {
		params[k] = v
	}
	for k, v := range req.Params {
		params[k] = v
	}

	experimentID := uuid.New().String()
	exp := Experiment{
		ExperimentID:   experimentID,
		UserID:         userID,
		OrgID:          orgID,
		OutpostID:      req.OutpostID,
		ExperimentType: et.Name,
		TargetAppNS:    targetNS,
		TargetAppLabel: req.TargetAppLabel,
		TargetAppKind:  targetKind,
		EngineName:     engineName(experimentID),
		Params:         params,
		Status:         StatusPending,
		Active:         true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := exp.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create experiment: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to create experiment", http.StatusInternalServerError)
		return
	}

	// Drive the outpost: enqueue a run-experiment command. If the gateway is
	// unreachable, mark the experiment errored so it does not hang as pending.
	cmdPayload := map[string]any{
		"experiment_id":    exp.ExperimentID,
		"experiment_type":  exp.ExperimentType,
		"engine_name":      exp.EngineName,
		"target_app_ns":    exp.TargetAppNS,
		"target_app_label": exp.TargetAppLabel,
		"target_app_kind":  exp.TargetAppKind,
		"params":           exp.Params,
	}
	if err := enqueueCommand(ctx, exp.OutpostID, "chaos", "run-experiment", orgID, userID, cmdPayload); err != nil {
		slog.Error("create experiment: enqueue command failed", "experiment_id", exp.ExperimentID, "error", err)
		exp.Status = StatusError
		exp.Verdict = "dispatch failed"
		now := time.Now().UTC()
		exp.EndedAt = &now
		_ = exp.Update(ctx)
		http.Error(w, "failed to dispatch experiment to outpost", http.StatusBadGateway)
		return
	}

	meterExperimentsCreated.Add(ctx, 1, metric.WithAttributes(attribute.String("experiment_type", exp.ExperimentType)))
	notifyHooks(ctx, eventExperimentStarted, exp)
	span.SetAttributes(attribute.String("experiment.id", exp.ExperimentID))
	span.SetStatus(codes.Ok, "")
	slog.Info("experiment created", "experiment_id", exp.ExperimentID, "user_id", userID, "outpost_id", exp.OutpostID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(exp) //nolint:errcheck
}

func handleListExperiments(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("chaos").Start(r.Context(), "handleListExperiments")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listExperiment", "chaos/experiments")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	limit, offset := paginationParams(r)
	filter := Experiment{}
	if orgID != "" {
		filter.OrgID = orgID
	} else {
		filter.UserID = userID
	}
	items, err := filter.List(ctx, limit, offset)
	if err != nil {
		span.RecordError(err)
		slog.Error("list experiments: db error", "error", err)
		http.Error(w, "failed to list experiments", http.StatusInternalServerError)
		return
	}
	out := make([]Experiment, 0, len(items))
	for _, it := range items {
		out = append(out, it.(Experiment))
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out) //nolint:errcheck
}

func handleGetExperiment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("chaos").Start(r.Context(), "handleGetExperiment")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getExperiment", "chaos/experiments/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	res, err := (Experiment{ExperimentID: id}).Get(ctx)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "experiment not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to get experiment", http.StatusInternalServerError)
		return
	}
	exp := res.(Experiment)
	if !canAccess(exp, userID, orgID) {
		http.Error(w, "experiment not found", http.StatusNotFound)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(exp) //nolint:errcheck
}

func handleDeleteExperiment(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("chaos").Start(r.Context(), "handleDeleteExperiment")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteExperiment", "chaos/experiments/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	res, err := (Experiment{ExperimentID: id}).Get(ctx)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "experiment not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to get experiment", http.StatusInternalServerError)
		return
	}
	exp := res.(Experiment)
	if !canAccess(exp, userID, orgID) {
		http.Error(w, "experiment not found", http.StatusNotFound)
		return
	}

	// Best-effort stop command so the outpost tears the ChaosEngine down.
	if !isTerminal(exp.Status) {
		if err := enqueueCommand(ctx, exp.OutpostID, "chaos", "stop", orgID, userID, map[string]any{
			"experiment_id": exp.ExperimentID,
			"engine_name":   exp.EngineName,
			"target_app_ns": exp.TargetAppNS,
		}); err != nil {
			slog.Warn("delete experiment: stop command failed", "experiment_id", exp.ExperimentID, "error", err)
		}
		now := time.Now().UTC()
		exp.Status = StatusStopped
		exp.EndedAt = &now
		if err := exp.Update(ctx); err != nil {
			slog.Warn("delete experiment: failed to mark stopped", "experiment_id", exp.ExperimentID, "error", err)
		}
	}
	if err := exp.Remove(ctx); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to delete experiment", http.StatusInternalServerError)
		return
	}
	span.SetStatus(codes.Ok, "")
	w.WriteHeader(http.StatusNoContent)
}

func handleListExperimentTypes(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("chaos").Start(r.Context(), "handleListExperimentTypes")
	defer span.End()
	if _, _, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listExperimentType", "chaos/experiment-types"); !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(experimentTypes) //nolint:errcheck
}

func lookupExperimentType(name string) (ExperimentType, bool) {
	idx := slices.IndexFunc(experimentTypes, func(t ExperimentType) bool { return t.Name == name })
	if idx < 0 {
		return ExperimentType{}, false
	}
	return experimentTypes[idx], true
}

// engineName derives a deterministic, DNS-safe ChaosEngine name from the
// experiment id. The outpost creates the engine under this name and the verdict
// event carries the experiment_id back, so the control plane never needs to map
// engine names.
func engineName(experimentID string) string {
	return "exp-" + experimentID
}

func canAccess(e Experiment, userID, orgID string) bool {
	return e.UserID == userID || (orgID != "" && e.OrgID == orgID)
}

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

func paginationParams(r *http.Request) (limit, offset int) {
	limit = defaultPageLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}
