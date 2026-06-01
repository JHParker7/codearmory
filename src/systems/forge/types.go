package main

import "time"

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusCancelled = "cancelled"
)

const (
	maxOutputBytes = 1 * 1024 * 1024 // 1 MB per stream stored in DB
	maxBodyBytes   = 64 * 1024
	defaultTimeout = int64(30)
	maxTimeout     = int64(3600)
)

// Execution is the persistent record for a single container run submitted to forge.
// Status progresses: pending → running → completed | failed | timed_out | cancelled.
type Execution struct {
	ExecutionID string            `json:"execution_id"`
	UserID      string            `json:"user_id"`
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env"`
	TimeoutSecs int64             `json:"timeout"`
	Status      string            `json:"status"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	Stdout      *string           `json:"stdout,omitempty"`
	Stderr      *string           `json:"stderr,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	EndedAt     *time.Time        `json:"ended_at,omitempty"`
}

type submitRequest struct {
	Image   string            `json:"image"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
	Timeout int64             `json:"timeout"`
}

// RunResult holds the output of a completed container run.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}
