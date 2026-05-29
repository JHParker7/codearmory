package main

import (
	"encoding/json"
	"time"
)

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

const (
	maxBodyBytes    = 64 * 1024
	maxResponseBody = 1 * 1024 * 1024 // 1 MB stored per step
	defaultTimeout  = int64(30)
	maxTimeout      = int64(3600)
	maxSteps        = 50
)

// WorkflowStep is a single step in a workflow definition. It describes a generic
// HTTP request to a named, registered service. The Bearer token from the
// triggering user is forwarded automatically.
//
// Path, Body, and Header values support ${KEY} substitution from run-level Inputs.
type WorkflowStep struct {
	Name           string            `json:"name"`
	Service        string            `json:"service"`                   // registered service name, e.g. "forge"
	Method         string            `json:"method"`                    // HTTP method; defaults to "POST"
	Path           string            `json:"path"`                      // path on the target service, e.g. "/executions"
	Body           json.RawMessage   `json:"body,omitempty"`            // optional JSON body
	Headers        map[string]string `json:"headers,omitempty"`         // extra request headers
	ExpectedStatus int               `json:"expected_status,omitempty"` // 0 = any 2xx response
	TimeoutSecs    int64             `json:"timeout_secs"`
}

// Workflow is a named, ordered sequence of steps.
type Workflow struct {
	WorkflowID  string         `json:"workflow_id"  gorm:"column:workflow_id;primaryKey"`
	Name        string         `json:"name"         gorm:"column:name"`
	Description string         `json:"description"  gorm:"column:description;default:''"`
	CreatedBy   string         `json:"created_by"   gorm:"column:created_by"`
	OrgID       string         `json:"org_id"       gorm:"column:org_id;default:''"`
	Steps       []WorkflowStep `json:"steps"        gorm:"column:steps;serializer:json"`
	CreatedAt   time.Time      `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt   time.Time      `json:"updated_at"   gorm:"column:updated_at"`
	Active      bool           `json:"-"            gorm:"column:active;default:true"`
}

func (Workflow) TableName() string { return "workflows" }

// WorkflowRun is a single triggered execution of a Workflow.
// Token holds the caller's Bearer JWT, forwarded to each step's target service.
// It is cleared once the run reaches a terminal state.
type WorkflowRun struct {
	RunID       string            `json:"run_id"       gorm:"column:run_id;primaryKey"`
	WorkflowID  string            `json:"workflow_id"  gorm:"column:workflow_id"`
	TriggeredBy string            `json:"triggered_by" gorm:"column:triggered_by"`
	OrgID       string            `json:"org_id"       gorm:"column:org_id;default:''"`
	Status      string            `json:"status"       gorm:"column:status;default:'pending'"`
	CurrentStep int               `json:"current_step" gorm:"column:current_step;default:0"`
	Inputs      map[string]string `json:"inputs"       gorm:"column:inputs;serializer:json"`
	Token       string            `json:"-"            gorm:"column:token"`
	StepRuns    []WorkflowStepRun `json:"step_runs"    gorm:"-"`
	CreatedAt   time.Time         `json:"created_at"   gorm:"column:created_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty" gorm:"column:started_at"`
	EndedAt     *time.Time        `json:"ended_at,omitempty"   gorm:"column:ended_at"`
}

func (WorkflowRun) TableName() string { return "workflow_runs" }

// WorkflowStepRun is the execution record for one step within a WorkflowRun.
type WorkflowStepRun struct {
	StepRunID      string     `json:"step_run_id"               gorm:"column:step_run_id;primaryKey"`
	RunID          string     `json:"run_id"                    gorm:"column:run_id"`
	StepIndex      int        `json:"step_index"                gorm:"column:step_index"`
	StepName       string     `json:"step_name"                 gorm:"column:step_name"`
	Status         string     `json:"status"                    gorm:"column:status;default:'pending'"`
	ResponseStatus *int       `json:"response_status,omitempty" gorm:"column:response_status"`
	ResponseBody   *string    `json:"response_body,omitempty"   gorm:"column:response_body"`
	StartedAt      *time.Time `json:"started_at,omitempty"      gorm:"column:started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"        gorm:"column:ended_at"`
}

func (WorkflowStepRun) TableName() string { return "workflow_step_runs" }
