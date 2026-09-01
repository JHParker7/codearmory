package main

import (
	"time"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// Repo is a hosted git repository. Two independent identities matter here and must
// not be conflated:
//
//   - Owner is the gatekeeper user_id — the authorization filter. Every read/write
//     query scopes by it so users only ever see their own rows. It is never
//     serialized and never appears in a URL.
//   - Namespace is the human-readable handle (an org name, or the caller's
//     username) that forms the clone-URL path segment /{namespace}/{name}.git.
//
// Uniqueness follows the clone URL, not ownership: (Namespace, Name) is unique, so
// two members of the same org cannot create repos that would resolve to the same
// URL. The stable ID — not the namespace or name — keys the bytes on disk, which
// is what makes a rename metadata-only (ARCHITECTURE §4).
type Repo struct {
	ID            string `gorm:"primaryKey" json:"id"`                           // uuid, stable, used in RBAC resource
	Owner         string `gorm:"index" json:"-"`                                 // gatekeeper user_id — ownership filter (indexed for owner-scoped list/get)
	Namespace     string `gorm:"uniqueIndex:ux_namespace_name" json:"namespace"` // org name or username — the clone-URL path segment
	Name          string `gorm:"uniqueIndex:ux_namespace_name" json:"name"`
	Description   string `json:"description"`
	DefaultBranch string `gorm:"default:main" json:"default_branch"`
	Shard         string `gorm:"serializer:json" json:"-"` // v2+: which git-node holds the bytes
	// Visibility is the ONE per-record fact that can authorize a caller who holds no
	// grant at all: "public" makes reads — a clone above all — available to anyone,
	// while every write still goes through gatekeeper. Default private, so a repo is
	// never published by omission (an unset column on an old row reads as private).
	Visibility string `gorm:"default:private" json:"visibility"`
	// Project files the repo into a gatekeeper Project so a project role grants access
	// to every repo in it at once (see the monorepo docs/projects/design.md). Project
	// is the slug for display/filtering; ProjectID (stable) widens repo lists cheaply;
	// ProjectNamespace is the owning namespace a member's project grant is qualified
	// with (always == this repo's Namespace). All empty ⇒ the repo is unfiled and
	// authorized by ownership/per-repo share/visibility exactly as before.
	Project          string `gorm:"default:''" json:"project,omitempty"`
	ProjectID        string `gorm:"default:''" json:"project_id,omitempty"`
	ProjectNamespace string `gorm:"default:''" json:"project_namespace,omitempty"`
	// SizeBytes is what the repo occupied at the end of the last push or gc — an
	// approximation from git's own object accounting, refreshed on the events that
	// change it rather than walked on read. 0 means "never measured" (a repo created
	// before the column existed, or one never pushed to), not "empty".
	SizeBytes int64 `gorm:"default:0" json:"size_bytes"`
	// Kind is "native" (born here, push-authoritative — the default and today's only
	// behaviour) or "mirror" (a cached copy of UpstreamURL, refreshed by fetching it).
	// A mirror is READ-ONLY over the wire: receive-pack is refused, because its source
	// of truth is upstream, not a client push. An unset column on an old row reads as
	// native, so existing repos are unaffected.
	Kind string `gorm:"default:native" json:"kind"`
	// UpstreamURL is the source a mirror fetches from, stored WITHOUT credentials
	// (userinfo stripped — see sanitizeUpstreamURL). The authenticated URL used to
	// fetch is supplied per-request by the caller (git-connector) and never persisted;
	// this field is only for display and for choosing the remote to re-fetch. Empty for
	// native repos.
	UpstreamURL string `json:"upstream_url,omitempty"`
	// MirrorAt is the last time a fetch from UpstreamURL succeeded. nil for a native
	// repo, or a mirror row created but not yet fetched.
	MirrorAt *time.Time `json:"mirror_at,omitempty"`
	// Version increments on every accepted write (push) and every mirror fetch that
	// moved a ref. A replica may serve a READ only once its applied version has caught
	// up to this (ReplicaState.Applied >= Version), which is what stops a clone right
	// after a push from being answered by a lagging replica (ARCHITECTURE §5 Step 4).
	Version   int64     `gorm:"default:0" json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Derived on every read from GIT_HTTP_BASE_URL (see Repo.AfterFind), never a
	// column — ARCHITECTURE §4. Stored, it would freeze the base URL at creation
	// time: re-pointing the deployment at its real external host is a config change,
	// and every repo created before it would go on advertising the old URL until a
	// migration rewrote the table. A rename would strand it the same way.
	HttpUrl string `gorm:"-" json:"http_url"`
}

// Visibility values. Private is the zero-value meaning as well as the column
// default, so anything unrecognised is treated as private rather than public.
const (
	visibilityPrivate = "private"
	visibilityPublic  = "public"
)

// Repo kinds. Native is the zero-value meaning as well as the column default, so an
// old row with an unset column reads as native and behaves exactly as before.
const (
	kindNative = "native"
	kindMirror = "mirror"
)

// mirrorRequest is the body of POST /internal/mirrors — the pull-through-cache entry
// point called by git-connector (never a browser). UpstreamURL carries credentials
// (git-connector mints them just-in-time); they are used to fetch and then discarded,
// only the sanitized URL is stored. Owner is the CI service identity the mirror is
// scoped to — mirrors are clonable only by that identity (design decision: restrict to
// the CI identity, not upstream's ACL).
type mirrorRequest struct {
	UpstreamURL   string `json:"upstream_url"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	DefaultBranch string `json:"default_branch"`
	// Ref, when set, must exist after the fetch or the call fails — this is what makes
	// a mirror CORRECT for CI: refresh-before-clone at a known commit/branch guarantees
	// the clone that follows sees it, rather than a fast-but-stale copy.
	Ref string `json:"ref"`
}

// cloneTokenRequest is the body of POST /internal/clone-token — git-connector asking
// for a runner-usable, read-only credential for one mirror. The repo is named either by
// id or by (namespace, name). TTLSeconds is clamped to a sane range.
type cloneTokenRequest struct {
	RepoID     string `json:"repo_id"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	TTLSeconds int    `json:"ttl_seconds"`
}

// cloneTokenResponse is what a runner needs to clone: the fully-authenticated URL and
// when it stops working. The bare token is not returned separately — the URL is the
// thing Forge injects into an env var.
type cloneTokenResponse struct {
	CloneURL  string    `json:"clone_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type createRepoRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	AssignToOrg bool   `json:"org_repo"`
	// Empty means private — a repo is public only if the caller asks for it.
	Visibility string `json:"visibility"`
	// Project files the new repo into a gatekeeper project (a slug the caller can write
	// to). Empty leaves it unfiled; a slug that names no accessible project is kept as a
	// plain label.
	Project string `json:"project,omitempty"`
}

type updateRepoRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Empty leaves the current visibility alone, so a rename can't publish a repo
	// by accident.
	Visibility string `json:"visibility"`
	// Project files (or re-files) the repo into a gatekeeper project. Empty leaves the
	// current filing untouched (a metadata edit can't unfile a repo by omission).
	Project string `json:"project,omitempty"`
	// File, when set, makes this a file-content edit instead of a metadata update — it
	// rides on this registered route so the browser editor works through the gateway.
	File *fileEditRequest `json:"file,omitempty"`
}

type fileEditRequest struct {
	Ref     string `json:"ref"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Message string `json:"message"`
}
