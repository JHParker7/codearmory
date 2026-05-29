package main

import "time"

const maxBodyBytes = 64 * 1024

// PipelineRule describes a rule that maps incoming webhook events for a
// repository to a workflow run. The Secret field is stored in the database
// but is intentionally omitted from all JSON responses.
type PipelineRule struct {
	RuleID       string            `json:"rule_id"        gorm:"column:rule_id;primaryKey"`
	Name         string            `json:"name"           gorm:"column:name"`
	Repo         string            `json:"repo"           gorm:"column:repo"`
	Events       []string          `json:"events"         gorm:"column:events;serializer:json"`
	RefFilter    string            `json:"ref_filter"     gorm:"column:ref_filter"`
	WorkflowID   string            `json:"workflow_id"    gorm:"column:workflow_id"`
	Secret       *string           `json:"-"              gorm:"column:secret"`
	InputMapping map[string]string `json:"input_mapping"  gorm:"column:input_mapping;serializer:json"`
	CreatedBy    string            `json:"created_by"     gorm:"column:created_by"`
	OrgID        string            `json:"org_id"         gorm:"column:org_id;default:''"`
	Active       bool              `json:"-"              gorm:"column:active;default:true"`
	CreatedAt    time.Time         `json:"created_at"     gorm:"column:created_at"`
	UpdatedAt    time.Time         `json:"updated_at"     gorm:"column:updated_at"`
}

func (PipelineRule) TableName() string { return "pipeline_rules" }

// HookEvent is a single webhook delivery received by the service.
type HookEvent struct {
	EventID      string            `json:"event_id"       gorm:"column:event_id;primaryKey"`
	Repo         string            `json:"repo"           gorm:"column:repo"`
	EventType    string            `json:"event_type"     gorm:"column:event_type"`
	Ref          string            `json:"ref"            gorm:"column:ref;default:''"`
	Payload      map[string]string `json:"payload"        gorm:"column:payload;serializer:json"`
	RulesMatched int               `json:"rules_matched"  gorm:"column:rules_matched;default:0"`
	Status       string            `json:"status"         gorm:"column:status;default:'received'"`
	Triggers     []HookTrigger     `json:"triggers"       gorm:"-"`
	CreatedAt    time.Time         `json:"created_at"     gorm:"column:created_at"`
}

func (HookEvent) TableName() string { return "hook_events" }

// HookTrigger records the outcome of dispatching a single rule match to the
// workflows service.
type HookTrigger struct {
	TriggerID  string    `json:"trigger_id"        gorm:"column:trigger_id;primaryKey"`
	EventID    string    `json:"event_id"          gorm:"column:event_id"`
	RuleID     string    `json:"rule_id"           gorm:"column:rule_id"`
	WorkflowID string    `json:"workflow_id"       gorm:"column:workflow_id"`
	RunID      *string   `json:"run_id,omitempty"  gorm:"column:run_id"`
	Status     string    `json:"status"            gorm:"column:status;default:'pending'"`
	Error      *string   `json:"error,omitempty"   gorm:"column:error"`
	CreatedAt  time.Time `json:"created_at"        gorm:"column:created_at"`
}

func (HookTrigger) TableName() string { return "hook_triggers" }
