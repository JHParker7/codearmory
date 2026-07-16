package main

import "time"

// The artifact store: named, user-owned blobs that OUTLIVE a run.
//
// Forge volumes are run-scoped and reaped when the run ends, so anything a run
// wants to keep — a Go build cache to reuse next time, a compiled CLI binary to
// publish — has nowhere to live. That is the gap this fills: a pipeline saves an
// artifact at the end of a run and restores it at the start of the next.
//
// Every artifact belongs to a user, and a user's total is capped. The cap is
// deliberately per-user rather than per-run: a cache is valuable precisely because
// it persists, so a run-scoped budget could not express "this user may keep 5 GB
// of cache". An admin sets the default for everyone and can override any single
// user — the same shape forge's concurrency limits use.

const (
	// maxNameLen bounds an artifact name. Names are user-supplied and become path
	// segments, so they are also validated for shape (see validateName).
	maxNameLen = 200
	// maxContentTypeLen bounds the stored content type.
	maxContentTypeLen = 128
)

// Artifact is one stored blob, unique per (user, name). Re-uploading the same name
// REPLACES it — a cache is a moving target, and versioning it would grow without
// bound against a fixed quota.
type Artifact struct {
	ArtifactID string `json:"artifact_id" gorm:"column:artifact_id;primaryKey"`
	// UserID owns the artifact and is the quota scope. OrgID is carried for
	// listing/visibility, never for the quota — a per-org pool would let one user
	// starve their colleagues.
	UserID string `json:"user_id" gorm:"column:user_id;index:idx_user_name,unique,priority:1"`
	OrgID  string `json:"org_id,omitempty" gorm:"column:org_id;default:''"`
	Name   string `json:"name" gorm:"column:name;index:idx_user_name,unique,priority:2"`
	// SizeBytes is the stored size, and the unit the quota is accounted in.
	SizeBytes   int64  `json:"size_bytes" gorm:"column:size_bytes"`
	ContentType string `json:"content_type,omitempty" gorm:"column:content_type;default:''"`
	// SHA256 lets a caller skip a download it already has, and detects truncation.
	SHA256    string    `json:"sha256,omitempty" gorm:"column:sha256;default:''"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

func (Artifact) TableName() string { return "artifacts" }

// Quota is an admin-set override of the default artifact allowance for one scope.
//
// Scope mirrors forge's concurrency limits so the two read the same way. Only
// ScopeUser is honoured today; the column exists so an org-wide pool can be added
// without a migration, rather than baking "user" into the primary key.
type Quota struct {
	Scope   string `json:"scope" gorm:"column:scope;primaryKey"`
	ScopeID string `json:"scope_id" gorm:"column:scope_id;primaryKey"`
	// MaxBytes is the total this scope may store. 0 means "blocked" rather than
	// "unlimited": an explicit 0 is a deliberate act, and reading it as unlimited
	// would turn a lockout into a free-for-all. Delete the row to fall back to the
	// default.
	MaxBytes  int64     `json:"max_bytes" gorm:"column:max_bytes"`
	SetBy     string    `json:"set_by,omitempty" gorm:"column:set_by;default:''"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

func (Quota) TableName() string { return "artifact_quotas" }

// ScopeUser is the only scope enforced today — see Quota.
const ScopeUser = "user"

// QuotaView is a scope's effective allowance and what it is using, which is what an
// admin actually needs to see: the raw override row says nothing about whether the
// user is anywhere near it.
type QuotaView struct {
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	// MaxBytes is the effective cap: the override when one exists, else the default.
	MaxBytes int64 `json:"max_bytes"`
	// Default reports whether MaxBytes came from the service default rather than an
	// override, so an admin can tell "never configured" from "deliberately set to
	// the same value".
	Default    bool  `json:"default"`
	UsedBytes  int64 `json:"used_bytes"`
	Artifacts  int64 `json:"artifacts"`
	SetBy      string `json:"set_by,omitempty"`
}
