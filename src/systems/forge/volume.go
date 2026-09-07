package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A shared workspace volume lets the steps of one workflow run share a filesystem:
// a "create-volume" step provisions it, a git step clones a repo into it, and later
// steps attach the same volume and see the checkout plus any build artifacts. The
// storage itself is ephemeral — a RAM/tmpfs-backed PVC in kubernetes, a tmpfs named
// volume in docker — so only this metadata row is durable. Forge keeps the row to
// (a) enforce the admin's per-workflow total-size cap, (b) validate that an
// execution may attach a volume it references, and (c) tear volumes down when the
// run (or an age reaper) says they are done.

const (
	// defaultVolumeMountPath is where an attached volume lands when the mount does not
	// name a path, and the default working dir a create-volume request records.
	defaultVolumeMountPath = "/workspace"
	mediumMemory           = "memory" // RAM/tmpfs-backed (default): fast, ephemeral, no disk
	mediumDisk             = "disk"   // node-disk-backed: survives node memory pressure
	volumeStatusActive     = "active"
	volumeStatusDeleted    = "deleted"

	// Volume readiness states reported by VolumeStatus and polled by a create-volume
	// workflow step (via GET /volumes/{id}). "ready" is terminal-success, "failed" is
	// terminal-failure, and "provisioning" keeps the poller waiting — so a downstream
	// step that mounts the volume pays no dynamic-provisioning latency inside its own
	// command timeout.
	volumeReadyReady        = "ready"
	volumeReadyProvisioning = "provisioning"
	volumeReadyFailed       = "failed"

	// minVolumeMB floors a request so a zero/omitted size does not create a 0-byte
	// volume; maxVolumeNameLen bounds the logical handle.
	minVolumeMB      = 16
	maxVolumeNameLen = 40
)

