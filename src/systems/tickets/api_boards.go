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
	Project     string `json:"project,omitempty"`
}

type updateBoardRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Color       *string `json:"color"`
	Position    *int    `json:"position"`
	Project     *string `json:"project,omitempty"`
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
		Project:     req.Project,
		Active:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// If Project names a real gatekeeper project the caller can reach, file the board
	// into it — but only if the caller may create within it (developer/admin/owner). A
	// slug that resolves to nothing stays a free-text label (unchanged behaviour); a
	// slug the caller may only view is refused rather than silently downgraded.
	if req.Project != "" {
		bearer := r.Header.Get("Authorization")
		if p := resolveProjectSlug(ctx, bearer, req.Project); p != nil {
			if !checkProjectPermission(ctx, bearer, "createBoard", p.Namespace, "boards", p.Slug, "") {
				span.SetStatus(codes.Ok, "")
				http.Error(w, "you cannot create boards in project "+p.Slug, http.StatusForbidden)
				return
			}
			b.ProjectID = p.ProjectID
			b.ProjectNamespace = p.Namespace
		}
	}

	if err := b.Add(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		slog.ErrorContext(ctx, "create board: db error", "user_id", userID, "error", err)
		http.Error(w, "failed to create board", http.StatusInternalServerError)
		return
	}

	// Give the board its own copy of the status columns so they can be edited
	// independently. Best-effort: on failure the board falls back to the
	// org/global status set, so this never blocks board creation.
	if err := seedBoardFieldDefs(ctx, b); err != nil {
		span.RecordError(err)
		slog.WarnContext(ctx, "create board: seed status columns failed", "board_id", b.BoardID, "user_id", userID, "error", err)
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

	boards, err := listBoards(ctx, userID, orgID, accessibleProjectIDs(ctx, r.Header.Get("Authorization")))
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
	if !authorizeBoard(ctx, r.Header.Get("Authorization"), "getBoard", b, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "board not found", http.StatusNotFound)
		return
	}

	// Decorate with the board's open/total ticket counts (best-effort — see
	// attachBoardCounts).
	boards := []Board{b}
	attachBoardCounts(ctx, userID, orgID, boards)
	b = boards[0]

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
	if !authorizeBoard(ctx, r.Header.Get("Authorization"), "updateBoard", existing, userID, orgID) {
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
	// Guard like tickets: a partial PUT that omits project must not silently wipe the
	// stored label. When project is sent, re-resolve membership: if the (possibly new)
	// slug names a real project the caller may write to, file it there; otherwise it
	// reverts to a plain label (clear the ids so a moved board never keeps stale scope).
	if req.Project != nil {
		existing.Project = *req.Project
		existing.ProjectID, existing.ProjectNamespace = "", ""
		bearer := r.Header.Get("Authorization")
		if p := resolveProjectSlug(ctx, bearer, existing.Project); p != nil &&
			checkProjectPermission(ctx, bearer, "updateBoard", p.Namespace, "boards", p.Slug, "") {
			existing.ProjectID = p.ProjectID
			existing.ProjectNamespace = p.Namespace
		}
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
	if !authorizeBoard(ctx, r.Header.Get("Authorization"), "deleteBoard", b, userID, orgID) {
		span.SetStatus(codes.Ok, "")
		http.Error(w, "board not found", http.StatusNotFound)
		return
	}

	// Deleting a board cascade-deletes its tickets, so require the caller to
	// re-type the board's exact name as confirmation (via ?confirm=<name>).
	if r.URL.Query().Get("confirm") != b.Name {
		http.Error(w, "confirmation required: pass ?confirm=<board name> matching the board's name to delete it and its tickets", http.StatusBadRequest)
		return
	}

	// Boards are required, so a board's tickets are cascade-deleted with it
	// rather than orphaned to a board-less "unassigned" pile.
	if err := deleteBoardTickets(ctx, id); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db error")
		slog.ErrorContext(ctx, "delete board: delete tickets", "board_id", id, "user_id", userID, "error", err)
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
