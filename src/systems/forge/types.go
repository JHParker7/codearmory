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
	// Backend is the runtime backend this execution runs on, snapshotted from the
	// runner class at submit time so re-pointing the class mid-flight cannot move
	// an already-queued job to a different runtime.
	Backend  string  `gorm:"column:backend;not null;default:default"                 json:"backend"`
	Status   string  `gorm:"column:status;not null;default:pending"                  json:"status"`
	ExitCode *int    `gorm:"column:exit_code"                                        json:"exit_code,omitempty"`
	Stdout   *string `gorm:"column:stdout"                                           json:"stdout,omitempty"`
	Stderr   *string `gorm:"column:stderr"                                           json:"stderr,omitempty"`
	// MemoryUsedMB is the peak memory the run's container consumed, captured
	// best-effort from the runtime (k8s metrics-server / docker stats). It is NULL
	// when metrics are unavailable — most often a very short job a metrics-server
	// never sampled. MemoryLimitMB is the runner class's memory ceiling at run time.
	MemoryUsedMB  *int64     `gorm:"column:memory_used_mb"                                json:"memory_used_mb,omitempty"`
	MemoryLimitMB *int64     `gorm:"column:memory_limit_mb"                               json:"memory_limit_mb,omitempty"`
	CreatedAt     time.Time  `gorm:"column:created_at;not null;default:now()"            json:"created_at"`
	StartedAt     *time.Time `gorm:"column:started_at"                                   json:"started_at,omitempty"`
	EndedAt       *time.Time `gorm:"column:ended_at"                                     json:"ended_at,omitempty"`
}

// RunnerClass defines the resource limits for a named execution tier. The same
// resource fields are reinterpreted per backend: docker/k8s read MemoryMB/
// CPUMillicores/PidsLimit/TmpfsMB; the proxmox backend maps MemoryMB→VM RAM,
// CPUMillicores→ceil(/1000) vCPU and adds DiskGB (which docker/k8s ignore).
type RunnerClass struct {
	Name          string `gorm:"primaryKey"                 json:"name"`
	MemoryMB      int64  `gorm:"not null"                   json:"memory_mb"`
	CPUMillicores int64  `gorm:"not null"                   json:"cpu_millicores"`
	PidsLimit     int64  `gorm:"not null;default:64"        json:"pids_limit"`
	TmpfsMB       int64  `gorm:"not null;default:64"        json:"tmpfs_mb"`
	DiskGB        int64  `gorm:"not null;default:10"        json:"disk_gb"`
	// Backend names the RuntimeBackend this class runs on. Defaults to "default",
	// the backend seeded from the legacy RUNTIME env, so existing classes keep
	// working unchanged.
	Backend string `gorm:"not null;default:default"   json:"backend"`
	Enabled bool   `gorm:"not null;default:true"      json:"enabled"`
	// Privileged runs the job as root with a writable root filesystem and
	// privilege escalation allowed, so package managers (apt/pacman/dnf) work. It
	// is ONLY honoured by VM-isolated (kata) backends, where the microVM — not the
	// container — is the isolation boundary. On container backends
	// (docker/kubernetes/runc) it is ignored at runtime: root in a shared-kernel
	// container is an escape risk, so the locked-down sandbox is always applied
	// there regardless of this flag.
	Privileged bool `gorm:"not null;default:false"     json:"privileged"`
}

// RuntimeBackend is an admin-managed runtime target. Type selects the runtime
// implementation (docker|kubernetes|proxmox|kata); Config holds non-secret settings
// (jsonb) and SecretRefs maps a logical key to the NAME of an env var read via
// secret() — credentials never live in the database, so returning a backend is
// always safe.
type RuntimeBackend struct {
	Name string `gorm:"column:name;primaryKey" json:"name"`
	Type string `gorm:"column:type;not null"   json:"type"`
	// Enabled has no DB-level default on purpose: a `default:true` tag makes GORM
	// omit the false zero-value on insert, so a backend created disabled would come
	// back enabled. Create/Update always set this explicitly from the request body.
	Enabled    bool              `gorm:"column:enabled;not null"                                    json:"enabled"`
	Config     map[string]string `gorm:"column:config;type:jsonb;not null;default:'{}';serializer:json"      json:"config"`
	SecretRefs map[string]string `gorm:"column:secret_refs;type:jsonb;not null;default:'{}';serializer:json" json:"secret_refs"`
	CreatedAt  time.Time         `gorm:"column:created_at;not null;default:now()"                   json:"created_at"`
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
	// MemoryUsedMB is the peak container memory in MB, nil when the runtime could
	// not measure it. MemoryLimitMB is the runner class's memory ceiling.
	MemoryUsedMB  *int64
	MemoryLimitMB *int64
}
