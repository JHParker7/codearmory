package main

import "time"

const maxBodyBytes = 64 * 1024

// PipelineRule describes a rule that maps incoming webhook events for a
// repository to a workflow run. The Secret field is stored in the database
// but is intentionally omitted from all JSON responses.
type PipelineRule struct {
	RuleID       string            `json:"rule_id"`
	Name         string            `json:"name"`
	Repo         string            `json:"repo"`
	Events       []string          `json:"events"`
	RefFilter    string            `json:"ref_filter"`    // empty = match all; "refs/heads/main" = exact; "refs/heads/*" = prefix
	WorkflowID   string            `json:"workflow_id"`
	InputMapping map[string]string `json:"input_mapping"` // workflow input key → payload field name
	CreatedBy    string            `json:"created_by"`
	OrgID        string            `json:"org_id"`
	Active       bool              `json:"active"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	// Secret is NOT included in JSON responses
}

// HookEvent is a single webhook delivery received by the service.
type HookEvent struct {
	EventID      string            `json:"event_id"`
	Repo         string            `json:"repo"`
	EventType    string            `json:"event_type"`
	Ref          string            `json:"ref"`
	Payload      map[string]string `json:"payload"`
	RulesMatched int               `json:"rules_matched"`
	Status       string            `json:"status"`
	Triggers     []HookTrigger     `json:"triggers"`
	CreatedAt    time.Time         `json:"created_at"`
}

// HookTrigger records the outcome of dispatching a single rule match to the
// workflows service.
type HookTrigger struct {
	TriggerID  string    `json:"trigger_id"`
	EventID    string    `json:"event_id"`
	RuleID     string    `json:"rule_id"`
	WorkflowID string    `json:"workflow_id"`
	RunID      *string   `json:"run_id,omitempty"`
	Status     string    `json:"status"`
	Error      *string   `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
