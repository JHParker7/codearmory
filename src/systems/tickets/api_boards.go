package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"
)

// canAccessBoard returns true when the caller owns the board or shares its org.
func canAccessBoard(b Board, userID, orgID string) bool {
	return b.CreatedBy == userID || (orgID != "" && b.OrgID == orgID)
}

type createBoardRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	Position    int    `json:"position"`
}

type updateBoardRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Color       *string `json:"color"`
	Position    *int    `json:"position"`
}

func handleCreateBoard(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleCreateBoard")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "createBoard", "tickets/boards")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("org.id", orgID))

	var req createBoardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	taken, err := boardNameTaken(ctx, req.Name, userID, orgID, "")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "create board: dup check", "user_id", userID, "error", err)
		http.Error(w, "failed to create board", http.StatusInternalServerError)
		return
	}
	if taken {
		http.Error(w, "a board with that name already exists", http.StatusConflict)
		return
	}

	now := time.Now().UTC()
	b := Board{
		BoardID:     uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		Color:       req.Color,
		Position:    req.Position,
		CreatedBy:   userID,
		OrgID:       orgID,
		Active:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := b.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create board: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to create board", http.StatusInternalServerError)
		return
	}

	span.SetAttributes(attribute.String("board.id", b.BoardID))
	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "board created", "board_id", b.BoardID, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(b) //nolint:errcheck
}

func handleListBoards(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleListBoards")
	defer span.End()

	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "listBoard", "tickets/boards")
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("org.id", orgID))

	boards, err := listBoards(ctx, userID, orgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db query failed")
		slog.ErrorContext(ctx, "list boards: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to list boards", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(boards) //nolint:errcheck
}

func handleGetBoard(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleGetBoard")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "getBoard", "tickets/boards/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("org.id", orgID))

	b, err := getBoard(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "board not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "get board: db error", "board_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get board", http.StatusInternalServerError)
		return
	}
	if !canAccessBoard(b, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "board not found", http.StatusNotFound)
		return
	}

	span.SetStatus(codes.Ok, "")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(b) //nolint:errcheck
}

func handleUpdateBoard(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleUpdateBoard")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "updateBoard", "tickets/boards/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("org.id", orgID))

	existing, err := getBoard(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "board not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "update board: fetch error", "board_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to get board", http.StatusInternalServerError)
		return
	}
	if !canAccessBoard(existing, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "board not found", http.StatusNotFound)
		return
	}

	var req updateBoardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	// Partial update: keep the current value for any omitted field.
	if req.Name != "" && req.Name != existing.Name {
		taken, err := boardNameTaken(ctx, req.Name, existing.CreatedBy, existing.OrgID, existing.BoardID)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "db error")
			http.Error(w, "failed to update board", http.StatusInternalServerError)
			return
		}
		if taken {
			http.Error(w, "a board with that name already exists", http.StatusConflict)
			return
		}
		existing.Name = req.Name
	}
	if req.Description != nil {
		existing.Description = *req.Description
	}
	if req.Color != nil {
		existing.Color = *req.Color
	}
	if req.Position != nil {
		existing.Position = *req.Position
	}

	if err := existing.Update(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db update failed")
		slog.ErrorContext(ctx, "update board: db error", "board_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to update board", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "board updated", "board_id", id, "user_id", userID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing) //nolint:errcheck
}

func handleDeleteBoard(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("tickets").Start(r.Context(), "handleDeleteBoard")
	defer span.End()

	id := r.PathValue("id")
	userID, orgID, ok := gatekeeperClient.CheckPermissions(ctx, w, r, "deleteBoard", "tickets/boards/"+id)
	if !ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetAttributes(attribute.String("user.id", userID), attribute.String("org.id", orgID))

	b, err := getBoard(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			span.SetStatus(codes.Ok, "")
			http.Error(w, "board not found", http.StatusNotFound)
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete board: fetch error", "board_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete board", http.StatusInternalServerError)
		return
	}
	if !canAccessBoard(b, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "board not found", http.StatusNotFound)
		return
	}

	// Orphan the board's tickets (board_id → NULL) before soft-deleting the board
	// so they remain accessible under the "unassigned" pile.
	if err := unassignBoardTickets(ctx, id); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete board: unassign tickets", "board_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete board", http.StatusInternalServerError)
		return
	}
	if err := b.Remove(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete board: db error", "board_id", id, "user_id", userID, "error", err)
		http.Error(w, "failed to delete board", http.StatusInternalServerError)
		return
	}

	span.SetStatus(codes.Ok, "")
	slog.InfoContext(ctx, "board deleted", "board_id", id, "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
