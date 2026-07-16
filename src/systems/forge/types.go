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

// Concurrency-limit scopes. A ConcurrencyLimit row keys on one of these plus the
// scope's id (an org_id or a user_id).
const (
	ConcurrencyScopeOrg  = "org"
	ConcurrencyScopeUser = "user"
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
	Backend string `gorm:"column:backend;not null;default:default"                 json:"backend"`
	// OrgID is the submitter's org, snapshotted from the gatekeeper permission
	// check at submit time. The worker uses it to resolve org-scoped secrets
	// (secret: refs) at dispatch without a live request context.
	OrgID string `gorm:"column:org_id;not null;default:''" json:"org_id,omitempty"`
	// Project is a free-text workspace label used only to filter list views; it is
	// not a security boundary (access stays governed by UserID/OrgID).
	Project string `gorm:"column:project;not null;default:''" json:"project,omitempty"`
	// SecretRefs maps a target env var NAME to a credential reference resolved at
	// dispatch and injected into the runtime env — never into the persisted env.
	// Reference schemes: "secret:<name>" (gatekeeper org secret),
	// "git:<repo-url>" (broker-minted clone URL for any linked backend), and
	// "gitea:<owner>/<repo>" (a minted Forgejo clone URL). Only the references are
	// stored here; the resolved values are never persisted or logged.
	SecretRefs map[string]string `gorm:"column:secret_refs;type:jsonb;not null;default:'{}';serializer:json" json:"secret_refs,omitempty"`
	// OutputEnv lists environment variable NAMES to capture from the command's shell
	// after it runs (so a script's exported/computed values become the execution's
	// structured output). Outputs holds the captured NAME→value map, returned in the
	// poll response so callers (e.g. workflows) can surface it as the step output
	// instead of raw stdout. Capture works for shell (`-c`) commands; see worker.go.
	OutputEnv []string          `gorm:"column:output_env;type:jsonb;not null;default:'[]';serializer:json" json:"output_env,omitempty"`
	Outputs   map[string]string `gorm:"column:outputs;type:jsonb;not null;default:'{}';serializer:json"     json:"outputs,omitempty"`
	// Checkout, when set, asks forge to `git clone` a repo into the sandbox working
	// dir and cd into it before running Command — the actions/checkout equivalent.
	// The clone URL is read at runtime from the env var it names (default
	// GIT_CLONE_URL), typically populated by a git:/gitea: secret_ref. Nil = no
	// auto-checkout (the command runs in the image's working dir as before).
	Checkout *CheckoutSpec `gorm:"column:checkout;type:jsonb;serializer:json" json:"checkout,omitempty"`
	// Volumes lists shared workspace volumes to attach to the sandbox. Each names a
	// volume previously created via POST /volumes (a run-scoped PVC in kubernetes / a
	// named tmpfs volume in docker) and the path to mount it at. This is how a
	// workflow shares a checked-out repo and build artifacts between steps: a git
	// step clones into an attached volume, later steps attach the same volume and see
	// its contents. Empty = an isolated sandbox with no shared storage (the default).
	Volumes []VolumeMount `gorm:"column:volumes;type:jsonb;not null;default:'[]';serializer:json" json:"volumes,omitempty"`
	// Build, when set, makes this execution an image build: forge derives the Image
	// (its own Kaniko builder) and Command (the assembled build invocation) from the
	// spec at submit, so the two fields above are materialised, not user-supplied.
	// Stored for display only — the worker runs the materialised Command and never
	// reads this back, so it is deliberately absent from claimPendingExecution's SELECT.
	Build *BuildSpec `gorm:"column:build;type:jsonb;serializer:json" json:"build,omitempty"`
	// Copy, when set, makes this a volume-copy execution: forge derives the Image (its
	// minimal runner image) and Command (a synthesised `cp` script) from the spec at
	// submit, so the two are materialised, not user-supplied. Like Build it is stored
	// for display only — the worker runs the materialised Command and never reads this
	// back, so it is deliberately absent from claimPendingExecution's SELECT.
	Copy *CopySpec `gorm:"column:copy;type:jsonb;serializer:json" json:"copy,omitempty"`
	// Resolve, when set, makes this a resolve-paths execution: forge derives the Image
	// and Command from the spec at submit and captures the matched paths via output_env.
	// Stored for display only — the worker runs the materialised Command and never reads
	// this back, so it is deliberately absent from claimPendingExecution's SELECT.
	Resolve  *ResolveSpec `gorm:"column:resolve;type:jsonb;serializer:json" json:"resolve,omitempty"`
	Status   string       `gorm:"column:status;not null;default:pending"                  json:"status"`
	ExitCode *int       `gorm:"column:exit_code"                                        json:"exit_code,omitempty"`
	Stdout   *string    `gorm:"column:stdout"                                           json:"stdout,omitempty"`
	Stderr   *string    `gorm:"column:stderr"                                           json:"stderr,omitempty"`
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

// BuildSpec configures a container-image build. Forge builds it with Kaniko — a
// daemonless builder that does userspace layer extraction, so it needs no Docker
// daemon, no host socket, and no elevated capabilities: only root + a writable
// rootfs, which forge grants via a privileged runner class on a kernel-isolated
// backend (kata or gvisor). The build context is normally a shared workspace volume
// a prior checkout step populated.
type BuildSpec struct {
	// Context is the build-context directory (default /workspace — usually a shared
	// volume). Dockerfile is the Dockerfile path relative to Context (default
	// "Dockerfile").
	Context    string `json:"context,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// Destinations are the image refs to build and push (e.g.
	// registry.example.com/acme/app:1.2.3). Required unless NoPush.
	Destinations []string `json:"destinations,omitempty"`
	// BuildArgs are Dockerfile ARG values. Target selects a stage in a multi-stage
	// build (optional).
	BuildArgs map[string]string `json:"build_args,omitempty"`
	Target    string            `json:"target,omitempty"`
	// RegistryAuth names the env var (set via a secret_ref) holding a Docker
	// config.json used to authenticate the push. Default REGISTRY_AUTH. Required when
	// pushing.
	RegistryAuth string `json:"registry_auth,omitempty"`
	// NoPush builds without pushing (a validation/PR build).
	NoPush bool `json:"no_push,omitempty"`
}

// CheckoutSpec configures forge's actions/checkout-style clone. Before running the
// command, forge `git clone`s the repo whose authenticated URL lives in the env var
// named by Env (default GIT_CLONE_URL — usually set by a git:/gitea: secret_ref)
// into Path and cd's into it, so the command starts inside a checked-out repo. A
// clone failure fails the whole execution. Requires `git` in the image and a shell
// (`-c`) command; both are enforced at submit time.
type CheckoutSpec struct {
	// Env names the env var holding the clone URL. Default GIT_CLONE_URL.
	Env string `json:"env,omitempty"`
	// Path is the directory (relative to the image's working dir) to clone into and
	// cd into. Default: the repo name derived from the git:/gitea: ref, else "repo".
	Path string `json:"path,omitempty"`
	// Ref is an optional branch or tag to check out (git clone --branch). Empty
	// clones the remote's default branch. Commit SHAs are not supported here (use a
	// full clone + your own `git checkout` in the command).
	Ref string `json:"ref,omitempty"`
	// Depth is the git clone --depth. Nil defaults to 1 (shallow, like
	// actions/checkout); 0 means a full clone; a positive value sets that depth.
	Depth *int `json:"depth,omitempty"`
}

// CopySpec configures a volume-copy execution: forge copies the declared paths from
// one or more source volumes into the single destination volume (the mount marked
// workdir: true), all attached via `volumes`. It is the sandbox-side primitive behind
// scatter/gather, needing neither a CSI clone nor ReadWriteMany — every source and the
// destination mount into one copy pod, so they attach to a single node, which RWO
// allows. Two shapes:
//   - clone (Disjoint false): one source, whole tree — seed a per-leg volume from the
//     base workspace so parallel legs never share a PVC.
//   - gather (Disjoint true): N sources, each with its declared owned paths — union
//     them back into the base, failing if two legs claim the same path.
type CopySpec struct {
	// Sources are the read-only volumes to copy from and, per source, the relative
	// paths within it to copy (default: the whole tree). Each Volume must name a mount
	// present in `volumes` that is not the destination.
	Sources []CopySource `json:"sources"`
	// Disjoint fails the copy if two sources would write the same relative path,
	// turning a multi-leg gather into a checked union instead of a silent
	// last-writer-wins. Leave off for a single-source clone.
	Disjoint bool `json:"disjoint,omitempty"`
}

// CopySource names one source volume mount and the relative paths to copy from it.
type CopySource struct {
	// Volume matches a VolumeMount.Name in the request's `volumes`.
	Volume string `json:"volume"`
	// Paths are relative to the source's mount root; "." or empty means the whole
	// tree (not allowed in disjoint/gather mode, which unions declared paths).
	Paths []string `json:"paths,omitempty"`
}

// ResolveSpec configures a resolve-paths execution: forge scans an attached volume and
// captures the entries whose path (relative to the volume root) matches Regex, filtered
// to directories or files. The matched, sorted, newline-separated list is captured as
// the output variable named by Output (default "paths"), which a workflow reads as the
// step output to drive a scatter's fan-out. Forge supplies the image and command.
type ResolveSpec struct {
	// Volume names the attached volume to scan (a `volumes[].name`). Defaults to the
	// mount marked workdir: true, else the single attached mount.
	Volume string `json:"volume,omitempty"`
	// Regex filters matching entries (POSIX ERE / grep -E), applied to each entry's
	// path relative to the volume root. Required.
	Regex string `json:"regex"`
	// Mode selects what to match: "dir" (default) or "file".
	Mode string `json:"mode,omitempty"`
	// MaxDepth bounds how deep to descend (find -maxdepth). 0/unset = unlimited.
	MaxDepth int `json:"max_depth,omitempty"`
	// Output names the env var / output key the matched-path list is captured into.
	// Default "paths".
	Output string `json:"output,omitempty"`
}

// RunnerClass defines the resource limits for a named execution tier. Every
// backend (docker/k8s/kata/gvisor) reads MemoryMB/CPUMillicores/PidsLimit/TmpfsMB.
// DiskGB is currently unused by all backends — retained on the schema/API so a
// future VM-style backend can reinstate a per-class disk size without a migration.
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
	// is only meaningful on kernel-isolated backends, where the guest/userspace
	// kernel — not the container — is the isolation boundary:
	//   - kata: the k8s runtime applies it via the pod/container securityContext
	//     inside a microVM.
	//   - gvisor: the k8s runtime applies it the same way; the gVisor Sentry
	//     contains the job's root.
	// validatePrivilegedBackend requires a privileged class to target such a backend.
	// On shared-kernel container backends (docker/kubernetes/runc) it is ignored
	// at runtime: root in a shared-kernel container is an escape risk, so the
	// locked-down sandbox is always applied there regardless of this flag.
	Privileged bool `gorm:"not null;default:false"     json:"privileged"`
}

// RuntimeBackend is an admin-managed runtime target. Type selects the runtime
// implementation (docker|kubernetes|kata|gvisor); Config holds non-secret settings
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

// ConcurrencyLimit caps how many executions may be RUNNING at once for a single
// scope — one org (Scope="org", ScopeID=org_id) or one user (Scope="user",
// ScopeID=user_id). It is an admin-managed override of the global env defaults
// (FORGE_MAX_CONCURRENT_PER_ORG / FORGE_MAX_CONCURRENT_PER_USER): a row sets an
// explicit cap for one org/user and its absence falls back to the default.
// MaxConcurrent <= 0 means unlimited.
//
// Limits are enforced at claim time (claimPendingExecution), not at submit: an
// execution over its scope's cap stays pending (queued) until a running peer
// finishes, rather than being rejected. Forge workers are slow to provision, so
// excess work queues and drains instead of failing — which is the whole point of
// the cap (it lets an operator size the worker fleet to a known ceiling).
type ConcurrencyLimit struct {
	Scope         string    `gorm:"column:scope;primaryKey"                  json:"scope"`
	ScopeID       string    `gorm:"column:scope_id;primaryKey"               json:"scope_id"`
	MaxConcurrent int       `gorm:"column:max_concurrent;not null"           json:"max_concurrent"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null;default:now()" json:"updated_at"`
}

type submitRequest struct {
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env"`
	Timeout     int64             `json:"timeout"`
	RunnerClass string            `json:"runner_class"`
	Project     string            `json:"project"`
	// OutputEnv names env vars to capture from the command's shell as structured
	// output (see Execution.OutputEnv).
	OutputEnv []string `json:"output_env"`
	// SecretRefs maps a target env var NAME to a credential reference. Supported
	// schemes: "secret:<name>" resolves a gatekeeper org secret (e.g. a git SSH
	// deploy key or token for GitHub/Bitbucket); "git:<repo-url>" asks the core git
	// credential-broker to mint clone credentials for the URL's backend;
	// "gitea:<owner>/<repo>" mints a short-lived Forgejo clone URL. Resolved at
	// dispatch, injected into the runtime env, and never persisted.
	SecretRefs map[string]string `json:"secret_refs"`
	// Checkout, when set, asks forge to clone the repo referenced by a secret_ref
	// (or plain env var) into the working dir before running the command — see
	// CheckoutSpec.
	Checkout *CheckoutSpec `json:"checkout"`
	// Volumes attaches shared workspace volumes (created via POST /volumes) to the
	// sandbox — see VolumeMount. Each referenced volume must already exist and belong
	// to the caller.
	Volumes []VolumeMount `json:"volumes"`
	// Build, when set, makes this an image-build execution: forge derives the image
	// (its Kaniko builder) and command from the spec, so `image` and `command` are
	// ignored. Requires a privileged runner class on a kata/gvisor backend. See
	// BuildSpec.
	Build *BuildSpec `json:"build"`
	// Copy, when set, makes this a volume-copy execution: forge derives the image
	// (its minimal runner image) and command (a synthesised `cp` script) from the
	// spec, copying declared paths between the attached `volumes`. It is the primitive
	// behind scatter-clone and gather — see CopySpec.
	Copy *CopySpec `json:"copy"`
	// Resolve, when set, makes this a resolve-paths execution: forge scans an attached
	// volume and captures the paths matching a regex (as structured output), the
	// fan-out set behind a scatter — see ResolveSpec.
	Resolve *ResolveSpec `json:"resolve"`
	// Artifact, when set, makes this an artifact transfer: forge derives the image
	// (its minimal runner image) and command (a synthesised tar|curl) from the spec,
	// moving a path between an attached volume and the artifact store. It is how a
	// build cache or a binary survives a run — see ArtifactSpec.
	Artifact *ArtifactSpec `json:"artifact"`
}

// RunResult holds the output of a completed container run. ExitCode is a pointer
// so a runtime failure that never produced an exit code (image pull, container
// create, cancellation, timeout) is recorded as NULL rather than a misleading 0.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode *int
	// Outputs is the captured OutputEnv NAME→value map, parsed out of stdout after
	// the run (nil when none were requested or the emit never ran).
	Outputs map[string]string
	// MemoryUsedMB is the peak container memory in MB, nil when the runtime could
	// not measure it. MemoryLimitMB is the runner class's memory ceiling.
	MemoryUsedMB  *int64
	MemoryLimitMB *int64
}
