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
	// StatusAwaitingApproval is a non-terminal, resumable state: a run paused on a
	// manual-approval step. It is excluded from Dequeue (only 'pending' is claimed)
	// and from stuck-run recovery (only 'running' is reaped), so a paused run
	// survives a worker restart untouched until someone approves or rejects it.
	StatusAwaitingApproval = "awaiting_approval"
)

// ActionApproval is a built-in gate action (like ActionHTTP it is not a registry
// catalog action). A step with this action pauses the run in StatusAwaitingApproval
// until an authorized user approves or rejects it via the run approval API.
const ActionApproval = "approval"

const (
	maxBodyBytes    = 64 * 1024
	maxResponseBody = 1 * 1024 * 1024 // 1 MB stored per step
	defaultTimeout  = int64(30)
	maxTimeout      = int64(3600)
	maxSteps        = 50
)

// ActionHTTP is the escape hatch for raw HTTP calls to any registered service.
const ActionHTTP = "http"

// ActionWorkflowsTrigger runs another pipeline as a sub-run and captures its
// declared outputs as the step's output. It is an async catalog action (POST /runs
// to create the sub-run, then poll GET /runs/{id} to a terminal state), so a matrix
// or parallel group over it fans out whole sub-pipelines with no extra machinery.
const ActionWorkflowsTrigger = "workflows/trigger"

// maxRunDepth caps how deep pipeline-triggers-pipeline nesting may go so a cycle
// (A→B→A) or a runaway fan-out can't spawn unbounded sub-runs. A top-level run is
// depth 0; each sub-run is its parent's depth + 1 and a create past this cap is
// rejected.
const maxRunDepth = 8

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
	// OutputMapField names a response field holding an object (e.g. forge's captured
	// output_env map). When set, it is the ONLY source of the SUCCESS step output:
	// that object — JSON-encoded — becomes the output (so a later step can reference a
	// key via ${steps.NAME.output.KEY}), and OutputField is NOT used as a stdout
	// fallback (an empty/absent map means the step has no output). OutputField still
	// applies on the failure path, so a failed step can surface its stdout.
	OutputMapField string   `json:"output_map_field,omitempty"`
	ErrorFields    []string `json:"error_fields"`
}

// BodyTransform rewrites a With key before the payload is sent to the service.
// If Wrap is non-empty the string value is appended to form a string slice.
type BodyTransform struct {
	FromKey string   `json:"from_key"`
	ToKey   string   `json:"to_key"`
	Wrap    []string `json:"wrap,omitempty"`
}

