package main

import "time"

// PermissionSpec is a single gatekeeper permission triple required by a step action.
type PermissionSpec struct {
	Service  string `json:"service"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

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

// ActionHTTP is the escape hatch for raw HTTP calls to any registered service.
const ActionHTTP = "http"

// AsyncConfig describes how to poll an async job to terminal state.
type AsyncConfig struct {
	IDField          string   `json:"id_field"`
	PollPath         string   `json:"poll_path"`
	PollIntervalSecs int      `json:"poll_interval_secs"`
	StatusField      string   `json:"status_field"`
	SuccessStates    []string `json:"success_states"`
	FailureStates    []string `json:"failure_states"`
	CancelStates     []string `json:"cancel_states"`
	OutputField      string   `json:"output_field"`
	ErrorFields      []string `json:"error_fields"`
}

// BodyTransform rewrites a With key before the payload is sent to the service.
// If Wrap is non-empty the string value is appended to form a string slice.
type BodyTransform struct {
	FromKey string   `json:"from_key"`
	ToKey   string   `json:"to_key"`
	Wrap    []string `json:"wrap,omitempty"`
}

// ActionDef is a callable workflow action loaded from the registry action catalog.
type ActionDef struct {
	Name               string          `json:"name"`
	ServiceName        string          `json:"service_name"`
	ServiceURL         string          `json:"service_url"`
	Method             string          `json:"method"`
	Path               string          `json:"path"`
	BodyTransforms     []BodyTransform `json:"body_transforms,omitempty"`
	Async              *AsyncConfig    `json:"async,omitempty"`
	RequiredPermission *PermissionSpec `json:"required_permission,omitempty"`
}

// Step is a reusable, named action definition that can be composed into workflows.
// Action determines what the step does; With holds action-specific configuration.
// String values inside With support ${...} substitution resolved at run time:
// ${inputs.NAME} (or bare ${NAME}) for run inputs, ${steps.STEP.output} for an
// earlier step's output, and ${steps.STEP.output.field} for a JSON field of it.
// See substitution.go.
type Step struct {
	StepID      string         `json:"step_id"      gorm:"column:step_id;primaryKey"`
	Name        string         `json:"name"         gorm:"column:name"`
	Description string         `json:"description"  gorm:"column:description;default:''"`
	Action      string         `json:"action"       gorm:"column:action"`
	With        map[string]any `json:"with"         gorm:"column:config;serializer:json"`
	Timeout     int64          `json:"timeout"      gorm:"column:timeout_secs;default:30"`
	CreatedBy   string         `json:"created_by"   gorm:"column:created_by"`
	OrgID       string         `json:"org_id"       gorm:"column:org_id;default:''"`
	Active      bool           `json:"active"       gorm:"column:active;default:true"`
	CreatedAt   time.Time      `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt   time.Time      `json:"updated_at"   gorm:"column:updated_at"`
}

func (Step) TableName() string { return "steps" }

// WorkflowStepRef records how a step is used within a specific workflow:
// which step and, optionally, which parallel execution group it belongs to.
// Steps sharing the same non-nil ParallelGroup execute concurrently; the run
// waits for all steps in a group before advancing.
type WorkflowStepRef struct {
	StepID        string `json:"step_id"`
	ParallelGroup *int   `json:"parallel_group,omitempty"`
}

// WorkflowStep enriches a WorkflowStepRef with the full Step definition.
// It is assembled at request/execution time and never stored in the DB.
type WorkflowStep struct {
	Step
	ParallelGroup *int `json:"parallel_group,omitempty"`
}

