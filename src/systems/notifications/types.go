package main

import "time"

// Channel types correspond to registered provider plugins (see providers.go).
const (
	ChannelTypeSlack   = "slack"
	ChannelTypeEmail   = "email"
	ChannelTypeWebhook = "webhook"
)

// Delivery statuses for a Notification row.
const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusFailed  = "failed"
)

const maxBodyBytes = 64 * 1024

// maxAttempts caps how many times the retry worker re-sends a failed/pending
// notification before giving it up permanently.
const maxAttempts = 5

// redacted is substituted for secret config values in API responses. On update,
// a field whose incoming value equals redacted is left unchanged so a UI
// round-trip never wipes a stored secret.
const redacted = "***"

// Channel is a configured delivery target — one instance of a provider plugin
// (e.g. a specific Slack webhook or SMTP mailbox). Config holds the
// provider-specific settings; secret keys are redacted in API responses.
// OrgID="" means the channel is personal to CreatedBy.
type Channel struct {
	ChannelID string            `json:"channel_id" gorm:"column:channel_id;primaryKey"`
	Name      string            `json:"name"       gorm:"column:name"`
	Type      string            `json:"type"       gorm:"column:type"`
	Config    map[string]string `json:"config"     gorm:"column:config;serializer:json"`
	Enabled   bool              `json:"enabled"    gorm:"column:enabled;default:true"`
	CreatedBy string            `json:"created_by" gorm:"column:created_by"`
	OrgID     string            `json:"org_id"     gorm:"column:org_id;default:''"`
	Active    bool              `json:"-"          gorm:"column:active;default:true"`
	CreatedAt time.Time         `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time         `json:"updated_at" gorm:"column:updated_at"`
}

// TableName sets the GORM table name for Channel.
func (Channel) TableName() string { return "channels" }

// Notification is a single delivery attempt record against a Channel. It doubles
// as the durable queue the retry worker drains: rows in StatusPending or
// StatusFailed with Attempts < maxAttempts are re-sent until they succeed or are
// exhausted.
type Notification struct {
	NotificationID string     `json:"notification_id" gorm:"column:notification_id;primaryKey"`
	ChannelID      string     `json:"channel_id"      gorm:"column:channel_id"`
	ChannelType    string     `json:"channel_type"    gorm:"column:channel_type"`
	Subject        string     `json:"subject"         gorm:"column:subject;default:''"`
	Body           string     `json:"body"            gorm:"column:body"`
	Status         string     `json:"status"          gorm:"column:status;default:'pending'"`
	Attempts       int        `json:"attempts"        gorm:"column:attempts;default:0"`
	LastError      string     `json:"last_error"      gorm:"column:last_error;default:''"`
	CreatedBy      string     `json:"created_by"      gorm:"column:created_by"`
	OrgID          string     `json:"org_id"          gorm:"column:org_id;default:''"`
	SentAt         *time.Time `json:"sent_at,omitempty" gorm:"column:sent_at"`
	CreatedAt      time.Time  `json:"created_at"      gorm:"column:created_at"`
	UpdatedAt      time.Time  `json:"updated_at"      gorm:"column:updated_at"`
}

// TableName sets the GORM table name for Notification.
func (Notification) TableName() string { return "notifications" }