// ActionDef is a callable workflow action loaded from the registry action catalog.
// Summary/Description are human-facing catalog metadata surfaced in the portal and
// CLI; they do not affect execution.
type ActionDef struct {
	Name               string          `json:"name"`
	Summary            string          `json:"summary,omitempty"`
	Description        string          `json:"description,omitempty"`
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

// maxMatrixValues caps how many executions a single matrix step may fan out to,
// so a runaway input list can't spawn an unbounded number of step runs.
const maxMatrixValues = 50

// MatrixConfig fans a single step out into one execution per value in a list.
// Each execution runs the same step with ${matrix.<Var>} (and the generic
// ${matrix.value}) bound to that value, so the same With template targets a
// different input each time. The list is either the literal Values or, when
// ValuesFrom is set, resolved at run time from a ${...} reference that yields a
// JSON array, a comma-separated string, or a whitespace-separated linux-style list
// (e.g. "${inputs.regions}" or a step that emits `ls`-style output). Matrix
// executions run concurrently (capped by maxParallelSteps, or by MaxConcurrent when
// it is set to a smaller value) and the step's aggregated output is the JSON array
// of each execution's output.
type MatrixConfig struct {
	Var        string   `json:"var"`
	Values     []string `json:"values,omitempty"`
	ValuesFrom string   `json:"values_from,omitempty"`
	// MaxConcurrent caps how many of the fan-out executions run at once. 0 (unset)
	// runs the default fan-out concurrency (3); a positive value sets a different cap,
	// up to the global maxParallelSteps ceiling (values above it have no extra effect).
	// Lower it for a matrix over resource-heavy runners (large sandboxes, image builds)
	// so it does not exhaust cluster capacity; raise it (up to the ceiling) to fan out
	// wider than the default.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// Sequential runs the values strictly one at a time (concurrency 1), instead of
	// the default parallel fan-out — the simple form of MaxConcurrent: 1 for when a
	// matrix must not launch its executions simultaneously (e.g. a shared external
	// resource, or to avoid overwhelming a small cluster). Takes precedence over
	// MaxConcurrent when set.
	Sequential bool `json:"sequential,omitempty"`
}

// ScatterConfig fans a step out over the paths in a shared workspace that match a
// regex: forge resolves the matching directories/files, each becomes one parallel leg
// running on its OWN clone of the workspace (so legs never share a PVC and never
// Multi-Attach across nodes), and after all legs finish their declared owned outputs
// are gathered back into the base workspace as a disjoint union. It is the "partition
// the build, run in parallel, recombine" pattern — a matrix that also clones and
// recombines the workspace. The matched path is bound to ${scatter.path} in the leg's
// With. Legs run concurrently (capped by MaxConcurrent, like a matrix).
type ScatterConfig struct {
	// Volume is the base workspace volume name to scan and clone per leg (a
	// create-volume step must have provisioned it earlier). Default "workspace".
	Volume string `json:"volume,omitempty"`
	// MountPath is where the (cloned) workspace mounts in resolve, each leg, and the
	// gather. Default "/workspace".
	MountPath string `json:"mount_path,omitempty"`
	// The fan-out path set is produced one of two ways (exactly one is required):
	//   - Regex scans the base workspace via forge/resolve-paths and fans out over the
	//     matching entries. Mode is "dir" (default) or "file", MaxDepth bounds descent.
	//   - PathsFrom skips the scan and takes the list directly from a ${...} reference
	//     resolved at run time (e.g. "${inputs.services}" or "${steps.discover.output}"),
	//     parsed like a matrix values_from (JSON array, or comma/whitespace-separated).
	// Either source is substituted for ${inputs.*}/${steps.*} before use, and each
	// resulting value is bound to ${scatter.path} in one leg (still on its own clone).
	Regex     string `json:"regex,omitempty"`
	Mode      string `json:"mode,omitempty"`
	MaxDepth  int    `json:"max_depth,omitempty"`
	PathsFrom string `json:"paths_from,omitempty"`
	// Outputs are the paths each leg owns, unioned back into the base workspace after
	// all legs finish (gather). They may reference ${scatter.path}; overlap across legs
	// fails the gather. Empty = gather nothing (legs are independent; collect their
	// results via output_env instead).
	Outputs []string `json:"outputs,omitempty"`
	// ShareBase mounts the base workspace READ-ONLY into every leg instead of giving each
	// leg its own clone — no per-leg volume, and no per-leg volume-copy (which is itself a
	// sandbox pod: on a kernel-isolated backend that is one microVM boot per leg, just to
	// duplicate a source tree none of the legs will write to).
	//
	// Use it for read-only fan-outs — compile/test/lint each partition — where legs write
	// nothing back into the workspace. The saving is not disk (a source tree is small); it
	// is the N clone volumes and N copy pods. It also lets legs share ONE writable cache
	// volume (declared in the step's own `volumes`), so they reuse compiled artifacts
	// instead of each starting from a cold cache — the thing a per-leg clone makes
	// impossible, since a cache seeded into the base would be duplicated N times.
	//
	// The trade-off is deliberate and is why this is opt-in: every leg attaches the SAME
	// PVC, which is exactly what cloning exists to avoid. On ReadWriteOnce block storage
	// that only works while all legs land on ONE node; across nodes it Multi-Attach fails.
	// Safe on a single-node cluster, or on a ReadOnlyMany/ReadWriteMany storage class.
	//
	// Mutually exclusive with Outputs: there are no per-leg volumes, so there is nothing
	// to gather. Legs return results via output_env.
	ShareBase bool `json:"share_base,omitempty"`
	// SizeMB / Medium size the per-leg clone volumes (should hold the workspace copy).
	// Empty = forge's create-volume defaults. Ignored when ShareBase is set.
	SizeMB int64  `json:"size_mb,omitempty"`
	Medium string `json:"medium,omitempty"`
	// MaxConcurrent caps how many legs run at once. 0 = the default fan-out
	// concurrency (3); a positive value sets a different cap up to the global ceiling.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
}

// ApprovalGate is an inline manual-approval pause declared directly on a pipeline
// step ref — no separate Step row is required. When a ref carries one (and no
// StepID), the run pauses at that position in StatusAwaitingApproval until an
// authorized user approves or rejects it. Message is shown to approvers; Approvers
// is an optional username allow-list. (Equivalent to a step with Action=approval,
// but the gate lives on the pipeline, not as a reusable step.)
type ApprovalGate struct {
	Message   string   `json:"message,omitempty"`
	Approvers []string `json:"approvers,omitempty"`
}

// WorkflowStepRef records how a step is used within a specific workflow. A ref is
// exactly ONE of three kinds:
//   - a reference to a stored, reusable step (StepID set), OR
//   - an INLINE step whose definition lives on the ref itself (Action set, no
//     StepID) — the step is private to this pipeline and not in the steps table, OR
//   - an inline Approval gate (Approval set).
//
// A step ref (reference or inline) may additionally carry a parallel group or a
// matrix (mutually exclusive); a gate is always solo. Stored as part of the
// workflow's `steps` JSON column, so no field needs its own DB column.
type WorkflowStepRef struct {
	StepID string `json:"step_id,omitempty"`
	// Action is set only for an INLINE step (StepID empty): the action the step runs,
	// mirroring Step.Action. When set, Name is the step's name and With is its full
	// config (not an override — there is no stored step to merge over).
	Action string `json:"action,omitempty"`
	// Timeout is the per-step timeout in seconds for an INLINE step (mirrors
	// Step.Timeout; 0 = the service default). Ignored for a reference or a gate.
	Timeout int64 `json:"timeout,omitempty"`
	// Name is the step name for an inline step. For a stored-step reference it instead
	// OVERRIDES the display/reference name for THIS occurrence, so the same step can
	// appear more than once with distinct names, each referenced unambiguously as
	// ${steps.<name>.output}. Empty = use the step definition's own name (or "approval"
	// for a gate).
	Name string `json:"name,omitempty"`
	// With is the full config for an inline step. For a stored-step reference it instead
	// holds per-occurrence overrides for the step's With config, merged over the step
	// definition's With at run time (these keys win). This is how a pipeline wires a
	// step's inputs to earlier steps' outputs — e.g. With:{"env":{"TARGET":
	// "${steps.build.output}"}} — without editing the shared step. The values support
	// the same ${...} substitution as any With value.
	With   map[string]any `json:"with,omitempty"`
	Matrix *MatrixConfig  `json:"matrix,omitempty"`
	// Scatter fans this step out over the regex-matched paths of a shared workspace,
	// each leg on its own clone, gathering owned outputs back afterward. Mutually
	// exclusive with Matrix — see ScatterConfig.
	Scatter  *ScatterConfig `json:"scatter,omitempty"`
	Approval *ApprovalGate  `json:"approval,omitempty"`
	// MapID puts this step inside the named map region (see MapDef): the region's
	// whole subgraph is repeated once per value, so unlike Matrix — which repeats a
	// single step — a map body can be several routed steps. Mutually exclusive with
	// Matrix/Scatter on the same step, since those are the step's own fan-out and
	// would nest inside the region's.
	MapID string `json:"map_id,omitempty"`
}

// MapDef declares a map region: a SUBGRAPH repeated once per value.
//
// It is the region-scoped counterpart of MatrixConfig. A matrix fans out one step;
// a map fans out every step carrying its MapID, along with the routes between them —
// so an iteration can build, then test, then conditionally push, which is what a
// matrix cannot express.
//
// Volume, when set, gives each iteration its OWN CLONE of that workspace (the same
// mechanism scatter uses). This is the reason the region exists rather than being
// composed from a matrix over sub-pipelines: volumes are ReadWriteOnce, so parallel
// iterations cannot share one checkout, and nothing else both clones per iteration
// and runs a multi-step body.
type MapDef struct {
	// ID is the region's name, referenced by WorkflowStepRef.MapID.
	ID string `json:"id"`
	// Var is the binding name: each iteration sees its value as ${map.<var>}.
	Var string `json:"var"`
	// Exactly one of Values / ValuesFrom, mirroring MatrixConfig. ValuesFrom is a
	// ${...} reference resolved when the region starts (typically an earlier step's
	// output), which is what makes the fan-out dynamic.
	Values     []string `json:"values,omitempty"`
	ValuesFrom string   `json:"values_from,omitempty"`
	// MaxConcurrent caps iterations in flight (0 = defaultFanoutConcurrency);
	// Sequential pins it to 1.
	MaxConcurrent int  `json:"max_concurrent,omitempty"`
	Sequential    bool `json:"sequential,omitempty"`
	// Volume names the base workspace to clone per iteration; empty = no clone (the
	// body then attaches whatever volumes it declares itself). MountPath/SizeMB/
	// Medium configure the clone, and Outputs are the paths each iteration owns,
	// gathered back into the base afterward — all as in ScatterConfig.
	Volume    string   `json:"volume,omitempty"`
	MountPath string   `json:"mount_path,omitempty"`
	SizeMB    int64    `json:"size_mb,omitempty"`
	Medium    string   `json:"medium,omitempty"`
	Outputs   []string `json:"outputs,omitempty"`
}

// maxMapValues caps a map region's fan-out, matching maxMatrixValues: a region
// iteration is far heavier than a matrix leg (a whole subgraph, optionally a volume
// clone), so the same ceiling is deliberately conservative.
const maxMapValues = 50

// WorkflowStep enriches a WorkflowStepRef with the full Step definition.
// It is assembled at request/execution time and never stored in the DB. For an
// inline approval gate the Step is synthesised (Action=approval) and Approval
// carries the gate config back out so the editor can round-trip it.
type WorkflowStep struct {
	Step
	Matrix   *MatrixConfig  `json:"matrix,omitempty"`
	Scatter  *ScatterConfig `json:"scatter,omitempty"`
	Approval *ApprovalGate  `json:"approval,omitempty"`
	// MapID is the map region this step belongs to — see MapDef.
	MapID string `json:"map_id,omitempty"`
}

// WorkflowInputDef declares a named input a pipeline accepts. Default is applied
// when the trigger omits the input; a Required input left unset (with no default)
// fails the trigger with 400. Inputs resolve as ${inputs.NAME} in step With values.
type WorkflowInputDef struct {
	Name        string `json:"name"`
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// WorkflowOutputDef declares a named output a pipeline produces. Value is a ${...}
// template (typically ${steps.STEP.output.KEY}) resolved against the run's step
// outputs when the run completes; the resolved name→value map becomes the run's
// outputs, surfaced to a parent pipeline that triggered this one via a
// workflows/trigger step (that step's output is this map).
type WorkflowOutputDef struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Workflow is a named, ordered pipeline of step references.
// StepRefs is the authoritative DB column (JSON array of WorkflowStepRef).
// Steps is populated at query time by joining against the steps table.
// RoleID is the gatekeeper role provisioned at creation time; it scopes run
// tokens to only the permissions the workflow's steps actually require.
type Workflow struct {
	WorkflowID  string `json:"workflow_id"  gorm:"column:workflow_id;primaryKey"`
	Name        string `json:"name"         gorm:"column:name"`
	Description string `json:"description"  gorm:"column:description;default:''"`
	CreatedBy   string `json:"created_by"   gorm:"column:created_by"`
	OrgID       string `json:"org_id"       gorm:"column:org_id;default:''"`
	Project     string `json:"project,omitempty" gorm:"column:project;default:''"`
	RoleID      string `json:"role_id,omitempty" gorm:"column:role_id;default:''"`
	// RolePermsVersion records which version of collectWorkflowPermissions built
	// RoleID. The role is provisioned once and reused across runs, so when the
	// derivation logic changes (constant bumped) a workflow with an older version
	// re-provisions on its next trigger — see handleTriggerRun. Default 0 means
	// "pre-versioning"; AutoMigrate backfills existing rows to 0.
	RolePermsVersion int       `json:"-"            gorm:"column:role_perms_version;default:0"`
	Active           bool      `json:"active"       gorm:"column:active;default:true"`
	CreatedAt        time.Time `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt        time.Time `json:"updated_at"   gorm:"column:updated_at"`
	// Inputs/Outputs declare the pipeline's interface — JSON columns, so AutoMigrate
	// adds them with no manual migration and existing rows read back as empty.
	Inputs  []WorkflowInputDef  `json:"inputs,omitempty"  gorm:"column:inputs;serializer:json"`
	Outputs []WorkflowOutputDef `json:"outputs,omitempty" gorm:"column:outputs;serializer:json"`
	// Maps declare the map regions this workflow contains — see MapDef. A step joins
	// a region by naming it in WorkflowStepRef.MapID.
	Maps []MapDef `json:"maps,omitempty" gorm:"column:maps;serializer:json"`
	// Routes are the explicit edges between steps — see WorkflowRoute. Empty means
	// the edges are DERIVED at load time (deriveRoutes) as a plain chain in array
	// order, which is why a pipeline authored before routes existed needs no
	// migration: an array with no routes IS a sequence.
	//
	// Routes are the ONLY way to express parallelism. There is no parallel_group:
	// two edges out of one node is a fork, and it composes with joins and conditions
	// in a way a positional run-length field never could. Fan-out WITHIN a node
	// (matrix/scatter/map) is that node's own concern and carries its own
	// max_concurrent.
	// A JSON column, so AutoMigrate adds it and existing rows read back empty.
	Routes []WorkflowRoute `json:"routes,omitempty" gorm:"column:routes;serializer:json"`
	// Ticket opts every run of this workflow into being mirrored to a ticket — see
	// TicketConfig. Nil/disabled means no ticket, which is why existing workflows are
	// untouched. A JSON column, so AutoMigrate adds it and existing rows read back nil.
	Ticket   *TicketConfig     `json:"ticket,omitempty" gorm:"column:ticket;serializer:json"`
	StepRefs []WorkflowStepRef `json:"-"            gorm:"column:steps;serializer:json"`
	Steps    []WorkflowStep    `json:"steps"        gorm:"-"`
}

func (Workflow) TableName() string { return "workflows" }

// WorkflowRun is a single triggered execution of a Workflow.
// Token holds a short-lived run-scoped JWT minted by gatekeeper at trigger time;
// it is never the triggering user's own session token. RunSessionID tracks the
// underlying gatekeeper session so it can be revoked on terminal state.
type WorkflowRun struct {
	RunID       string            `json:"run_id"       gorm:"column:run_id;primaryKey"`
	WorkflowID  string            `json:"workflow_id"  gorm:"column:workflow_id"`
	TriggeredBy string            `json:"triggered_by" gorm:"column:triggered_by"`
	OrgID       string            `json:"org_id"       gorm:"column:org_id;default:''"`
	Project     string            `json:"project,omitempty" gorm:"column:project;default:''"`
	Status      string            `json:"status"       gorm:"column:status;default:'pending'"`
	CurrentStep int               `json:"current_step" gorm:"column:current_step;default:0"`
	Inputs      map[string]string `json:"inputs"       gorm:"column:inputs;serializer:json"`
	// Outputs is the resolved pipeline-output map, computed from the declared
	// WorkflowOutputDefs when the run completes; empty until then. Surfaced to a
	// parent run as the workflows/trigger step's output.
	Outputs map[string]string `json:"outputs,omitempty" gorm:"column:outputs;serializer:json"`
	// Depth is the sub-pipeline nesting depth (0 for a top-level run); ParentRunID
	// links a sub-run to the run whose workflows/trigger step started it.
	Depth       int    `json:"depth,omitempty"         gorm:"column:depth;default:0"`
	ParentRunID string `json:"parent_run_id,omitempty" gorm:"column:parent_run_id;default:''"`
	// TicketID is the ticket mirroring this run (see TicketConfig), empty when the
	// workflow did not opt in. Recorded on the RUN, not just on the ticket, for two
	// reasons: a run that pauses on an approval gate is re-executed from the top when
	// it resumes, and without this it would open a second ticket for the same run; and
	// it is the link a client follows from a run to its ticket, which the tickets API
	// cannot serve in reverse (it has no run_id filter).
	TicketID     string `json:"ticket_id,omitempty" gorm:"column:ticket_id;default:''"`
	Token        string `json:"-"            gorm:"column:token"`
	RunSessionID string `json:"-"            gorm:"column:run_session_id"`
	// RoleID is the workflow role the run's token was scoped to at trigger time.
	// Recorded per-run (not just on the Workflow) because the workflow's role is
	// re-provisioned on update: a run keeps authenticating against the role it started
	// with, so that role must not be deleted while the run is still active, and it is
	// garbage-collected once the last run using it finishes. AutoMigrate backfills
	// existing rows to '' (harmless — those runs are already terminal).
	RoleID    string            `json:"-"            gorm:"column:role_id;default:''"`
	StepRuns  []WorkflowStepRun `json:"step_runs"    gorm:"-"`
	CreatedAt time.Time         `json:"created_at"   gorm:"column:created_at"`
	StartedAt *time.Time        `json:"started_at,omitempty" gorm:"column:started_at"`
	EndedAt   *time.Time        `json:"ended_at,omitempty"   gorm:"column:ended_at"`
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
	// Logs is the step's human-readable execution log — the backing action's stdout
	// (forge: the command's stdout), captured for display on every terminal outcome
	// (success, failure, cancel) so a step's stdout is always viewable in the run
	// view. Distinct from Output: stdout is never the consumable ${steps.NAME.output}
	// (that is the output_env map). NULL only when the action exposes no stdout (e.g.
	// the http escape hatch); on failure Output then carries the error reason alone.
	Logs *string `json:"logs,omitempty" gorm:"column:logs"`
	// MemoryUsedMB/MemoryLimitMB are carried through from the forge execution a
	// forge-backed step ran (NULL for non-forge steps and when forge could not
	// measure usage). See forge's Execution for how they are captured.
	MemoryUsedMB  *int64     `json:"memory_used_mb,omitempty"  gorm:"column:memory_used_mb"`
	MemoryLimitMB *int64     `json:"memory_limit_mb,omitempty" gorm:"column:memory_limit_mb"`
	StartedAt     *time.Time `json:"started_at,omitempty"   gorm:"column:started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"     gorm:"column:ended_at"`
}

func (WorkflowStepRun) TableName() string { return "workflow_step_runs" }
