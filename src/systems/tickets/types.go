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
	CreatedBy        string          `json:"created_by"         gorm:"column:created_by"`
	OrgID            string          `json:"org_id"             gorm:"column:org_id;default:''"`
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
