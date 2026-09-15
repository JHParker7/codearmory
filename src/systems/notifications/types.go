package main

import "time"

// Channel is a configured delivery destination: a provider (slack/discord/webhook/email)
// plus the config it needs and the event types it wants. It is owned by the user/org that
// created it and may be scoped to a project so a project's members share it.
type Channel struct {
	ChannelID string            `gorm:"primaryKey;column:channel_id" json:"id"`
	Name      string            `gorm:"column:name" json:"name"`
	Provider  string            `gorm:"column:provider;index" json:"provider"` // slack | discord | webhook | email
	Config    map[string]string `gorm:"column:config;serializer:json" json:"config"`
	// Events is the set of event TYPES this channel wants (e.g. "repo.pull_request.opened").
	// A single "*" matches every event.
	Events    []string  `gorm:"column:events;serializer:json" json:"events"`
	Project   string    `gorm:"column:project;index" json:"project,omitempty"`
	Enabled   bool      `gorm:"column:enabled" json:"enabled"`
	CreatedBy string    `gorm:"column:created_by;index" json:"created_by"`
	OrgID     string    `gorm:"column:org_id;index" json:"org_id,omitempty"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Channel) TableName() string { return "channels" }

// Delivery is the per-attempt log: which channel got which event, and whether it landed.
type Delivery struct {
	DeliveryID string    `gorm:"primaryKey;column:delivery_id" json:"id"`
	ChannelID  string    `gorm:"column:channel_id;index" json:"channel_id"`
	EventType  string    `gorm:"column:event_type" json:"event_type"`
	EventID    string    `gorm:"column:event_id" json:"event_id"`
	Status     string    `gorm:"column:status" json:"status"` // delivered | failed
	Error      string    `gorm:"column:error" json:"error,omitempty"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
}

func (Delivery) TableName() string { return "deliveries" }

// Provider is a static description of an integration type (for GET /providers so a UI can
// render the config form). Not persisted.
type Provider struct {
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ConfigKeys  []string `json:"config_keys"` // the Config fields this provider expects
}

func providers() []Provider {
	return []Provider{
		{Type: "slack", Name: "Slack", Description: "Post to a Slack channel via an incoming webhook.", ConfigKeys: []string{"url"}},
		{Type: "discord", Name: "Discord", Description: "Post to a Discord channel via a channel webhook.", ConfigKeys: []string{"url"}},
		{Type: "webhook", Name: "Webhook", Description: "POST the raw event JSON to any HTTPS endpoint.", ConfigKeys: []string{"url"}},
		{Type: "email", Name: "Email", Description: "Send an email via SMTP (SMTP_HOST/PORT/USER/PASS/FROM env).", ConfigKeys: []string{"to"}},
	}
}

func validProvider(t string) bool {
	for _, p := range providers() {
		if p.Type == t {
			return true
		}
	}
	return false
}

// Event mirrors the events service's envelope (src/systems/events/types.go). Redeclared
// here so notifications takes no dependency on the events module.
type Event struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Source     string         `json:"source"`
	Subject    string         `json:"subject"`
	OccurredAt string         `json:"occurred_at"`
	Data       map[string]any `json:"data,omitempty"`
	Actor      struct {
		OrgID  string `json:"org_id,omitempty"`
		UserID string `json:"user_id,omitempty"`
	} `json:"actor"`
}
