package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// db is the common interface implemented by all persistent entities.
type db interface {
	Add(ctx context.Context) error
	Update(ctx context.Context) error
	Remove(ctx context.Context) error
	Get(ctx context.Context) (db, error)
	List(ctx context.Context, limit, offset int) ([]db, error)
}

var gormDB *gorm.DB
var gormDBRead *gorm.DB
var dbInitMu sync.Mutex

func connect() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDB != nil {
		return gormDB
	}
	conn, err := gorm.Open(postgres.Open(secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/tickets")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to database", "error", err)
		os.Exit(1)
	}
	gormDB = conn
	return gormDB
}

func connectRead() *gorm.DB {
	dbInitMu.Lock()
	defer dbInitMu.Unlock()
	if gormDBRead != nil {
		return gormDBRead
	}
	if gormDB != nil {
		return gormDB
	}
	readURL := secret("DATABASE_READ_URL")
	if readURL == "" {
		readURL = secretOrDefault("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/tickets")
	}
	conn, err := gorm.Open(postgres.Open(readURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		slog.Error("unable to connect to read database", "error", err)
		os.Exit(1)
	}
	gormDBRead = conn
	return gormDBRead
}

// ── Ticket ────────────────────────────────────────────────────────────────────

func (t Ticket) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket.add")
	defer span.End()
	span.SetAttributes(attribute.String("ticket.id", t.TicketID))
	if err := connect().WithContext(ctx).Create(&t).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (t Ticket) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket.update")
	defer span.End()
	span.SetAttributes(attribute.String("ticket.id", t.TicketID))
	t.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&t).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (t Ticket) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket.remove")
	defer span.End()
	span.SetAttributes(attribute.String("ticket.id", t.TicketID))
	if err := connect().WithContext(ctx).Model(&Ticket{}).
		Where("ticket_id=? AND active=?", t.TicketID, true).
		Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (t Ticket) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket.get")
	defer span.End()
	span.SetAttributes(attribute.String("ticket.id", t.TicketID))
	var result Ticket
	if err := connectRead().WithContext(ctx).Where("ticket_id=? AND active=?", t.TicketID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	result.Comments = []TicketComment{}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (t Ticket) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket.list")
	defer span.End()
	var tickets []Ticket
	t.Active = true
	q := connectRead().WithContext(ctx).Where(t)
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&tickets).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(tickets))
	for i, tk := range tickets {
		result[i] = tk
	}
	return result, nil
}

// ── Board ─────────────────────────────────────────────────────────────────────

