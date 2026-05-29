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
	TicketID         string          `json:"ticket_id"`
	Title            string          `json:"title"`
	Description      string          `json:"description"`
	Status           string          `json:"status"`
	Priority         string          `json:"priority"`
	CreatedBy        string          `json:"created_by"`
	OrgID            string          `json:"org_id"`
	AssigneeID       *string         `json:"assignee_id,omitempty"`
	WorkflowID       *string         `json:"workflow_id,omitempty"`
	RunID            *string         `json:"run_id,omitempty"`
	ForgeExecutionID *string         `json:"forge_execution_id,omitempty"`
	Comments         []TicketComment `json:"comments"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// TicketComment is a message attached to a Ticket.
type TicketComment struct {
	CommentID string    `json:"comment_id"`
	TicketID  string    `json:"ticket_id"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
