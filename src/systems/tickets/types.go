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
var validStatuses = []string{StatusOpen, StatusInProgress, StatusResolved, StatusClosed}
var validPriorities = []string{PriorityLow, PriorityMedium, PriorityHigh, PriorityCritical}

// terminalStatuses are the statuses that mark a ticket as done (no longer
// counted as "open") for the per-board open/total tallies. Mirrors the portal's
// isTerminal().
var terminalStatuses = []string{StatusResolved, StatusClosed}

const maxBodyBytes = 64 * 1024

// Ticket is a task or issue belonging to a user and optionally an org.
// WorkflowID/RunID and ForgeExecutionID are optional references to linked resources
// in other services; they are stored as plain text with no FK enforcement.
type Ticket struct {
	TicketID    string     `json:"ticket_id"          gorm:"column:ticket_id;primaryKey"`
	Title       string     `json:"title"              gorm:"column:title"`
	Description string     `json:"description"        gorm:"column:description;default:''"`
	Status      string     `json:"status"             gorm:"column:status;default:'open'"`
	Priority    string     `json:"priority"           gorm:"column:priority;default:'medium'"`
	Timescale   string     `json:"timescale"          gorm:"column:timescale;default:''"`
	DueDate     *time.Time `json:"due_date,omitempty" gorm:"column:due_date"`
	CreatedBy   string     `json:"created_by"         gorm:"column:created_by"`
	OrgID       string     `json:"org_id"             gorm:"column:org_id;default:''"`
	// Namespace is the OWNER's gatekeeper namespace — the creator's username —
	// recorded at creation so a per-record permission check can name the owner
	// instead of the caller. Without it gatekeeper evaluates an identical resource
	// whoever asks, and returns authorized for any id.
	//
	// Empty on every row created before this field existed. That is deliberate and
	// load-bearing: an empty namespace means "legacy row", and such a ticket keeps
	// the old caller-scoped resource (see ticketResource) so it stays reachable
	// without a backfill that would have to resolve a username per distinct creator.
	Namespace string  `json:"namespace,omitempty" gorm:"column:namespace;default:''"`
	Project   string  `json:"project,omitempty"  gorm:"column:project;default:''"`
	BoardID   *string `json:"board_id,omitempty" gorm:"column:board_id"`
	// ParentID links this ticket to a parent ticket (sub-ticket hierarchy). nil = a
	// top-level ticket. Validated to exist, be accessible, and not form a cycle.
	ParentID         *string `json:"parent_id,omitempty" gorm:"column:parent_id"`
	AssigneeID       *string `json:"assignee_id,omitempty"        gorm:"column:assignee_id"`
	WorkflowID       *string `json:"workflow_id,omitempty"        gorm:"column:workflow_id"`
	RunID            *string `json:"run_id,omitempty"             gorm:"column:run_id"`
	ForgeExecutionID *string `json:"forge_execution_id,omitempty" gorm:"column:forge_execution_id"`
	Active           bool    `json:"-"                  gorm:"column:active;default:true"`
	// Version is the optimistic-concurrency token, incremented by the database on
	// every write. Clients read it as an ETag on GET and send it back as If-Match
	// on PUT to say "apply this only if nobody else has changed the ticket since I
	// read it"; a mismatch is a 412 rather than a silent overwrite.
	//
	// Without this, PUT is last-write-wins: two callers can both read a ticket,
	// both write, and both believe they won. That is invisible when two people
	// edit a ticket in the portal, and it is a correctness problem when several
	// agent hosts race to claim the same work.
	//
	// If-Match is OPTIONAL. A caller that omits it keeps the previous
	// last-write-wins behaviour, so this is additive and needs no client flag day.
	Version  int64           `json:"version"            gorm:"column:version;not null;default:0"`
	Comments []TicketComment `json:"comments"           gorm:"-"`
	// DependsOn is the tickets that must be finished before this one can be
	// worked. Unlike Comments it IS populated on listings — see loadDependencies
	// for why that difference is deliberate.
	DependsOn []TicketDependencyView `json:"depends_on"         gorm:"-"`
	CreatedAt time.Time              `json:"created_at"         gorm:"column:created_at"`
	UpdatedAt time.Time              `json:"updated_at"         gorm:"column:updated_at"`
}

