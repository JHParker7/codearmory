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
	ExecutionID string            `gorm:"column:execution_id;primaryKey"                          json:"execution_id"`
	UserID      string            `gorm:"column:user_id;not null"                                 json:"user_id"`
	Image       string            `gorm:"column:image;not null"                                   json:"image"`
	Command     []string          `gorm:"column:command;type:jsonb;not null;serializer:json"       json:"command"`
	Env         map[string]string `gorm:"column:env;type:jsonb;not null;default:'{}';serializer:json" json:"env"`
	TimeoutSecs int64             `gorm:"column:timeout_secs;not null;default:30"                 json:"timeout"`
	RunnerClass string            `gorm:"column:runner_class;not null;default:standard"           json:"runner_class"`
	Status      string            `gorm:"column:status;not null;default:pending"                  json:"status"`
	ExitCode    *int              `gorm:"column:exit_code"                                        json:"exit_code,omitempty"`
	Stdout      *string           `gorm:"column:stdout"                                           json:"stdout,omitempty"`
	Stderr      *string           `gorm:"column:stderr"                                           json:"stderr,omitempty"`
	CreatedAt   time.Time         `gorm:"column:created_at;not null;default:now()"                json:"created_at"`
	StartedAt   *time.Time        `gorm:"column:started_at"                                       json:"started_at,omitempty"`
	EndedAt     *time.Time        `gorm:"column:ended_at"                                         json:"ended_at,omitempty"`
}

// RunnerClass defines the resource limits for a named execution tier.
type RunnerClass struct {
	Name          string `gorm:"primaryKey"            json:"name"`
	MemoryMB      int64  `gorm:"not null"              json:"memory_mb"`
	CPUMillicores int64  `gorm:"not null"              json:"cpu_millicores"`
	PidsLimit     int64  `gorm:"not null;default:64"   json:"pids_limit"`
	TmpfsMB       int64  `gorm:"not null;default:64"   json:"tmpfs_mb"`
	Enabled       bool   `gorm:"not null;default:true" json:"enabled"`
}

type submitRequest struct {
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env"`
	Timeout     int64             `json:"timeout"`
	RunnerClass string            `json:"runner_class"`
}

// RunResult holds the output of a completed container run. ExitCode is a pointer
// so a runtime failure that never produced an exit code (image pull, container
// create, cancellation, timeout) is recorded as NULL rather than a misleading 0.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode *int
}
