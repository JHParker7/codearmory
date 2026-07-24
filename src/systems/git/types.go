package main

import "time"

// Supported git hosting backends. The credential broker mints (or brokers) clone
// credentials for each; "short-lived" applies where the backend supports it
// (GitHub App installation tokens, GitLab OAuth refresh, Forgejo admin-minted
// revoke-on-reuse tokens). PAT/basic modes broker a stored secret just-in-time.
const (
	backendGitHub  = "github"
	backendGitLab  = "gitlab"
	backendForgejo = "forgejo"
	backendGeneric = "generic"
)

// Per-type auth modes (authConfig.Mode).
const (
	// GitHub
	modeApp = "app" // GitHub App: app_id + installation_id + private_key → 1h installation token
	modePAT = "pat" // personal access token (brokered as-is)
	// GitLab
	modeToken = "token" // personal/group/project access token (brokered as-is)
	modeOAuth = "oauth" // OAuth refresh token → short-lived access token
	// Forgejo / Gitea
	modeAdmin = "admin" // admin token mints a per-user scoped, revoke-on-reuse token
	// Generic
	modeBasic = "basic" // username + password/token over HTTPS basic auth
)

// GitBackend is a user-linked git host. The credential material lives in AuthEnc,
// an AES-GCM ciphertext over a JSON authConfig; it is never returned by the API.
// At most one backend per (owner, host) so a clone URL resolves unambiguously, and
// names are unique per owner.
type GitBackend struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	Owner     string    `gorm:"uniqueIndex:ux_git_owner_host;uniqueIndex:ux_git_owner_name;index" json:"owner"`
	Name      string    `gorm:"uniqueIndex:ux_git_owner_name" json:"name"`
	Type      string    `json:"type"`
	BaseURL   string    `json:"base_url"`
	Host      string    `gorm:"uniqueIndex:ux_git_owner_host" json:"host"`
	AuthMode  string    `json:"auth_mode"`
	AuthEnc   []byte    `json:"-"`
	// PreferMirror opts this backend into git-factory's pull-through cache: on the
	// INTERNAL clone-token path (Forge/Workflows), the broker asks git-factory to keep a
	// warm mirror of the repo and returns git-factory's clone URL instead of upstream's,
	// so runners clone from the in-cluster cache. Off by default — a backend is only
	// mirrored when the user asks — and never affects the user-facing /credentials path.
	PreferMirror bool      `gorm:"default:false" json:"prefer_mirror"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// authConfig is the decrypted credential material for a backend. Only the fields
// relevant to (Type, Mode) are populated. Marshalled to JSON, encrypted, and
// stored in GitBackend.AuthEnc.
type authConfig struct {
	Mode string `json:"mode"`

	// GitHub App
	AppID          int64  `json:"app_id,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
	PrivateKey     string `json:"private_key,omitempty"`

	// Token-based (GitHub PAT, GitLab token, Forgejo token)
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`

	// Generic basic-auth
	Password string `json:"password,omitempty"`

	// GitLab OAuth refresh
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`

	// Forgejo admin minting
	AdminToken string `json:"admin_token,omitempty"`
}

// GitRepo is a manually-registered repository for an owner. Most repos are
// discovered live by enumerating a linked backend's API, but backends that can't
// be enumerated (generic basic-auth) — or repos a user wants pinned regardless —
// are stored here so they still appear in the repo selector. URL is the HTTPS
// clone URL; Host is derived from it so it lines up with the owning backend.
type GitRepo struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	Owner     string    `gorm:"uniqueIndex:ux_gitrepo_owner_url;index" json:"owner"`
	Name      string    `json:"name"`
	URL       string    `gorm:"uniqueIndex:ux_gitrepo_owner_url" json:"url"`
	Host      string    `json:"host"`
	CreatedAt time.Time `json:"created_at"`
	// WorkflowSync opts this repo into GitOps: the workflows service syncs pipeline
	// configs from `.armory/workflows/*` on a push. Off by default. Branches is the
	// allowlist — configs are synced ONLY from these branches, so a push to a feature
	// branch or an untrusted PR can never register or run a workflow. Empty branches
	// with sync enabled means "main only" (see effectiveSyncBranches).
	WorkflowSyncEnabled  bool     `gorm:"default:false" json:"workflow_sync_enabled"`
	WorkflowSyncBranches []string `gorm:"serializer:json" json:"workflow_sync_branches"`
}

// Repo source labels: a repo is either discovered by enumerating a backend's API
// ("enumerated") or pinned by the user in the GitRepo table ("manual").
const (
	repoSourceEnumerated = "enumerated"
	repoSourceManual     = "manual"
)

// repoView is one entry in the repo selector. It carries the clone URL the forge
// `git:` secret_ref consumes plus enough context (backend, type, source) to label
// it. ID is set only for manual repos, so the UI can offer a delete.
type repoView struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Backend     string `json:"backend,omitempty"`
	BackendType string `json:"backend_type,omitempty"`
	Source      string `json:"source"`
	// GitOps sync settings — only meaningful for a pinned (manual) repo.
	WorkflowSyncEnabled  bool     `json:"workflow_sync_enabled,omitempty"`
	WorkflowSyncBranches []string `json:"workflow_sync_branches,omitempty"`
}

// createRepoRequest is the body of POST /repos. Name is optional; when blank it is
// derived from the URL path (e.g. "owner/repo").
type createRepoRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// backendView is the safe, secret-free projection returned by the API.
type backendView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	BaseURL      string    `json:"base_url"`
	Host         string    `json:"host"`
	AuthMode     string    `json:"auth_mode"`
	PreferMirror bool      `json:"prefer_mirror"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (b GitBackend) view() backendView {
	return backendView{
		ID:           b.ID,
		Name:         b.Name,
		Type:         b.Type,
		BaseURL:      b.BaseURL,
		Host:         b.Host,
		AuthMode:     b.AuthMode,
		PreferMirror: b.PreferMirror,
		CreatedAt:    b.CreatedAt,
		UpdatedAt:    b.UpdatedAt,
	}
}

// createBackendRequest is the body of POST /backends.
type createBackendRequest struct {
	Name    string     `json:"name"`
	Type    string     `json:"type"`
	BaseURL string     `json:"base_url"`
	Auth    authConfig `json:"auth"`
	// PreferMirror opts the backend into git-factory's pull-through cache (default off).
	PreferMirror bool `json:"prefer_mirror"`
}

// updateBackendRequest is the body of PUT /backends/{id}. Only base_url, auth, and the
// mirror preference may change; the host derived from base_url must not collide with
// another backend. PreferMirror is a pointer so "omitted" is distinct from "set false".
type updateBackendRequest struct {
	BaseURL      string      `json:"base_url"`
	Auth         *authConfig `json:"auth"`
	PreferMirror *bool       `json:"prefer_mirror"`
}

// credential is the broker's response: an authenticated clone URL plus the raw
// username/secret so callers that build their own git config can use either. The
// secret is never logged or persisted.
type credential struct {
	Type        string    `json:"type"` // always "basic" today (HTTPS basic auth)
	Username    string    `json:"username"`
	Secret      string    `json:"secret"`
	CloneURL    string    `json:"clone_url"`
	Backend     string    `json:"backend"`
	BackendType string    `json:"backend_type"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

// mintRequest is the user-facing POST /credentials body.
type mintRequest struct {
	RepoURL string `json:"repo_url"`
}

// internalMintRequest is the forge/workflows-facing POST /internal/clone-token body.
type internalMintRequest struct {
	UserID  string `json:"user_id"`
	RepoURL string `json:"repo_url"`
}

const maxBodyBytes = 1 << 20 // 1 MiB