func (b Board) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.board.add")
	defer span.End()
	span.SetAttributes(attribute.String("board.id", b.BoardID))
	if err := connect().WithContext(ctx).Create(&b).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (b Board) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.board.update")
	defer span.End()
	span.SetAttributes(attribute.String("board.id", b.BoardID))
	b.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&b).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (b Board) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.board.remove")
	defer span.End()
	span.SetAttributes(attribute.String("board.id", b.BoardID))
	if err := connect().WithContext(ctx).Model(&Board{}).
		Where("board_id=? AND active=?", b.BoardID, true).
		Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (b Board) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.board.get")
	defer span.End()
	span.SetAttributes(attribute.String("board.id", b.BoardID))
	var result Board
	if err := connectRead().WithContext(ctx).Where("board_id=? AND active=?", b.BoardID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (b Board) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.board.list")
	defer span.End()
	boards, err := listBoards(ctx, b.CreatedBy, b.OrgID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if limit > 0 {
		if offset >= len(boards) {
			boards = nil
		} else {
			end := offset + limit
			if end > len(boards) {
				end = len(boards)
			}
			boards = boards[offset:end]
		}
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(boards))
	for i, bd := range boards {
		result[i] = bd
	}
	return result, nil
}

// getBoard returns a single active board by ID.
func getBoard(ctx context.Context, id string) (Board, error) {
	row, err := (Board{BoardID: id}).Get(ctx)
	if err != nil {
		return Board{}, err
	}
	return row.(Board), nil
}

// listBoards returns active boards accessible to the caller (owned or same-org),
// ordered by position then name.
func listBoards(ctx context.Context, userID, orgID string) ([]Board, error) {
	var boards []Board
	if err := connectRead().WithContext(ctx).
		Where("active = ? AND (created_by = ? OR (org_id != '' AND org_id = ?))", true, userID, orgID).
		Order("position, name, created_at").
		Find(&boards).Error; err != nil {
		return nil, err
	}
	if boards == nil {
		boards = []Board{}
	}
	return boards, nil
}

// boardNameTaken reports whether an active board with the same name already
// exists in the caller's scope (org-shared when org-backed, else per-user),
// excluding the board with excludeID (empty to check all).
func boardNameTaken(ctx context.Context, name, userID, orgID, excludeID string) (bool, error) {
	q := connectRead().WithContext(ctx).Model(&Board{}).Where("active = ? AND name = ?", true, name)
	if orgID != "" {
		q = q.Where("org_id = ?", orgID)
	} else {
		q = q.Where("org_id = '' AND created_by = ?", userID)
	}
	if excludeID != "" {
		q = q.Where("board_id <> ?", excludeID)
	}
	var count int64
	if err := q.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// deleteBoardTickets soft-deletes every active ticket on boardID. Boards are
// required, so deleting a board cascade-deletes its tickets rather than orphaning
// them to a board-less "unassigned" pile.
func deleteBoardTickets(ctx context.Context, boardID string) error {
	return connect().WithContext(ctx).Model(&Ticket{}).
		Where("board_id = ? AND active = ?", boardID, true).
		Update("active", false).Error
}

// defaultBoardName is the name of the per-scope board a ticket lands on when the
// caller creates it without naming a board. Every ticket must belong to a board.
const defaultBoardName = "Default"

// findDefaultBoard looks up the caller's existing default board within its owner
// scope (shared per-org for org-backed callers, else per-user), returning
// gorm.ErrRecordNotFound when it has not been created yet.
func findDefaultBoard(ctx context.Context, userID, orgID string) (Board, error) {
	q := connectRead().WithContext(ctx).Where("active = ? AND name = ?", true, defaultBoardName)
	if orgID != "" {
		q = q.Where("org_id = ?", orgID)
	} else {
		q = q.Where("org_id = '' AND created_by = ?", userID)
	}
	var b Board
	err := q.First(&b).Error
	return b, err
}

// getOrCreateDefaultBoard returns the caller's default board, creating it (and
// seeding its own status columns) on first use. It is the board a ticket is
// placed on when the caller does not specify one, so the "every ticket has a
// board" invariant holds without every client having to pick a board.
func getOrCreateDefaultBoard(ctx context.Context, userID, orgID string) (Board, error) {
	existing, err := findDefaultBoard(ctx, userID, orgID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Board{}, err
	}

	now := time.Now().UTC()
	b := Board{
		BoardID:     uuid.New().String(),
		Name:        defaultBoardName,
		Description: "Default board",
		CreatedBy:   userID,
		OrgID:       orgID,
		Active:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := b.Add(ctx); err != nil {
		// A concurrent create may have won the race against the unique name
		// index; fall back to reading the board it inserted.
		if raced, rerr := findDefaultBoard(ctx, userID, orgID); rerr == nil {
			return raced, nil
		}
		return Board{}, err
	}
	// Best-effort, matching handleCreateBoard: on failure the board falls back to
	// the org/global status set rather than failing ticket creation.
	if err := seedBoardStatuses(ctx, b); err != nil {
		slog.WarnContext(ctx, "default board: seed status columns failed", "board_id", b.BoardID, "error", err)
	}
	return b, nil
}

// backfillTicketBoards assigns any pre-existing board-less tickets to their
// scope's default board, so the "every ticket has a board" invariant also holds
// for rows created before boards were required. Idempotent: once every ticket
// has a board it is a no-op.
func backfillTicketBoards(ctx context.Context) error {
	type scope struct {
		CreatedBy string
		OrgID     string
	}
	var scopes []scope
	if err := connect().WithContext(ctx).Model(&Ticket{}).
		Select("created_by, org_id").
		Where("active = ? AND (board_id IS NULL OR board_id = '')", true).
		Group("created_by, org_id").
		Scan(&scopes).Error; err != nil {
		return err
	}
	for _, s := range scopes {
		b, err := getOrCreateDefaultBoard(ctx, s.CreatedBy, s.OrgID)
		if err != nil {
			return err
		}
		if err := connect().WithContext(ctx).Model(&Ticket{}).
			Where("active = ? AND (board_id IS NULL OR board_id = '') AND created_by = ? AND org_id = ?", true, s.CreatedBy, s.OrgID).
			Update("board_id", b.BoardID).Error; err != nil {
			return err
		}
	}
	return nil
}

// getTicket returns a single active ticket by ID with an empty comments slice.
func getTicket(ctx context.Context, id string) (Ticket, error) {
	row, err := (Ticket{TicketID: id}).Get(ctx)
	if err != nil {
		return Ticket{}, err
	}
	return row.(Ticket), nil
}

// listTickets returns active tickets accessible to the caller with optional filters.
// projectFilter/boardFilter are view filters only — they never widen access beyond
// the created_by/org_id scope above. A boardFilter of "none" selects unassigned
// tickets (no board).
func listTickets(ctx context.Context, userID, orgID, statusFilter, priorityFilter, assigneeFilter, timescaleFilter, projectFilter, boardFilter string) ([]Ticket, error) {
	q := connectRead().WithContext(ctx).
		Where("active = ? AND (created_by = ? OR (org_id != '' AND org_id = ?))", true, userID, orgID)
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	if priorityFilter != "" {
		q = q.Where("priority = ?", priorityFilter)
	}
	if assigneeFilter != "" {
		q = q.Where("assignee_id = ?", assigneeFilter)
	}
	if timescaleFilter != "" {
		q = q.Where("timescale = ?", timescaleFilter)
	}
	if projectFilter != "" {
		q = q.Where("project = ?", projectFilter)
	}
	if boardFilter == "none" {
		q = q.Where("board_id IS NULL OR board_id = ''")
	} else if boardFilter != "" {
		q = q.Where("board_id = ?", boardFilter)
	}
	var tickets []Ticket
	if err := q.Order("created_at DESC").Limit(100).Find(&tickets).Error; err != nil {
		return nil, err
	}
	for i := range tickets {
		tickets[i].Comments = []TicketComment{}
	}
	if tickets == nil {
		tickets = []Ticket{}
	}
	return tickets, nil
}

// ── TicketComment ─────────────────────────────────────────────────────────────

func (c TicketComment) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket_comment.add")
	defer span.End()
	span.SetAttributes(attribute.String("comment.id", c.CommentID))
	if err := connect().WithContext(ctx).Create(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c TicketComment) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket_comment.update")
	defer span.End()
	span.SetAttributes(attribute.String("comment.id", c.CommentID))
	c.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&c).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c TicketComment) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket_comment.remove")
	defer span.End()
	span.SetAttributes(attribute.String("comment.id", c.CommentID))
	if err := connect().WithContext(ctx).Model(&TicketComment{}).
		Where("comment_id=? AND active=?", c.CommentID, true).
		Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (c TicketComment) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket_comment.get")
	defer span.End()
	span.SetAttributes(attribute.String("comment.id", c.CommentID))
	var result TicketComment
	if err := connectRead().WithContext(ctx).Where("comment_id=? AND active=?", c.CommentID, true).First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (c TicketComment) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.ticket_comment.list")
	defer span.End()
	var comments []TicketComment
	c.Active = true
	q := connectRead().WithContext(ctx).Where(c).Order("created_at")
	if limit > 0 {
		q = q.Limit(limit).Offset(offset)
	}
	if err := q.Find(&comments).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(comments))
	for i, cm := range comments {
		result[i] = cm
	}
	return result, nil
}

