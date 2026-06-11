package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

var validFieldKinds = []string{FieldKindStatus, FieldKindPriority, FieldKindTimescale}

type createFieldDefRequest struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Label    string `json:"label"`
	Color    string `json:"color"`
	Position int    `json:"position"`
}

type updateFieldDefRequest struct {
	Label    string  `json:"label"`
	Color    *string `json:"color"`
	Position *int    `json:"position"`
}

func handleListFieldDefs(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleListFieldDefs")
	defer span.End()

	_, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listFieldDef", "tickets/field-defs")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("org.id", orgID))

	kind := r.URL.Query().Get("kind")
	if kind != "" && !slices.Contains(validFieldKinds, kind) {
		http.Error(w, "kind must be one of: status, priority, timescale", http.StatusBadRequest)
		return
	}
	defs, err := listFieldDefs(ctx, orgID, kind)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.Error("list field defs: db error", "org_id", orgID, "error", err)
		http.Error(w, "failed to list field defs", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(defs) //nolint:errcheck
}

func handleCreateFieldDef(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleCreateFieldDef")
	defer span.End()

	_, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createFieldDef", "tickets/field-defs")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("org.id", orgID))

	var req createFieldDefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !slices.Contains(validFieldKinds, req.Kind) {
		http.Error(w, "kind must be one of: status, priority, timescale", http.StatusBadRequest)
		return
	}
	if req.Value == "" {
		http.Error(w, "value is required", http.StatusBadRequest)
		return
	}
	if req.Label == "" {
		http.Error(w, "label is required", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	f := TicketFieldDef{
		FieldDefID: uuid.New().String(),
		OrgID:      orgID,
		Kind:       req.Kind,
		Value:      req.Value,
		Label:      req.Label,
		Color:      req.Color,
		Position:   req.Position,
		Active:     true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := f.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.Error("create field def: db error", "org_id", orgID, "error", err)
		http.Error(w, "failed to create field def", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("field def created", "field_def_id", f.FieldDefID, "org_id", orgID, "kind", f.Kind, "value", f.Value)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(f) //nolint:errcheck
}

func handleUpdateFieldDef(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleUpdateFieldDef")
	defer span.End()

	id := r.PathValue("id")
	_, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateFieldDef", "tickets/field-defs/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("org.id", orgID))

	row, err := (TicketFieldDef{FieldDefID: id}).Get(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "field def not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to get field def", http.StatusInternalServerError)
		return
	}
	f := row.(TicketFieldDef)
	if f.OrgID != orgID {
		http.Error(w, "field def not found", http.StatusNotFound)
		return
	}

	var req updateFieldDefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Label != "" {
		f.Label = req.Label
	}
	if req.Color != nil {
		f.Color = *req.Color
	}
	if req.Position != nil {
		f.Position = *req.Position
	}

	if err := f.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		http.Error(w, "failed to update field def", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(f) //nolint:errcheck
}

func handleDeleteFieldDef(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleDeleteFieldDef")
	defer span.End()

	id := r.PathValue("id")
	_, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteFieldDef", "tickets/field-defs/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("org.id", orgID))

	row, err := (TicketFieldDef{FieldDefID: id}).Get(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "field def not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to get field def", http.StatusInternalServerError)
		return
	}
	f := row.(TicketFieldDef)
	if f.OrgID != "" && f.OrgID != orgID {
		http.Error(w, "field def not found", http.StatusNotFound)
		return
	}

	if err := f.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		http.Error(w, "failed to delete field def", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.Info("field def deleted", "field_def_id", id, "org_id", orgID)
	w.WriteHeader(http.StatusNoContent)
}
