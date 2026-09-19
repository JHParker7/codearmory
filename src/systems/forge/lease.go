package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A lease is a sandbox that outlives a single command.
//
// The one-shot execution model — schedule a sandbox, run one command, tear it
// down — charges the full startup cost to every command. That is the right trade
// for a caller running one command, and the wrong one for a caller running a
// dozen against the same checkout: it pays the boot a dozen times. On a
// kernel-isolated backend the boot is seconds of microVM start, which then
// dominates the wall time of work whose commands are individually short.
//
// A lease inverts that: forge boots the sandbox once, holds it idle, and runs
// many commands inside it. Each command is still an ordinary Execution — same
// row, same status polling, same output capture, same permission check — it
// merely names a lease instead of an image, and the worker execs into the held
// sandbox rather than creating a job. The client contract for running a command
// is therefore unchanged, and the new concept is confined to dispatch.
//
// What a lease deliberately does NOT do is get recycled between holders. It is
// created for one unit of work, dies with it, and is never handed to a second
// caller. Reuse would trade away the property the one-shot model buys — a
// compromised sandbox dies with the command that compromised it — for a foothold
// that survives into someone else's work. The saving here comes from removing
// repeated boots WITHIN one task, never from sharing a sandbox ACROSS tasks, and
// the two are easy to conflate right up until something goes wrong.
const (
	// leaseStarting covers boot: the sandbox is being created and any checkout is
	// running. Execs against a starting lease are refused rather than queued, so a
	// caller that skipped the readiness poll gets a clear error instead of a hang.
	leaseStarting = "starting"
	// leaseReady means the sandbox is up and accepting execs.
	leaseReady = "ready"
	// leaseStopped is terminal and normal: released by its holder, or reaped for
	// age or idleness.
	leaseStopped = "stopped"
	// leaseFailed is terminal and abnormal: the sandbox never came up, or its
	// checkout failed. Detail carries why.
	leaseFailed = "failed"
)

// leaseWorkDir is where a lease's commands run and where its checkout lands.
//
// A lease pins ONE working directory for its whole life, unlike a one-shot
// execution which can clone into a repo-name subdirectory and cd there for the
// single command that follows. That cd cannot survive into a later exec — each
// exec is a fresh process in the container — so a lease that cloned into a
// subdirectory would silently run every subsequent command one level above its
// own checkout. Pinning the directory and cloning into its root removes the
// question entirely.
const leaseWorkDir = "/workspace"

// Lease is the persistent record for a held sandbox.
type Lease struct {
	LeaseID string `gorm:"column:lease_id;primaryKey"        json:"lease_id"`
	UserID  string `gorm:"column:user_id;not null;index"     json:"user_id"`
	OrgID   string `gorm:"column:org_id;not null;default:''" json:"org_id,omitempty"`
	// Project/ProjectID/ProjectNamespace mirror Execution's: a slug that resolves to
	// a real gatekeeper project widens access to its members, otherwise it stays a
	// free-text label and access stays governed by UserID.
	Project          string `gorm:"column:project;not null;default:''"           json:"project,omitempty"`
	ProjectID        string `gorm:"column:project_id;not null;default:''"        json:"project_id,omitempty"`
	ProjectNamespace string `gorm:"column:project_namespace;not null;default:''" json:"project_namespace,omitempty"`

	Image       string `gorm:"column:image;not null"                          json:"image"`
	RunnerClass string `gorm:"column:runner_class;not null;default:standard"  json:"runner_class"`
	// Backend is snapshotted from the runner class at create time for the same
	// reason an execution snapshots it: re-pointing the class must not move an
	// already-running sandbox to a different runtime.
	Backend string `gorm:"column:backend;not null;default:default" json:"backend"`

	// Env and SecretRefs are fixed for the lease's whole life, because a container's
	// environment is set when it starts and a later exec cannot change it. Resolved
	// credentials are injected into the sandbox at boot and, as everywhere else in
	// forge, never persisted or logged — only the references are stored here.
	//
	// This is why an exec against a lease may not carry its own secret_refs: honouring
	// them would mean passing the resolved value through the exec's argv, where it is
	// readable from /proc by anything else in the sandbox. A lease clones its repo at
	// boot with the credential in the pod environment, which is the same exposure a
	// one-shot execution already has, and no worse.
	Env        map[string]string `gorm:"column:env;type:jsonb;not null;default:'{}';serializer:json"         json:"env,omitempty"`
	SecretRefs map[string]string `gorm:"column:secret_refs;type:jsonb;not null;default:'{}';serializer:json" json:"secret_refs,omitempty"`

	// Volumes are attached at boot and stay attached; every exec sees them. A mount
	// that sets workdir replaces the default emptyDir at leaseWorkDir, so a lease can
	// keep its working tree on a shared volume that outlives it.
	Volumes []VolumeMount `gorm:"column:volumes;type:jsonb;not null;default:'[]';serializer:json" json:"volumes,omitempty"`
	// Checkout, when set, clones the repo into leaseWorkDir ONCE at boot rather than
	// per command. This is most of the point of a lease for an agent: ten commands
	// against one checkout instead of ten clones.
	Checkout *CheckoutSpec `gorm:"column:checkout;type:jsonb;serializer:json" json:"checkout,omitempty"`

	Status string `gorm:"column:status;not null;default:starting" json:"status"`
	// Detail explains a failed lease. Empty otherwise.
	Detail string `gorm:"column:detail;not null;default:''" json:"detail,omitempty"`

	// IdleTimeoutSecs stops a lease that has gone quiet, and MaxLifetimeSecs stops
	// one that has not. Both exist because a lease is memory held open: a caller that
	// crashes between execs would otherwise pin a sandbox until forge restarted.
	IdleTimeoutSecs int64 `gorm:"column:idle_timeout_secs;not null;default:300"  json:"idle_timeout"`
	MaxLifetimeSecs int64 `gorm:"column:max_lifetime_secs;not null;default:3600" json:"max_lifetime"`

	CreatedAt time.Time  `gorm:"column:created_at;not null;default:now()" json:"created_at"`
	StartedAt *time.Time `gorm:"column:started_at"                        json:"started_at,omitempty"`
	// LastUsedAt is bumped when an exec is dispatched against the lease; it is what
	// the idle timeout measures from.
	LastUsedAt *time.Time `gorm:"column:last_used_at" json:"last_used_at,omitempty"`
	EndedAt    *time.Time `gorm:"column:ended_at"     json:"ended_at,omitempty"`
}

