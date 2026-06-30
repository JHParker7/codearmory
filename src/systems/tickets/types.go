package main

import "time"

const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	StatusResolved   = "resolved"
	StatusClosed     = "closed"
)

const (
	PriorityLow      = "low"
	PriorityMedium   = "medium"
	PriorityHigh     = "high"
	PriorityCritical = "critical"
)

const (
	FieldKindStatus    = "status"
	FieldKindPriority  = "priority"
	FieldKindTimescale = "timescale"
)

// validStatuses / validPriorities are the built-in fallbacks used when an org
// has no custom field defs defined.
var validStatuses   = []string{StatusOpen, StatusInProgress, StatusResolved, StatusClosed}
var validPriorities = []string{PriorityLow, PriorityMedium, PriorityHigh, PriorityCritical}

const maxBodyBytes = 64 * 1024

// Ticket is a task or issue belonging to a user and optionally an org.
// WorkflowID/RunID and ForgeExecutionID are optional references to linked resources
// in other services; they are stored as plain text with no FK enforcement.
type Ticket struct {
	TicketID         string          `json:"ticket_id"          gorm:"column:ticket_id;primaryKey"`
	Title            string          `json:"title"              gorm:"column:title"`
	Description      string          `json:"description"        gorm:"column:description;default:''"`
	Status           string          `json:"status"             gorm:"column:status;default:'open'"`
	Priority         string          `json:"priority"           gorm:"column:priority;default:'medium'"`
	Timescale        string          `json:"timescale"          gorm:"column:timescale;default:''"`
	DueDate          *time.Time      `json:"due_date,omitempty" gorm:"column:due_date"`
	CreatedBy        string          `json:"created_by"         gorm:"column:created_by"`
	OrgID            string          `json:"org_id"             gorm:"column:org_id;default:''"`
	Project          string          `json:"project,omitempty"  gorm:"column:project;default:''"`
	BoardID          *string         `json:"board_id,omitempty" gorm:"column:board_id"`
	AssigneeID       *string         `json:"assignee_id,omitempty"        gorm:"column:assignee_id"`
	WorkflowID       *string         `json:"workflow_id,omitempty"        gorm:"column:workflow_id"`
	RunID            *string         `json:"run_id,omitempty"             gorm:"column:run_id"`
	ForgeExecutionID *string         `json:"forge_execution_id,omitempty" gorm:"column:forge_execution_id"`
	Active           bool            `json:"-"                  gorm:"column:active;default:true"`
	Comments         []TicketComment `json:"comments"           gorm:"-"`
	CreatedAt        time.Time       `json:"created_at"         gorm:"column:created_at"`
	UpdatedAt        time.Time       `json:"updated_at"         gorm:"column:updated_at"`
}

// TableName sets the GORM table name for Ticket.
func (Ticket) TableName() string { return "tickets" }

// TicketComment is a message attached to a Ticket.
type TicketComment struct {
	CommentID string    `json:"comment_id" gorm:"column:comment_id;primaryKey"`
	TicketID  string    `json:"ticket_id"  gorm:"column:ticket_id"`
	AuthorID  string    `json:"author_id"  gorm:"column:author_id"`
	Body      string    `json:"body"       gorm:"column:body"`
	Active    bool      `json:"-"          gorm:"column:active;default:true"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

// TableName sets the GORM table name for TicketComment.
func (TicketComment) TableName() string { return "ticket_comments" }

// Board is a named grouping of tickets (a kanban board) owned by a user and
// optionally shared within an org. Tickets reference a board via Ticket.BoardID;
// the board's columns are the org's configured status field defs.
type Board struct {
	BoardID     string    `json:"board_id"     gorm:"column:board_id;primaryKey"`
	Name        string    `json:"name"         gorm:"column:name"`
	Description string    `json:"description"  gorm:"column:description;default:''"`
	Color       string    `json:"color"        gorm:"column:color;default:''"`
	Position    int       `json:"position"     gorm:"column:position;default:0"`
	CreatedBy   string    `json:"created_by"   gorm:"column:created_by"`
	OrgID       string    `json:"org_id"       gorm:"column:org_id;default:''"`
	Active      bool      `json:"-"            gorm:"column:active;default:true"`
	CreatedAt   time.Time `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt   time.Time `json:"updated_at"   gorm:"column:updated_at"`
}

// TableName sets the GORM table name for Board.
func (Board) TableName() string { return "ticket_boards" }

// TicketFieldDef defines a custom status, priority, or timescale value.
// OrgID="" means it is a system-wide default visible to all orgs.
type TicketFieldDef struct {
	FieldDefID string    `json:"field_def_id" gorm:"column:field_def_id;primaryKey"`
	OrgID      string    `json:"org_id"       gorm:"column:org_id;default:''"`
	Kind       string    `json:"kind"         gorm:"column:kind"`
	Value      string    `json:"value"        gorm:"column:value"`
	Label      string    `json:"label"        gorm:"column:label"`
	Color      string    `json:"color"        gorm:"column:color;default:''"`
	Position   int       `json:"position"     gorm:"column:position;default:0"`
	Active     bool      `json:"-"            gorm:"column:active;default:true"`
	CreatedAt  time.Time `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt  time.Time `json:"updated_at"   gorm:"column:updated_at"`
}

// TableName sets the GORM table name for TicketFieldDef.
func (TicketFieldDef) TableName() string { return "ticket_field_defs" }