// Workflow is a named, ordered pipeline of step references.
// StepRefs is the authoritative DB column (JSON array of WorkflowStepRef).
// Steps is populated at query time by joining against the steps table.
// RoleID is the gatekeeper role provisioned at creation time; it scopes run
// tokens to only the permissions the workflow's steps actually require.
type Workflow struct {
	WorkflowID  string            `json:"workflow_id"  gorm:"column:workflow_id;primaryKey"`
	Name        string            `json:"name"         gorm:"column:name"`
	Description string            `json:"description"  gorm:"column:description;default:''"`
	CreatedBy   string            `json:"created_by"   gorm:"column:created_by"`
	OrgID       string            `json:"org_id"       gorm:"column:org_id;default:''"`
	Project     string            `json:"project,omitempty" gorm:"column:project;default:''"`
	RoleID      string            `json:"role_id,omitempty" gorm:"column:role_id;default:''"`
	Active      bool              `json:"active"       gorm:"column:active;default:true"`
	CreatedAt   time.Time         `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt   time.Time         `json:"updated_at"   gorm:"column:updated_at"`
	StepRefs    []WorkflowStepRef `json:"-"            gorm:"column:steps;serializer:json"`
	Steps       []WorkflowStep    `json:"steps"        gorm:"-"`
}

func (Workflow) TableName() string { return "workflows" }

// WorkflowRun is a single triggered execution of a Workflow.
// Token holds a short-lived run-scoped JWT minted by gatekeeper at trigger time;
// it is never the triggering user's own session token. RunSessionID tracks the
// underlying gatekeeper session so it can be revoked on terminal state.
type WorkflowRun struct {
	RunID        string            `json:"run_id"       gorm:"column:run_id;primaryKey"`
	WorkflowID   string            `json:"workflow_id"  gorm:"column:workflow_id"`
	TriggeredBy  string            `json:"triggered_by" gorm:"column:triggered_by"`
	OrgID        string            `json:"org_id"       gorm:"column:org_id;default:''"`
	Project      string            `json:"project,omitempty" gorm:"column:project;default:''"`
	Status       string            `json:"status"       gorm:"column:status;default:'pending'"`
	CurrentStep  int               `json:"current_step" gorm:"column:current_step;default:0"`
	Inputs       map[string]string `json:"inputs"       gorm:"column:inputs;serializer:json"`
	Token        string            `json:"-"            gorm:"column:token"`
	RunSessionID string            `json:"-"            gorm:"column:run_session_id"`
	StepRuns     []WorkflowStepRun `json:"step_runs"    gorm:"-"`
	CreatedAt    time.Time         `json:"created_at"   gorm:"column:created_at"`
	StartedAt    *time.Time        `json:"started_at,omitempty" gorm:"column:started_at"`
	EndedAt      *time.Time        `json:"ended_at,omitempty"   gorm:"column:ended_at"`
}

func (WorkflowRun) TableName() string { return "workflow_runs" }

// WorkflowStepRun is the execution record for one step within a WorkflowRun.
type WorkflowStepRun struct {
	StepRunID string  `json:"step_run_id"            gorm:"column:step_run_id;primaryKey"`
	RunID     string  `json:"run_id"                 gorm:"column:run_id"`
	StepIndex int     `json:"step_index"             gorm:"column:step_index"`
	StepName  string  `json:"step_name"              gorm:"column:step_name"`
	Status    string  `json:"status"                 gorm:"column:status;default:'pending'"`
	Output    *string `json:"output,omitempty"       gorm:"column:response_body"`
	// MemoryUsedMB/MemoryLimitMB are carried through from the forge execution a
	// forge-backed step ran (NULL for non-forge steps and when forge could not
	// measure usage). See forge's Execution for how they are captured.
	MemoryUsedMB  *int64     `json:"memory_used_mb,omitempty"  gorm:"column:memory_used_mb"`
	MemoryLimitMB *int64     `json:"memory_limit_mb,omitempty" gorm:"column:memory_limit_mb"`
	StartedAt     *time.Time `json:"started_at,omitempty"   gorm:"column:started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"     gorm:"column:ended_at"`
}

func (WorkflowStepRun) TableName() string { return "workflow_step_runs" }