// getComment returns a single active comment by ID and ticket ID.
func getComment(ctx context.Context, commentID, ticketID string) (TicketComment, error) {
	var c TicketComment
	if err := connectRead().WithContext(ctx).
		Where("comment_id=? AND ticket_id=? AND active=?", commentID, ticketID, true).
		First(&c).Error; err != nil {
		return TicketComment{}, err
	}
	return c, nil
}

// listComments returns active comments for a ticket, ordered by creation time.
func listComments(ctx context.Context, ticketID string) ([]TicketComment, error) {
	var comments []TicketComment
	if err := connectRead().WithContext(ctx).
		Where("ticket_id=? AND active=?", ticketID, true).
		Order("created_at").
		Find(&comments).Error; err != nil {
		return nil, err
	}
	if comments == nil {
		comments = []TicketComment{}
	}
	return comments, nil
}

// refreshComment re-fetches a comment from the DB to pick up server-set timestamps.
func refreshComment(ctx context.Context, commentID string) (TicketComment, error) {
	var c TicketComment
	if err := connectRead().WithContext(ctx).Where("comment_id=?", commentID).First(&c).Error; err != nil {
		return TicketComment{}, err
	}
	return c, nil
}

// isNotFound returns true when err is a GORM record-not-found error.
func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// ── TicketFieldDef ────────────────────────────────────────────────────────────

