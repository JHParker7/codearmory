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

// getTicket returns a single active ticket by ID with an empty comments slice.
func getTicket(ctx context.Context, id string) (Ticket, error) {
	row, err := (Ticket{TicketID: id}).Get(ctx)
	if err != nil {
		return Ticket{}, err
	}
	return row.(Ticket), nil
}

// listTickets returns active tickets accessible to the caller with optional filters.
// projectFilter is a view filter only — it never widens access beyond the
// created_by/org_id scope above.
func listTickets(ctx context.Context, userID, orgID, statusFilter, priorityFilter, assigneeFilter, timescaleFilter, projectFilter string) ([]Ticket, error) {
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
	defs, err := listFieldDefs(ctx, f.OrgID, f.Kind)
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

// listFieldDefs returns active field defs visible to orgID: system defaults
// (OrgID='') plus org-specific ones, ordered by position. Org-specific defs
// override global ones with the same Kind+Value.
func listFieldDefs(ctx context.Context, orgID, kind string) ([]TicketFieldDef, error) {
	q := connectRead().WithContext(ctx).
		Where("active = ? AND (org_id = '' OR org_id = ?)", true, orgID)
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

// getFieldDefValues returns distinct valid values for a kind visible to orgID.
// Falls back to the provided defaults if the DB pool is uninitialised or returns nothing.
func getFieldDefValues(ctx context.Context, orgID, kind string, fallback []string) []string {
	// Skip the DB call entirely when no connection pool has been opened yet
	// (e.g. unit-test processes that never call connect()).
	dbInitMu.Lock()
	uninit := gormDB == nil && gormDBRead == nil
	dbInitMu.Unlock()
	if uninit {
		return fallback
	}

	defs, err := listFieldDefs(ctx, orgID, kind)
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
