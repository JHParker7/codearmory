package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

func handleListApps(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("argo").Start(r.Context(), "handleListApps")
	defer span.End()
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listApp", "argo/apps")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	apps, err := listApps(ctx, orgScope(orgID, userID))
	if err != nil {
		span.RecordError(err)
		http.Error(w, "failed to list apps", http.StatusInternalServerError)
		return
	}
	if apps == nil {
		apps = []App{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apps) //nolint:errcheck
}

func handleGetApp(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("argo").Start(r.Context(), "handleGetApp")
	defer span.End()
	name := r.PathValue("name")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getApp", "argo/apps/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	a, err := getApp(ctx, orgScope(orgID, userID), name)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to get app", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(a) //nolint:errcheck
}

func handleSyncApp(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("argo").Start(r.Context(), "handleSyncApp")
	defer span.End()
	name := r.PathValue("name")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "syncApp", "argo/apps/"+name)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}

	var req syncRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional

	// Resolve the target outpost: prefer an explicit outpost_id, else the one we
	// learned for this app from app-state events.
	outpostID := req.OutpostID
	if outpostID == "" {
		a, err := getApp(ctx, orgScope(orgID, userID), name)
		if err == nil {
			outpostID = a.OutpostID
		}
	}
	if outpostID == "" {
		http.Error(w, "unknown outpost for app; provide outpost_id (the app has not reported state yet)", http.StatusBadRequest)
		return
	}

	revision := req.Revision
	if revision == "" {
		revision = "HEAD"
	}
	syncID := uuid.New().String()
	s := Sync{
		SyncID:    syncID,
		OrgID:     orgScope(orgID, userID),
		UserID:    userID,
		OutpostID: outpostID,
		AppName:   name,
		Revision:  revision,
		Status:    SyncPending,
		Active:    true,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Add(ctx); err != nil {
		span.RecordError(err)
		http.Error(w, "failed to create sync", http.StatusInternalServerError)
		return
	}
	if err := enqueueCommand(ctx, outpostID, "argo", "sync", orgID, userID, map[string]any{
		"sync_id":  syncID,
		"app_name": name,
		"revision": revision,
	}); err != nil {
		slog.Error("sync: enqueue command failed", "sync_id", syncID, "error", err)
		if ferr := markSyncFailed(ctx, syncID, "dispatch failed"); ferr != nil {
			slog.Error("sync: failed to mark sync failed", "sync_id", syncID, "error", ferr)
		}
		http.Error(w, "failed to dispatch sync to outpost", http.StatusBadGateway)
		return
	}
	meterSyncsTriggered.Add(ctx, 1)
	span.SetAttributes(attribute.String("sync.id", syncID), attribute.String("app", name))
	span.SetStatus(codes.Ok, "")
	slog.Info("sync triggered", "sync_id", syncID, "app", name, "outpost_id", outpostID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(s) //nolint:errcheck
}

func handleGetSync(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("argo").Start(r.Context(), "handleGetSync")
	defer span.End()
	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getSync", "argo/syncs/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	s, err := getSync(ctx, id)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "sync not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		http.Error(w, "failed to get sync", http.StatusInternalServerError)
		return
	}
	if s.OrgID != orgScope(orgID, userID) {
		http.Error(w, "sync not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s) //nolint:errcheck
}