var (
	// volumeNameRe restricts the logical volume handle (and, after sanitising, the
	// derived backend resource name) to a DNS-1123 label so it is valid as both a
	// kubernetes object name and a docker volume name.
	volumeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// workflowIDRe keeps the workflow scope key to safe characters (it is hashed into
	// the resource name and used verbatim in WHERE clauses via parameters).
	workflowIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// maxWorkflowVolumeMB is the admin-configured ceiling on the summed size of all
// active volumes for a single workflow, read once at startup from
// FORGE_MAX_WORKFLOW_VOLUME_MB (default 10 GiB). A create request that would push a
// workflow's total over this is rejected. maxVolumeMB bounds any single volume
// (FORGE_MAX_VOLUME_MB, default = the per-workflow cap). Set the per-workflow cap to
// 0 to disable shared volumes entirely (every create is refused).
var (
	maxWorkflowVolumeMB int64
	maxVolumeMB         int64
	// volumeMaxAge is how long a volume may live before the reaper removes it as
	// orphaned — a safety net for runs whose workflow crashed before teardown. It
	// defaults comfortably above the longest possible run (maxTimeout) so a live run's
	// volume is never reaped out from under it.
	volumeMaxAge time.Duration
	// volumeStorageClass / volumeAccessMode configure the kubernetes PVC the volume is
	// backed by; empty storage class means the cluster default. A RAM/tmpfs-backed
	// StorageClass here is what makes the volume "in memory".
	volumeStorageClass string
	volumeAccessMode   string
)

func initVolumeConfig() {
	maxWorkflowVolumeMB = int64(envIntOrDefault("FORGE_MAX_WORKFLOW_VOLUME_MB", 10*1024))
	maxVolumeMB = int64(envIntOrDefault("FORGE_MAX_VOLUME_MB", int(maxWorkflowVolumeMB)))
	volumeMaxAge = time.Duration(envIntOrDefault("FORGE_VOLUME_MAX_AGE_SECS", int(maxTimeout)+3600)) * time.Second
	volumeStorageClass = envOrDefault("FORGE_VOLUME_STORAGE_CLASS", "")
	volumeAccessMode = envOrDefault("FORGE_VOLUME_ACCESS_MODE", "ReadWriteOnce")
}

// Volume is the durable accounting record for one shared workspace volume. The
// concrete storage is named ResourceName (a PVC / docker volume); the (WorkflowID,
// Name) pair is the stable handle every step references.
type Volume struct {
	// ResourceName is the concrete backend object name (PVC / docker volume),
	// derived deterministically from (WorkflowID, Name) so every step can compute it
	// without a lookup. Primary key.
	ResourceName string `gorm:"column:resource_name;primaryKey" json:"resource_name"`
	// WorkflowID scopes the volume to one workflow run — the key for the per-workflow
	// size cap and for bulk teardown. Name is the logical handle within that scope.
	WorkflowID string `gorm:"column:workflow_id;not null;index"  json:"workflow_id"`
	Name       string `gorm:"column:name;not null"               json:"name"`
	UserID     string `gorm:"column:user_id;not null"            json:"user_id"`
	OrgID      string `gorm:"column:org_id;not null;default:''"  json:"org_id,omitempty"`
	// Project/ProjectID/ProjectNamespace mirror Lease's: a slug that resolves to a
	// real gatekeeper project widens access to its members, otherwise it stays a
	// free-text label and access stays governed by UserID.
	Project          string `gorm:"column:project;not null;default:''"           json:"project,omitempty"`
	ProjectID        string `gorm:"column:project_id;not null;default:''"        json:"project_id,omitempty"`
	ProjectNamespace string `gorm:"column:project_namespace;not null;default:''" json:"project_namespace,omitempty"`
	Backend          string `gorm:"column:backend;not null;default:default" json:"backend"`
	SizeMB           int64  `gorm:"column:size_mb;not null"            json:"size_mb"`
	Medium           string `gorm:"column:medium;not null;default:memory"   json:"medium"`
	MountPath        string `gorm:"column:mount_path;not null;default:/workspace" json:"mount_path"`
	// Status is active until the volume is torn down; a deleted row is kept briefly
	// only so a double-delete is a no-op. Only active volumes count against the cap.
	Status    string    `gorm:"column:status;not null;default:active"   json:"status"`
	CreatedAt time.Time `gorm:"column:created_at;not null;default:now()" json:"created_at"`
}

func (Volume) TableName() string { return "volumes" }

// VolumeMount attaches a shared volume to an execution's sandbox. It names the same
// (WorkflowID, Name) handle used to create the volume; forge resolves that to the
// concrete backend resource and mounts it at MountPath. Submit-time validation
// checks the volume exists and belongs to the caller, so a job cannot mount an
// arbitrary volume.
type VolumeMount struct {
	WorkflowID string `json:"workflow_id"`
	Name       string `json:"name"`
	// MountPath overrides where the volume mounts (default: the volume's own recorded
	// mount path, else /workspace).
	MountPath string `json:"mount_path,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
	// Workdir sets the container working directory to MountPath so the command runs
	// inside the shared volume (e.g. the checked-out repo). At most one mount per
	// execution may set it.
	Workdir bool `json:"workdir,omitempty"`
}

// createVolumeRequest is the POST /volumes body.
type createVolumeRequest struct {
	WorkflowID  string `json:"workflow_id"`
	Name        string `json:"name"`
	SizeMB      int64  `json:"size_mb"`
	Medium      string `json:"medium"`     // memory (default) | disk
	MountPath   string `json:"mount_path"` // default /workspace
	RunnerClass string `json:"runner_class"`
	Backend     string `json:"backend"`
	// Project is an optional gatekeeper project slug; when it resolves, the volume is
	// filed into that project and ProjectID/ProjectNamespace are stamped from it.
	Project string `json:"project,omitempty"`
}

// VolumeSpec is what a runtime needs to materialise a volume; the runtime is
// oblivious to workflow scoping and the size cap, which forge enforces before it
// ever calls the runtime.
type VolumeSpec struct {
	ResourceName string
	SizeMB       int64
	Medium       string
}

// VolumeRuntime is implemented by runtimes that can back a shared workspace volume
// (docker, kubernetes). The volume handlers type-assert to it, so a backend that
// cannot cleanly reports "volumes not supported" instead of mis-running.
type VolumeRuntime interface {
	CreateVolume(ctx context.Context, spec VolumeSpec) error
	DeleteVolume(ctx context.Context, resourceName string) error
	// VolumeStatus reports whether a volume's backing storage is usable yet, as one of
	// volumeReadyReady / volumeReadyProvisioning / volumeReadyFailed plus an optional
	// human-readable detail. A create-volume step polls it so slow dynamic provisioning
	// (e.g. Ceph RBD at ~35s) completes before a downstream step mounts the volume,
	// rather than being charged against that step's command timeout.
	VolumeStatus(ctx context.Context, resourceName string) (state string, detail string, err error)
}

// volumeStatusResponse is the GET /volumes/{id} body the create-volume async poll
// reads: Status is the readiness state (success_states: ["ready"], failure_states:
// ["failed"]); Detail carries a provisioning-failure reason for the failure path.
type volumeStatusResponse struct {
	ResourceName string `json:"resource_name"`
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
}

// volumeResourceName derives the concrete backend object name from the (workflowID,
// name) handle. It is deterministic so create and every attach agree without a
// lookup, DNS-1123-valid (≤63 chars) for kubernetes, and collision-safe across
// workflows via a hash of the workflow id.
func volumeResourceName(workflowID, name string) string {
	sum := sha256.Sum256([]byte(workflowID))
	return "fv-" + hex.EncodeToString(sum[:6]) + "-" + name
}

// resolvedMount is a VolumeMount resolved to the concrete backend resource name a
// runtime mounts. The resource name is derived deterministically, so the runtime
// needs no database read to attach a volume.
type resolvedMount struct {
	resourceName string
	mountPath    string
	readOnly     bool
	workdir      bool
}

// resolveVolumeMounts turns an execution's requested VolumeMounts into concrete
// mounts for the runtime, applying the default mount path. Both the docker and
// kubernetes runtimes share it so the two cannot derive resource names differently.
func resolveVolumeMounts(mounts []VolumeMount) []resolvedMount {
	out := make([]resolvedMount, 0, len(mounts))
	for _, m := range mounts {
		path := m.MountPath
		if path == "" {
			path = defaultVolumeMountPath
		}
		out = append(out, resolvedMount{
			resourceName: volumeResourceName(m.WorkflowID, m.Name),
			mountPath:    path,
			readOnly:     m.ReadOnly,
			workdir:      m.Workdir,
		})
	}
	return out
}

// normalizeCreateVolume applies defaults and validates a create request, returning
// the resource name to use. Size is floored to minVolumeMB and capped per-volume;
// the per-workflow total is enforced separately (it needs a DB read).
func normalizeCreateVolume(req *createVolumeRequest) (resourceName string, err error) {
	if !workflowIDRe.MatchString(req.WorkflowID) {
		return "", fmt.Errorf("workflow_id must match %s", workflowIDRe.String())
	}
	if req.Name == "" {
		req.Name = "workspace"
	}
	if len(req.Name) > maxVolumeNameLen || !volumeNameRe.MatchString(req.Name) {
		return "", fmt.Errorf("name %q: must be a DNS-1123 label of at most %d chars", req.Name, maxVolumeNameLen)
	}
	switch req.Medium {
	case "":
		req.Medium = mediumMemory
	case mediumMemory, mediumDisk:
	default:
		return "", fmt.Errorf("medium %q: must be %q or %q", req.Medium, mediumMemory, mediumDisk)
	}
	if req.MountPath == "" {
		req.MountPath = defaultVolumeMountPath
	}
	if err := validateMountPath(req.MountPath); err != nil {
		return "", err
	}
	if req.SizeMB < minVolumeMB {
		req.SizeMB = minVolumeMB
	}
	if maxWorkflowVolumeMB <= 0 {
		return "", fmt.Errorf("shared volumes are disabled (FORGE_MAX_WORKFLOW_VOLUME_MB=0)")
	}
	if req.SizeMB > maxVolumeMB {
		return "", fmt.Errorf("size_mb %d exceeds the per-volume limit of %d", req.SizeMB, maxVolumeMB)
	}
	return volumeResourceName(req.WorkflowID, req.Name), nil
}

// validateMountPath requires an absolute, clean-ish path with no "..", so a mount
// cannot be redirected to a parent/relative location. It is interpolated into
// container specs, not a shell, so the checks are about path shape, not quoting.
func validateMountPath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, "\x00") {
		return fmt.Errorf("mount_path %q: must be an absolute path with no %q", p, "..")
	}
	// Disallow mounting over paths the sandbox relies on.
	if slices.Contains([]string{"/", "/tmp", "/etc", "/proc", "/sys", "/dev"}, p) {
		return fmt.Errorf("mount_path %q is reserved", p)
	}
	return nil
}

// validateVolumeMounts checks the shape of an execution's requested mounts (names,
// paths, at most one workdir). Existence/ownership of each referenced volume is
// checked against the database by the caller (submit), which has the ctx and user.
func validateVolumeMounts(mounts []VolumeMount) error {
	workdirs := 0
	seen := map[string]bool{}
	for i := range mounts {
		m := &mounts[i]
		if !workflowIDRe.MatchString(m.WorkflowID) {
			return fmt.Errorf("volume[%d].workflow_id must match %s", i, workflowIDRe.String())
		}
		if len(m.Name) > maxVolumeNameLen || !volumeNameRe.MatchString(m.Name) {
			return fmt.Errorf("volume[%d].name %q: must be a DNS-1123 label", i, m.Name)
		}
		if m.MountPath == "" {
			m.MountPath = defaultVolumeMountPath
		}
		if err := validateMountPath(m.MountPath); err != nil {
			return fmt.Errorf("volume[%d]: %w", i, err)
		}
		if seen[m.MountPath] {
			return fmt.Errorf("volume[%d]: duplicate mount_path %q", i, m.MountPath)
		}
		seen[m.MountPath] = true
		if m.Workdir {
			workdirs++
		}
	}
	if workdirs > 1 {
		return fmt.Errorf("at most one attached volume may set workdir")
	}
	return nil
}