func (Lease) TableName() string { return "leases" }

// active reports whether the lease still owns a sandbox.
func (l Lease) active() bool { return l.Status == leaseStarting || l.Status == leaseReady }

// resourceName is the concrete backend object name for the lease's sandbox. It is
// derived rather than stored so a reaper can address a sandbox from the row alone,
// including after a restart that lost all in-memory state.
func (l Lease) resourceName() string { return "forge-lease-" + l.LeaseID }

// workDir is where the lease's commands run: a volume mount that asked to be the
// working directory, else the default. Kept as a method so the runtimes and the
// exec path cannot disagree about it.
func (l Lease) workDir() string {
	for _, m := range l.Volumes {
		if m.Workdir && m.MountPath != "" {
			return m.MountPath
		}
	}
	return leaseWorkDir
}

// idleDeadline and hardDeadline are when the reaper may stop this lease. A lease
// that never recorded a use is measured from when it started, so a caller that
// creates a lease and abandons it is still collected.
func (l Lease) idleDeadline() time.Time {
	from := l.CreatedAt
	if l.StartedAt != nil {
		from = *l.StartedAt
	}
	if l.LastUsedAt != nil && l.LastUsedAt.After(from) {
		from = *l.LastUsedAt
	}
	return from.Add(time.Duration(l.IdleTimeoutSecs) * time.Second)
}

func (l Lease) hardDeadline() time.Time {
	return l.CreatedAt.Add(time.Duration(l.MaxLifetimeSecs) * time.Second)
}

// startCommand is what the lease's container runs: the checkout prologue, if any,
// then an idle wait.
//
// The wait is bounded by the lease's own max lifetime rather than being infinite,
// so a sandbox whose forge lost track of it — a crash between creating the pod and
// committing the row, say — still exits on its own. The reaper is the primary
// collector; this is the backstop for the case where there is no row left to reap
// from. `exec` replaces the shell so the sleep is PID 1 and a runtime stop signal
// reaches it directly.
func (l Lease) startCommand() []string {
	var b strings.Builder
	b.WriteString("set -e\n")
	if l.Checkout != nil {
		// intoWorkdirRoot is unconditionally true: a lease pins one working directory
		// (see leaseWorkDir) and the tree has to be at its root for later execs, which
		// each start fresh in that directory, to see it.
		b.WriteString(l.Checkout.script(l.SecretRefs, true))
	}
	// A ready marker distinguishes "the sandbox is up and the checkout succeeded"
	// from "the sandbox is up"; the runtime waits for the container to be running,
	// but a failed clone would otherwise look like a ready lease whose every exec
	// then fails confusingly.
	b.WriteString("echo " + leaseReadyMarker + "\n")
	b.WriteString(fmt.Sprintf("exec sleep %d\n", l.MaxLifetimeSecs))
	return []string{"/bin/sh", "-c", b.String()}
}

// leaseReadyMarker is printed by the start command once any checkout has completed.
// The runtime waits for it rather than for the container merely to be running.
const leaseReadyMarker = "__forge_lease_ready__"

