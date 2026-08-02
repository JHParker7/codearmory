package main

import (
	"context"
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

// canModifyFieldDef reports whether the caller may update or delete f.
//
// Three ownership classes reach this through ONE route, which is why the decision
// cannot live in conductor — it templates a resource from path params alone and so
// cannot see which class an id belongs to.
//
//   - A board-scoped status column belongs to whoever can access its board.
//   - An org def belongs to its org.
//   - A global seeded def (OrgID == "") belongs to the PLATFORM. Nobody's own grants
//     reach it; it takes a permission on codearmory/tickets/field-defs, i.e. an admin.
//
// That last case used to be `f.OrgID == orgID` for everything non-board-scoped, which
// held for a real org — "" never equals a real org id — but not for a caller with NO
// org, where "" == "" authorized them. Since the global defs ARE the instance-wide
// status and priority catalogs, any org-less user could delete the default statuses and
// break ticket creation platform-wide. The empty string was doing duty both as "the
// platform" and as "this caller has no org", and comparing the two was never a
// statement about ownership.
func canModifyFieldDef(ctx context.Context, bearer, action string, f TicketFieldDef, userID, orgID string) bool {
	if f.BoardID != "" {
		b, err := getBoard(ctx, f.BoardID)
		if err != nil {
			return false
		}
		return canAccessBoard(b, userID, orgID)
	}
	if f.OrgID == "" {
		return checkPlatformPermission(ctx, bearer, action, "field-defs")
	}
	return f.OrgID == orgID
}

type createFieldDefRequest struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Label    string `json:"label"`
	Color    string `json:"color"`
	Position int    `json:"position"`
	// BoardID scopes a status column to a single board. Only valid for
	// kind=status; ignored (must be empty) for priority/timescale.
	BoardID string `json:"board_id"`
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
	// board_id scopes the status columns to a single board (other kinds ignore it).
	boardID := r.URL.Query().Get("board_id")
	defs, err := listFieldDefs(ctx, orgID, kind, boardID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "list field defs: db error", "org_id", orgID, "error", err)
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

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createFieldDef", "tickets/field-defs")
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
	// Status and priority columns can be board-scoped; timescale stays org-wide.
	if req.BoardID != "" && !boardScopedKind(req.Kind) {
		http.Error(w, "only status and priority columns can be board-scoped", http.StatusBadRequest)
		return
	}
	if req.BoardID != "" {
		b, err := getBoard(ctx, req.BoardID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "board not found or not accessible", http.StatusBadRequest)
				return
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "board lookup failed")
			http.Error(w, "failed to create field def", http.StatusInternalServerError)
			return
		}
		if !canAccessBoard(b, userID, orgID) {
			http.Error(w, "board not found or not accessible", http.StatusBadRequest)
			return
		}
	}
	// (kind, value) must be unique within the org and board so a def is identified
	// by its value rather than its UUID (enforced by
	// uq_ticket_field_defs_org_board_kind_value).
	var dupCount int64
	if err := connect().WithContext(ctx).Model(&TicketFieldDef{}).
		Where("org_id = ? AND board_id = ? AND kind = ? AND value = ? AND active = ?", orgID, req.BoardID, req.Kind, req.Value, true).
		Count(&dupCount).Error; err == nil && dupCount > 0 {
		http.Error(w, "a field def with that kind and value already exists", http.StatusConflict)
		return
	}

	now := time.Now().UTC()
	f := TicketFieldDef{
		FieldDefID: uuid.New().String(),
		OrgID:      orgID,
		BoardID:    req.BoardID,
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
		slog.ErrorContext(ctx, "create field def: db error", "org_id", orgID, "error", err)
		http.Error(w, "failed to create field def", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "field def created", "field_def_id", f.FieldDefID, "org_id", orgID, "kind", f.Kind)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(f) //nolint:errcheck
}

func handleUpdateFieldDef(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleUpdateFieldDef")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateFieldDef", "tickets/field-defs/"+id)
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
	if !canModifyFieldDef(ctx, r.Header.Get("Authorization"), "updateFieldDef", f, userID, orgID) {
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
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteFieldDef", "tickets/field-defs/"+id)
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
	// Ownership check (mirrors handleUpdateFieldDef): board-scoped columns require
	// board access, an org def requires an org match, and a global seeded def
	// (OrgID="") requires a platform grant. Deleting one of those removes a default
	// status instance-wide, so it must not be reachable by an ordinary caller — see
	// canModifyFieldDef for why an org comparison alone did not achieve that.
	if !canModifyFieldDef(ctx, r.Header.Get("Authorization"), "deleteFieldDef", f, userID, orgID) {
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
	slog.InfoContext(ctx, "field def deleted", "field_def_id", id, "org_id", orgID)
	w.WriteHeader(http.StatusNoContent)
}
