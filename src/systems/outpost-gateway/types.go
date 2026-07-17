package main

import "time"

// Outpost status values.
const (
	OutpostPending   = "pending"   // registered in the portal, not yet enrolled
	OutpostConnected = "connected" // enrolled and seen recently via heartbeat/poll
	OutpostStale     = "stale"     // enrolled but not seen within the liveness window
)

// Command status values (the outpost_commands queue).
const (
	CmdPending = "pending"
	CmdClaimed = "claimed"
	CmdDone    = "done"
	// CmdFailed is a terminal ack state: the outpost ran the command and it failed
	// (e.g. an integration-test Job that exited non-zero). Distinct from CmdDone so a
	// caller polling the command can gate on the actual outcome, not merely delivery.
	CmdFailed = "failed"
)

// Event delivery status values (the outpost_events outbox).
const (
	EvPending   = "pending"
	EvDelivered = "delivered"
	EvFailed    = "failed" // dead-lettered after exhausting retries
)

const maxBodyBytes = 64 * 1024

// maxDeliveryAttempts bounds dead-letter retries for an outbound event.
const maxDeliveryAttempts = 8

// commandClaimLease is the visibility timeout for a claimed command. If an
// outpost claims a command but never acks it (crash, lost response, swallowed
// ack), the command is reclaimed and re-delivered once it has been claimed for
// longer than this. It must comfortably exceed how long a live outpost takes to
// handle a command and ack it, so a slow-but-alive outpost is not double-served.
const commandClaimLease = 5 * time.Minute

// Outpost is a customer/cluster-deployed agent. The gateway owns its credentials
// (an enrollment token, single-use, then a long-lived outpost key) and never
// stores cluster credentials itself.
type Outpost struct {
	OutpostID       string     `json:"outpost_id"  gorm:"column:outpost_id;primaryKey"`
	OrgID           string     `json:"org_id"      gorm:"column:org_id;default:''"`
	UserID          string     `json:"user_id"     gorm:"column:user_id;default:''"`
	Name            string     `json:"name"        gorm:"column:name"`
	Modules         string     `json:"modules"     gorm:"column:modules;default:''"` // csv, e.g. "chaos,argo"
	Status          string     `json:"status"      gorm:"column:status;default:'pending'"`
	EnrollTokenHash string     `json:"-"           gorm:"column:enroll_token_hash;default:''"`
	KeyHash         string     `json:"-"           gorm:"column:key_hash;default:''"`
	Active          bool       `json:"-"           gorm:"column:active;default:true"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty" gorm:"column:last_seen_at"`
	CreatedAt       time.Time  `json:"created_at"  gorm:"column:created_at"`
	UpdatedAt       time.Time  `json:"updated_at"  gorm:"column:updated_at"`
}

func (Outpost) TableName() string { return "outposts" }

// OutpostCommand is a control→outpost instruction. Control-plane services enqueue
// these via /internal/commands; the matching outpost long-polls and claims them.
type OutpostCommand struct {
	ID          string         `json:"id"          gorm:"column:id;primaryKey"`
	OutpostID   string         `json:"outpost_id"  gorm:"column:outpost_id;index"`
	Integration string         `json:"integration" gorm:"column:integration"`
	Type        string         `json:"type"        gorm:"column:type"`
	Payload     map[string]any `json:"payload"     gorm:"column:payload;serializer:json"`
	Status      string         `json:"status"      gorm:"column:status;default:'pending';index"`
	// Error carries the outpost's failure detail (e.g. the test Job's summary) when
	// Status is CmdFailed, so a poller sees why it failed, not just that it did.
	Error     string     `json:"error"      gorm:"column:error;default:''"`
	CreatedAt time.Time  `json:"created_at" gorm:"column:created_at"`
	ClaimedAt *time.Time `json:"-"          gorm:"column:claimed_at"`
}

func (OutpostCommand) TableName() string { return "outpost_commands" }

// OutpostEvent is an outpost→control message held in a transactional outbox and
// delivered by the dispatcher to the integration's consumer service. Retries are
// at-least-once with exponential backoff; consumers dedupe by ID.
type OutpostEvent struct {
	ID          string         `json:"id"          gorm:"column:id;primaryKey"`
	OutpostID   string         `json:"outpost_id"  gorm:"column:outpost_id;index"`
	OrgID       string         `json:"org_id"      gorm:"column:org_id;default:''"`
	UserID      string         `json:"user_id"     gorm:"column:user_id;default:''"`
	Integration string         `json:"integration" gorm:"column:integration"`
	Type        string         `json:"type"        gorm:"column:type"`
	Payload     map[string]any `json:"payload"     gorm:"column:payload;serializer:json"`
	Status      string         `json:"status"      gorm:"column:status;default:'pending';index"`
	Attempt     int            `json:"attempt"     gorm:"column:attempt;default:0"`
	LastError   string         `json:"last_error"  gorm:"column:last_error;default:''"`
	NextRetryAt time.Time      `json:"next_retry_at" gorm:"column:next_retry_at"`
	CreatedAt   time.Time      `json:"created_at"  gorm:"column:created_at"`
}

func (OutpostEvent) TableName() string { return "outpost_events" }