func (f TicketFieldDef) Add(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.field_def.add")
	defer span.End()
	if err := connect().WithContext(ctx).Create(&f).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (f TicketFieldDef) Update(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.field_def.update")
	defer span.End()
	f.UpdatedAt = time.Now().UTC()
	if err := connect().WithContext(ctx).Save(&f).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (f TicketFieldDef) Remove(ctx context.Context) error {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.field_def.remove")
	defer span.End()
	if err := connect().WithContext(ctx).Model(&TicketFieldDef{}).
		Where("field_def_id=? AND active=?", f.FieldDefID, true).
		Update("active", false).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (f TicketFieldDef) Get(ctx context.Context) (db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.field_def.get")
	defer span.End()
	var result TicketFieldDef
	if err := connectRead().WithContext(ctx).
		Where("field_def_id=? AND active=?", f.FieldDefID, true).
		First(&result).Error; err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (f TicketFieldDef) List(ctx context.Context, limit, offset int) ([]db, error) {
	ctx, span := otel.Tracer("tickets").Start(ctx, "db.field_def.list")
	defer span.End()
	defs, err := listFieldDefs(ctx, f.OrgID, f.Kind, f.BoardID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if limit > 0 {
		if offset >= len(defs) {
			defs = nil
		} else {
			end := offset + limit
			if end > len(defs) {
				end = len(defs)
			}
			defs = defs[offset:end]
		}
	}
	span.SetStatus(codes.Ok, "")
	result := make([]db, len(defs))
	for i, d := range defs {
		result[i] = d
	}
	return result, nil
}

// queryFieldDefs returns active field defs at an exact board scope visible to
// orgID: system defaults (OrgID=”) plus org-specific ones, ordered by position.
// Org-specific defs override global ones with the same Kind+Value. boardID is
// matched exactly (” = the org/global level).
func queryFieldDefs(ctx context.Context, orgID, kind, boardID string) ([]TicketFieldDef, error) {
	q := connectRead().WithContext(ctx).
		Where("active = ? AND board_id = ? AND (org_id = '' OR org_id = ?)", true, boardID, orgID)
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	// Org-specific first so dedup keeps them over globals.
	var defs []TicketFieldDef
	if err := q.Order("CASE WHEN org_id = '' THEN 1 ELSE 0 END, position, created_at").Find(&defs).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(defs))
	deduped := make([]TicketFieldDef, 0, len(defs))
	for _, d := range defs {
		key := d.Kind + ":" + d.Value
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, d)
		}
	}
	return deduped, nil
}

// listFieldDefs returns the field defs visible to orgID in the context of
// boardID. Status columns are board-scoped: a board's own status defs fully
// replace the org/global set, falling back to org/global when the board has
// configured none (or boardID==""). Priority/timescale defs are always
// org/global. Results are ordered by position within each kind.
func listFieldDefs(ctx context.Context, orgID, kind, boardID string) ([]TicketFieldDef, error) {
	// Non-status kinds are never board-scoped.
	if kind != "" && kind != FieldKindStatus {
		return queryFieldDefs(ctx, orgID, kind, "")
	}

	// Resolve the effective status set: the board's own columns, else org/global.
	var statuses []TicketFieldDef
	if boardID != "" {
		own, err := queryFieldDefs(ctx, orgID, FieldKindStatus, boardID)
		if err != nil {
			return nil, err
		}
		statuses = own
	}
	if len(statuses) == 0 {
		base, err := queryFieldDefs(ctx, orgID, FieldKindStatus, "")
		if err != nil {
			return nil, err
		}
		statuses = base
	}
	if kind == FieldKindStatus {
		return statuses, nil
	}

	// kind == "": all kinds — board-scoped statuses plus org/global others.
	others, err := queryFieldDefs(ctx, orgID, "", "")
	if err != nil {
		return nil, err
	}
	out := statuses
	for _, d := range others {
		if d.Kind != FieldKindStatus {
			out = append(out, d)
		}
	}
	return out, nil
}

// getFieldDefValues returns distinct valid values for a kind visible to orgID
// in the context of boardID (only status is board-scoped), ordered by position
// so the first element is the left-most column. Falls back to the provided
// defaults if the DB pool is uninitialised or returns nothing.
func getFieldDefValues(ctx context.Context, orgID, kind, boardID string, fallback []string) []string {
	// Skip the DB call entirely when no connection pool has been opened yet
	// (e.g. unit-test processes that never call connect()).
	dbInitMu.Lock()
	uninit := gormDB == nil && gormDBRead == nil
	dbInitMu.Unlock()
	if uninit {
		return fallback
	}

	defs, err := listFieldDefs(ctx, orgID, kind, boardID)
	if err != nil || len(defs) == 0 {
		return fallback
	}
	seen := map[string]bool{}
	var values []string
	for _, d := range defs {
		if !seen[d.Value] {
			seen[d.Value] = true
			values = append(values, d.Value)
		}
	}
	return values
}

// builtinStatusDefs returns the hard-coded default status columns, used to seed
// a board when the org/global status set is somehow empty.
func builtinStatusDefs() []TicketFieldDef {
	return []TicketFieldDef{
		{Kind: FieldKindStatus, Value: StatusOpen, Label: "Open", Position: 0},
		{Kind: FieldKindStatus, Value: StatusInProgress, Label: "In Progress", Position: 1},
		{Kind: FieldKindStatus, Value: StatusResolved, Label: "Resolved", Position: 2},
		{Kind: FieldKindStatus, Value: StatusClosed, Label: "Closed", Position: 3},
	}
}

// seedBoardStatuses gives a freshly-created board its own copy of the effective
// org/global status columns so its columns can be edited independently of other
// boards. Best-effort: a failure leaves the board falling back to org/global
// statuses, so callers log rather than fail board creation.
func seedBoardStatuses(ctx context.Context, b Board) error {
	base, err := queryFieldDefs(ctx, b.OrgID, FieldKindStatus, "")
	if err != nil {
		return err
	}
	if len(base) == 0 {
		base = builtinStatusDefs()
	}
	now := time.Now().UTC()
	defs := make([]TicketFieldDef, 0, len(base))
	for i, s := range base {
		defs = append(defs, TicketFieldDef{
			FieldDefID: uuid.New().String(),
			OrgID:      b.OrgID,
			BoardID:    b.BoardID,
			Kind:       FieldKindStatus,
			Value:      s.Value,
			Label:      s.Label,
			Color:      s.Color,
			Position:   i,
			Active:     true,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}
	if len(defs) == 0 {
		return nil
	}
	return connect().WithContext(ctx).Create(&defs).Error
}

// seedDefaultFieldDefs inserts global system defaults for each kind
// independently, so adding a new kind doesn't skip seeding existing ones.
func seedDefaultFieldDefs(ctx context.Context) error {
	now := time.Now().UTC()
	type kindEntry struct {
		kind     string
		defaults []TicketFieldDef
	}
	entries := []kindEntry{
		{
			kind: FieldKindStatus,
			defaults: []TicketFieldDef{
				{FieldDefID: uuid.New().String(), Kind: FieldKindStatus, Value: StatusOpen, Label: "Open", Position: 0, Active: true, CreatedAt: now, UpdatedAt: now},
				{FieldDefID: uuid.New().String(), Kind: FieldKindStatus, Value: StatusInProgress, Label: "In Progress", Position: 1, Active: true, CreatedAt: now, UpdatedAt: now},
				{FieldDefID: uuid.New().String(), Kind: FieldKindStatus, Value: StatusResolved, Label: "Resolved", Position: 2, Active: true, CreatedAt: now, UpdatedAt: now},
				{FieldDefID: uuid.New().String(), Kind: FieldKindStatus, Value: StatusClosed, Label: "Closed", Position: 3, Active: true, CreatedAt: now, UpdatedAt: now},
			},
		},
		{
			kind: FieldKindPriority,
			defaults: []TicketFieldDef{
				{FieldDefID: uuid.New().String(), Kind: FieldKindPriority, Value: PriorityLow, Label: "Low", Color: "#4a5346", Position: 0, Active: true, CreatedAt: now, UpdatedAt: now},
				{FieldDefID: uuid.New().String(), Kind: FieldKindPriority, Value: PriorityMedium, Label: "Medium", Color: "#7d8a78", Position: 1, Active: true, CreatedAt: now, UpdatedAt: now},
				{FieldDefID: uuid.New().String(), Kind: FieldKindPriority, Value: PriorityHigh, Label: "High", Color: "#c9b060", Position: 2, Active: true, CreatedAt: now, UpdatedAt: now},
				{FieldDefID: uuid.New().String(), Kind: FieldKindPriority, Value: PriorityCritical, Label: "Critical", Color: "#d46b55", Position: 3, Active: true, CreatedAt: now, UpdatedAt: now},
			},
		},
	}
	for _, e := range entries {
		var count int64
		if err := connect().WithContext(ctx).Model(&TicketFieldDef{}).
			Where("org_id = '' AND kind = ? AND active = ?", e.kind, true).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if err := connect().WithContext(ctx).Create(&e.defaults).Error; err != nil {
			return err
		}
	}
	return nil
}