// LeaseRuntime is implemented by runtimes that can hold a sandbox open across
// commands. The lease handlers type-assert to it, so a backend that cannot reports
// "leases not supported" rather than silently degrading to one-shot behaviour —
// which would be a correctness bug, not just a slow path: the caller's second
// command would run against a fresh sandbox with none of the first's work in it.
type LeaseRuntime interface {
	// StartLease creates the sandbox and blocks until it is ready to accept execs,
	// the context is cancelled, or it fails to come up.
	StartLease(ctx context.Context, lease Lease) error
	// Exec runs one command inside an already-started lease. It blocks until the
	// command completes or the execution's timeout elapses. The lease's sandbox
	// survives the command whatever its exit code.
	Exec(ctx context.Context, lease Lease, exec Execution) (RunResult, error)
	// StopLease tears the sandbox down. It is idempotent: stopping an already-gone
	// sandbox is success, so the reaper and an explicit release can race harmlessly.
	StopLease(ctx context.Context, lease Lease) error
}

// Lease configuration, read once at startup.
var (
	// maxLeasesPerUser bounds how many sandboxes one caller may hold at once. A lease
	// is memory reserved for as long as it is held, so this is the setting that stops
	// leases from becoming a way to reserve the cluster.
	maxLeasesPerUser int
	// leaseMaxLifetimeSecs / leaseIdleTimeoutSecs are the ceilings a request's own
	// values are clamped to.
	leaseMaxLifetimeSecs int64
	leaseIdleTimeoutSecs int64
	// leaseStartTimeoutSecs bounds boot: a lease that has not reported ready within
	// this is failed and its sandbox torn down, rather than occupying a slot forever.
	// It has to accommodate a microVM boot plus a full clone.
	leaseStartTimeoutSecs int64
)

func initLeaseConfig() {
	maxLeasesPerUser = envIntOrDefault("FORGE_MAX_LEASES_PER_USER", 4)
	leaseMaxLifetimeSecs = int64(envIntOrDefault("FORGE_LEASE_MAX_LIFETIME_SECS", 3600))
	leaseIdleTimeoutSecs = int64(envIntOrDefault("FORGE_LEASE_IDLE_TIMEOUT_SECS", 300))
	leaseStartTimeoutSecs = int64(envIntOrDefault("FORGE_LEASE_START_TIMEOUT_SECS", 300))
}

// createLeaseRequest is the POST /leases body.
type createLeaseRequest struct {
	Image       string            `json:"image"`
	RunnerClass string            `json:"runner_class"`
	Env         map[string]string `json:"env"`
	SecretRefs  map[string]string `json:"secret_refs"`
	Volumes     []VolumeMount     `json:"volumes"`
	Checkout    *CheckoutSpec     `json:"checkout"`
	Project     string            `json:"project"`
	IdleTimeout int64             `json:"idle_timeout"`
	MaxLifetime int64             `json:"max_lifetime"`
}

// validateLeaseRequest applies the bounds a lease request must satisfy. Image and
// volume checks are shared with submit and run at the handler, which has the
// request context needed to resolve them.
func validateLeaseRequest(req *createLeaseRequest) error {
	if strings.TrimSpace(req.Image) == "" {
		return fmt.Errorf("image is required")
	}
	if req.Checkout != nil {
		// A lease's checkout always lands at the working directory root (see
		// leaseWorkDir), so an explicit path would be silently ignored. Say so rather
		// than accepting it and cloning somewhere else.
		if req.Checkout.Path != "" {
			return fmt.Errorf("checkout.path is not supported on a lease: the repo is cloned into the working directory root so every exec starts inside it")
		}
		// The shared validator checks the spec against the command it will be woven
		// into. A lease's command is forge's own start script, which is always the
		// shell form, so a placeholder stands in for it here.
		if err := validateCheckout(req.Checkout, []string{"/bin/sh", "-c", ""}, req.SecretRefs, req.Env); err != nil {
			return err
		}
	}
	// At most one mount may claim the working directory, matching the rule submit
	// applies to executions — two would make the working directory ambiguous.
	workdirs := 0
	for _, m := range req.Volumes {
		if m.Workdir {
			workdirs++
		}
	}
	if workdirs > 1 {
		return fmt.Errorf("at most one volume mount may set workdir")
	}
	if req.MaxLifetime <= 0 || req.MaxLifetime > leaseMaxLifetimeSecs {
		req.MaxLifetime = leaseMaxLifetimeSecs
	}
	if req.IdleTimeout <= 0 || req.IdleTimeout > leaseIdleTimeoutSecs {
		req.IdleTimeout = leaseIdleTimeoutSecs
	}
	// An idle timeout above the hard lifetime can never fire, which reads as an idle
	// timeout that does not work. Clamp rather than reject: the caller asked for "do
	// not reap me for idleness before my lifetime is up", and that is what they get.
	if req.IdleTimeout > req.MaxLifetime {
		req.IdleTimeout = req.MaxLifetime
	}
	return nil
}