// TicketDependency records that one ticket cannot proceed until another is
// finished. Distinct from ParentID, which is hierarchy: a sub-ticket is PART OF
// its parent, whereas a dependency is ORDERING between tickets that may live
// anywhere. Conflating the two would mean either that work cannot be broken down
// without implying an order, or that it cannot be ordered without implying
// containment.
//
// The pair is the primary key, so declaring the same dependency twice is
// idempotent rather than an error or a duplicate row.
type TicketDependency struct {
	TicketID    string    `json:"ticket_id"     gorm:"column:ticket_id;primaryKey"`
	DependsOnID string    `json:"depends_on_id" gorm:"column:depends_on_id;primaryKey"`
	CreatedBy   string    `json:"created_by"    gorm:"column:created_by;default:''"`
	CreatedAt   time.Time `json:"created_at"    gorm:"column:created_at"`
}

// TableName sets the GORM table name for TicketDependency.
func (TicketDependency) TableName() string { return "ticket_dependencies" }

// TicketDependencyView is one entry in a ticket's depends_on list.
//
// It carries the blocker's STATUS, not just its id, so a caller can decide
// whether the block is still in force without fetching every dependency
// separately. That matters most to the agent runtimes, which poll a column and
// would otherwise turn one listing into an N+1 storm.
//
// Status is deliberately raw rather than a "satisfied" boolean: boards define
// their own columns, so only the caller knows which of its columns means done.
// A dependency the caller cannot see is returned as an id with no title or
// status — enough to know something blocks it, without leaking what.
type TicketDependencyView struct {
	TicketID string `json:"ticket_id"`
	Title    string `json:"title,omitempty"`
	Status   string `json:"status,omitempty"`
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
	BoardID     string `json:"board_id"     gorm:"column:board_id;primaryKey"`
	Name        string `json:"name"         gorm:"column:name"`
	Description string `json:"description"  gorm:"column:description;default:''"`
	Color       string `json:"color"        gorm:"column:color;default:''"`
	Position    int    `json:"position"     gorm:"column:position;default:0"`
	CreatedBy   string `json:"created_by"   gorm:"column:created_by"`
	OrgID       string `json:"org_id"       gorm:"column:org_id;default:''"`
	// Project is the free-text project (workspace) slug the board is filed under; when
	// it resolves to a real gatekeeper project ProjectID/ProjectNamespace are set and
	// access is additionally granted by a project role (see project.go).
	Project          string    `json:"project,omitempty"           gorm:"column:project;default:''"`
	ProjectID        string    `json:"project_id,omitempty"        gorm:"column:project_id;default:''"`
	ProjectNamespace string    `json:"project_namespace,omitempty" gorm:"column:project_namespace;default:''"`
	Active           bool      `json:"-"            gorm:"column:active;default:true"`
	CreatedAt        time.Time `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt        time.Time `json:"updated_at"   gorm:"column:updated_at"`
	// OpenCount/TotalCount are computed on read (not stored): TotalCount is every
	// active ticket on the board and OpenCount those still open (not in a terminal
	// status). They are populated by the list/get board handlers.
	OpenCount  int64 `json:"open_count"  gorm:"-"`
	TotalCount int64 `json:"total_count" gorm:"-"`
}

// TableName sets the GORM table name for Board.
func (Board) TableName() string { return "ticket_boards" }

// TicketFieldDef defines a custom status, priority, or timescale value.
// OrgID="" means it is a system-wide default visible to all orgs.
//
// BoardID scopes a def to a single board so each board owns its own status columns
// AND its own priority options ("linked to the board"), never merged across boards.
// BoardID="" is the org/global level used as the fallback for boards that have not
// configured their own. Status and priority defs are board-scoped; timescale defs
// always keep BoardID="".
type TicketFieldDef struct {
	FieldDefID string    `json:"field_def_id"        gorm:"column:field_def_id;primaryKey"`
	OrgID      string    `json:"org_id"              gorm:"column:org_id;default:''"`
	BoardID    string    `json:"board_id,omitempty"  gorm:"column:board_id;default:''"`
	Kind       string    `json:"kind"                gorm:"column:kind"`
	Value      string    `json:"value"               gorm:"column:value"`
	Label      string    `json:"label"               gorm:"column:label"`
	Color      string    `json:"color"               gorm:"column:color;default:''"`
	Position   int       `json:"position"            gorm:"column:position;default:0"`
	Active     bool      `json:"-"                   gorm:"column:active;default:true"`
	CreatedAt  time.Time `json:"created_at"          gorm:"column:created_at"`
	UpdatedAt  time.Time `json:"updated_at"          gorm:"column:updated_at"`
}

// TableName sets the GORM table name for TicketFieldDef.
func (TicketFieldDef) TableName() string { return "ticket_field_defs" }
