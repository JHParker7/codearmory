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
	WorkflowID  string         `json:"workflow_id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	CreatedBy   string         `json:"created_by"`
	OrgID       string         `json:"org_id"`
	Steps       []WorkflowStep `json:"steps"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Active      bool           `json:"active"`
}

// WorkflowRun is a single triggered execution of a Workflow.
// Token holds the caller's Bearer JWT, forwarded to each step's target service.
// It is cleared once the run reaches a terminal state.
type WorkflowRun struct {
	RunID       string            `json:"run_id"`
	WorkflowID  string            `json:"workflow_id"`
	TriggeredBy string            `json:"triggered_by"`
	OrgID       string            `json:"org_id"`
	Status      string            `json:"status"`
	CurrentStep int               `json:"current_step"`
	Inputs      map[string]string `json:"inputs"`
	StepRuns    []WorkflowStepRun `json:"step_runs"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	EndedAt     *time.Time        `json:"ended_at,omitempty"`
}

// WorkflowStepRun is the execution record for one step within a WorkflowRun.
type WorkflowStepRun struct {
	StepRunID      string     `json:"step_run_id"`
	RunID          string     `json:"run_id"`
	StepIndex      int        `json:"step_index"`
	StepName       string     `json:"step_name"`
	Status         string     `json:"status"`
	ResponseStatus *int       `json:"response_status,omitempty"`
	ResponseBody   *string    `json:"response_body,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
}
