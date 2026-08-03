package main

import (
	"context"
	"errors"
	"time"
)

// Org represents a tenant organisation. All roles and users belong to an org.
type Org struct {
	OrgID     string    `json:"org_id"     gorm:"column:org_id;primaryKey"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
	OrgName   string    `json:"org_name"   gorm:"column:org_name;uniqueIndex"`
	OwnerID   string    `json:"owner_id"   gorm:"column:owner_id"`
	Active    bool      `json:"active"     gorm:"column:active;default:true"`
}

// Role defines a named permission set scoped to an Org.
// Name is empty for user-facing roles and "workflow:<id>" for workflow service roles.
type Role struct {
	RoleID         string    `json:"role_id"          gorm:"column:role_id;primaryKey"`
	CreatedAt      time.Time `json:"created_at"       gorm:"column:created_at"`
	UpdatedAt      time.Time `json:"updated_at"       gorm:"column:updated_at"`
	Name           string    `json:"name,omitempty"   gorm:"column:name;default:''"`
	PermissionsIDs []string  `json:"permissions_ids"  gorm:"column:permissions_ids;serializer:json"`
	OrgID          *string   `json:"org_id"           gorm:"column:org_id"`
	OwnerID        string    `json:"owner_id"         gorm:"column:owner_id"`
	Active         bool      `json:"active"           gorm:"column:active;default:true"`
}

// Team groups users within an org and assigns them a shared Role.
type Team struct {
	TeamID    string    `json:"team_id"    gorm:"column:team_id;primaryKey"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
	TeamName  string    `json:"team_name"  gorm:"column:team_name"`
	RoleID    *string   `json:"role_id"    gorm:"column:role_id"`
	OwnerID   string    `json:"owner_id"   gorm:"column:owner_id"`
	OrgID     *string   `json:"org_id"     gorm:"column:org_id"`
	Active    bool      `json:"active"     gorm:"column:active;default:true"`
}

// User represents an authenticated account belonging to an Org, Team, and Role.
// OrgID, TeamID, and RoleID are NULL when not explicitly assigned.
//
// DefaultRoleID points at the system-managed "default" role that holds the
// user-scoped default grants. It is kept separate from RoleID (which holds
// org/team-ownership and custom grants) so the default role can be rebuilt
// wholesale on login without disturbing the user's other permissions.
// DefaultGrantsVersion records the grants hash the default role was last built
// from; when it diverges from grantsVersion() the role is rebuilt.
type User struct {
	UserID               string    `gorm:"column:user_id;primaryKey"`
	HashedPassword       string    `gorm:"column:hashed_password"`
	CreatedAt            time.Time `gorm:"column:created_at"`
	UpdatedAt            time.Time `gorm:"column:updated_at"`
	Firstname            string    `gorm:"column:firstname"`
	Lastname             string    `gorm:"column:lastname"`
	Email                string    `gorm:"column:email;uniqueIndex"`
	OrgID                *string   `gorm:"column:org_id"`
	RoleID               *string   `gorm:"column:role_id"`
	TeamID               *string   `gorm:"column:team_id"`
	DefaultRoleID        *string   `gorm:"column:default_role_id"`
	DefaultGrantsVersion string    `gorm:"column:default_grants_version"`
	Username             string    `gorm:"column:username;uniqueIndex"`
	Active               bool      `gorm:"column:active;default:true"`
}

// UserOrgMembership records that a user belongs to an org. A user may hold many
// memberships at once (they can be in multiple orgs); User.OrgID names the single
// org they are currently *acting in* — the "active org" — which must always match
// one of their active membership rows. Membership is what invite-accept grants and
// what the org switch/leave endpoints add and remove; the active org is a pointer
// into the membership set that drives every per-request org scoping decision (list
// results, secrets, permission stamping, the service gate, and the org_id returned
// by check_permissions that downstream services read). Keeping the active org on
// User.OrgID means no downstream service needs to change: they still see one org
// per request.
type UserOrgMembership struct {
	MembershipID string    `json:"membership_id" gorm:"column:membership_id;primaryKey"`
	UserID       string    `json:"user_id"       gorm:"column:user_id"`
	OrgID        string    `json:"org_id"        gorm:"column:org_id"`
	CreatedAt    time.Time `json:"created_at"    gorm:"column:created_at"`
	UpdatedAt    time.Time `json:"updated_at"    gorm:"column:updated_at"`
	Active       bool      `json:"-"             gorm:"column:active;default:true"`
}

func (UserOrgMembership) TableName() string { return "user_org_memberships" }

// Session holds the per-session ECDSA public key used to verify the JWT signature.
// The JWT itself is never stored — authMiddleware re-validates the signature on
// each request using the stored PubKey, so retaining the token would be redundant
// and would expose all active sessions on a database breach.
// ScopedRoleID, when set, restricts permission checks to only the permissions in
// that role — regardless of the user's own role or team membership. Used by
// run-scoped tokens to enforce the workflow's minimal permission set.
// ClientID is set (and UserID is empty) for sessions issued via the OAuth2
// client_credentials grant. Permission checks use the client's assigned role.
type Session struct {
	SessionID    string    `gorm:"column:session_id;primaryKey"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
	UserID       string    `gorm:"column:user_id"`
	ExpiresAt    time.Time `gorm:"column:expires_at"`
	PubKey       string    `gorm:"column:pub_key"`
	Active       bool      `gorm:"column:active;default:true"`
	ScopedRoleID *string   `gorm:"column:scoped_role_id"`
	ClientID     *string   `gorm:"column:client_id"`
}

// Invite represents a pending or resolved invitation for a user to join an Org or Team.
// ResourceType is "org" or "team"; Status is "pending", "accepted", or "declined".
type Invite struct {
	InviteID     string    `json:"invite_id"     gorm:"column:invite_id;primaryKey"`
	CreatedAt    time.Time `json:"created_at"    gorm:"column:created_at"`
	UpdatedAt    time.Time `json:"updated_at"    gorm:"column:updated_at"`
	InviterID    string    `json:"inviter_id"    gorm:"column:inviter_id"`
	InviteeEmail string    `json:"invitee_email" gorm:"column:invitee_email"`
	ResourceType string    `json:"resource_type" gorm:"column:resource_type"`
	ResourceID   string    `json:"resource_id"   gorm:"column:resource_id"`
	Status       string    `json:"status"        gorm:"column:status;default:pending"`
	ExpiresAt    time.Time `json:"expires_at"    gorm:"column:expires_at"`
	Active       bool      `json:"-"             gorm:"column:active;default:true"`
}

// SignupAllowlistEntry is a single permitted-email rule for invite-only signup.
// Email holds either a full address ("alice@example.com") or a domain rule
// ("@example.com", matching any address at that domain), always normalised to
// lower case. Entries are managed by platform admins at runtime; the signup gate
// consults them only when the SignupPolicy has InviteOnly set.
type SignupAllowlistEntry struct {
	EntryID   string    `json:"entry_id"   gorm:"column:entry_id;primaryKey"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
	Email     string    `json:"email"      gorm:"column:email"`
	Note      string    `json:"note"       gorm:"column:note;default:''"`
	CreatedBy string    `json:"created_by" gorm:"column:created_by"`
	Active    bool      `json:"-"          gorm:"column:active;default:true"`
}

func (SignupAllowlistEntry) TableName() string { return "signup_allowlist" }

// signupPolicySingletonID is the fixed primary key of the single SignupPolicy row.
const signupPolicySingletonID = "singleton"

// SignupPolicy is a single-row table holding instance-wide registration policy.
// When InviteOnly is true, handleSignup rejects any email that is not matched by
// the signup allowlist (the bootstrap admin's very first signup is exempt so the
// instance can always be initialised). Seeded from GATEKEEPER_INVITE_ONLY at
// startup, then changed by admins at runtime via PUT /signup-policy.
//
// InviteOnly deliberately carries no `default:` gorm tag: a default tag makes
// GORM omit the field on Create when it holds its zero value (false), so the
// column could not be explicitly stored false. It is always set explicitly.
type SignupPolicy struct {
	ID         string    `json:"-"          gorm:"column:id;primaryKey"`
	InviteOnly bool      `json:"invite_only" gorm:"column:invite_only"`
	UpdatedAt  time.Time `json:"updated_at" gorm:"column:updated_at"`
	UpdatedBy  string    `json:"updated_by" gorm:"column:updated_by;default:''"`
}

func (SignupPolicy) TableName() string { return "signup_policy" }

// Permissions defines a set of allowed actions on resources for a given service.
type Permissions struct {
	Name          string    `json:"name"           gorm:"column:name"`
	CreatedAt     time.Time `json:"created_at"     gorm:"column:created_at"`
	UpdatedAt     time.Time `json:"updated_at"     gorm:"column:updated_at"`
	PermissionsID string    `json:"permissions_id" gorm:"column:permissions_id;primaryKey"`
	Service       string    `json:"service"        gorm:"column:service"`
	Actions       []string  `json:"actions"        gorm:"column:actions;serializer:json"`
	Resources     []string  `json:"resources"      gorm:"column:resources;serializer:json"`
	OwnerID       string    `json:"owner_id"       gorm:"column:owner_id"`
	OrgID         *string   `json:"org_id"         gorm:"column:org_id"`
	Active        bool      `json:"active"         gorm:"column:active;default:true"`
}

// ServiceAccount represents a non-user service identity with a hashed key and
// an associated role. Permissions are added to the role via ServicePermissionRequest.
//
// Key fields:
//   - HashedKey: bcrypt hash of the current key. Updated on each rotation.
//   - HashedBootstrapKey: bcrypt hash of the bootstrap key from GATEKEEPER_SERVICES.
//     Never changes after the account is seeded. Used as a fallback in
//     requireServiceAuth so that a service pod can re-authenticate after a restart
//     even if its rotated key was only stored in memory.
//
// Network binding fields (both optional; nil / empty slice means "no restriction"):
//
//   - AllowedCIDRs: if non-empty, the source IP of every authenticated request
//     must fall within at least one of these CIDR ranges.
//   - ClientCertFingerprints: if non-empty, mutual TLS must be enabled on the
//     gatekeeper server and the presented client certificate's SHA-256 fingerprint
//     (lower-case hex of DER bytes) must match one of these values.
//
// MAC-address binding is not supported at the HTTP layer: MAC addresses do not
// cross IP routers and are not visible to the server in normal deployments.
type ServiceAccount struct {
	ServiceAccountID       string    `json:"service_account_id"                    gorm:"column:service_account_id;primaryKey"`
	CreatedAt              time.Time `json:"created_at"                            gorm:"column:created_at"`
	UpdatedAt              time.Time `json:"updated_at"                            gorm:"column:updated_at"`
	ServiceName            string    `json:"service_name"                          gorm:"column:service_name;uniqueIndex"`
	HashedKey              string    `json:"-"                                     gorm:"column:hashed_key"`
	HashedBootstrapKey     string    `json:"-"                                     gorm:"column:hashed_bootstrap_key;default:''"`
	RoleID                 *string   `json:"role_id"                               gorm:"column:role_id"`
	AllowedCIDRs           []string  `json:"allowed_cidrs,omitempty"               gorm:"column:allowed_cidrs;serializer:json"`
	ClientCertFingerprints []string  `json:"client_cert_fingerprints,omitempty"    gorm:"column:client_cert_fingerprints;serializer:json"`
	Active                 bool      `json:"active"                                gorm:"column:active;default:true"`
}

// ServicePermissionRequest is a pending request from a service to add a permission
// to its service role. Status transitions: pending → approved | declined.
type ServicePermissionRequest struct {
	RequestID   string     `json:"request_id"   gorm:"column:request_id;primaryKey"`
	CreatedAt   time.Time  `json:"created_at"   gorm:"column:created_at"`
	UpdatedAt   time.Time  `json:"updated_at"   gorm:"column:updated_at"`
	ServiceName string     `json:"service_name" gorm:"column:service_name"`
	Name        string     `json:"name"         gorm:"column:name"`
	Service     string     `json:"service"      gorm:"column:service"`
	Actions     []string   `json:"actions"      gorm:"column:actions;serializer:json"`
	Resources   []string   `json:"resources"    gorm:"column:resources;serializer:json"`
	Status      string     `json:"status"       gorm:"column:status;default:pending"`
	ResolvedBy  *string    `json:"resolved_by"  gorm:"column:resolved_by"`
	ResolvedAt  *time.Time `json:"resolved_at"  gorm:"column:resolved_at"`
	Active      bool       `json:"active"       gorm:"column:active;default:true"`
}

// AuditLog is an append-only record of mutations to access-control entities.
// Rows are never updated or soft-deleted; the table acts as an immutable ledger.
type AuditLog struct {
	AuditLogID string    `json:"audit_log_id" gorm:"column:audit_log_id;primaryKey"`
	CreatedAt  time.Time `json:"created_at"   gorm:"column:created_at;autoCreateTime"`
	ActorID    string    `json:"actor_id"     gorm:"column:actor_id"`   // user_id or service_name
	ActorType  string    `json:"actor_type"   gorm:"column:actor_type"` // "user" | "service"
	Action     string    `json:"action"       gorm:"column:action"`     // e.g. "role.update"
	ResourceID string    `json:"resource_id"  gorm:"column:resource_id"`
	OrgID      *string   `json:"org_id"       gorm:"column:org_id"` // actor's org at the time of the action
	Detail     string    `json:"detail"       gorm:"column:detail"`
}

func (AuditLog) Update(_ context.Context) error { return errors.New("audit logs are immutable") }
func (AuditLog) Remove(_ context.Context) error { return errors.New("audit logs are immutable") }

// PermissionsCheck is an audit record of a single permission evaluation. Each
// call to GET /check_permissions that resolves successfully persists one row so
// that access decisions can be reviewed after the fact.
type PermissionsCheck struct {
	PermissionsCheckID string    `json:"permissions_check_id" gorm:"column:permissions_check_id;primaryKey"`
	CreatedAt          time.Time `json:"created_at"           gorm:"column:created_at"`
	UpdatedAt          time.Time `json:"updated_at"           gorm:"column:updated_at"`
	Service            string    `json:"service"              gorm:"column:service"`
	Action             string    `json:"action"               gorm:"column:action"`
	Resource           string    `json:"resource"             gorm:"column:resource"`
	UserID             string    `json:"user_id"              gorm:"column:user_id"`
	OrgID              *string   `json:"org_id"               gorm:"column:org_id"`
	TeamID             *string   `json:"team_id"               gorm:"column:team_id"`
	Granted            bool      `json:"granted"              gorm:"column:granted"`
	Active             bool      `json:"active"               gorm:"column:active;default:true"`
}

// Secret holds an AES-256-GCM encrypted value scoped to an org.
// The plaintext value is never returned by the API (write-only).
type Secret struct {
	SecretID   string    `json:"secret_id"  gorm:"column:secret_id;primaryKey"`
	OrgID      string    `json:"org_id"     gorm:"column:org_id;not null"`
	Name       string    `json:"name"       gorm:"column:name;not null"`
	Ciphertext []byte    `json:"-"          gorm:"column:ciphertext;not null"`
	CreatedBy  string    `json:"created_by" gorm:"column:created_by"`
	Active     bool      `json:"active"     gorm:"column:active;default:true"`
	CreatedAt  time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt  time.Time `json:"updated_at" gorm:"column:updated_at"`
}

func (Secret) TableName() string { return "secrets" }

// OrgSecretProvider records which secrets backend an org uses.
// Provider is one of: builtin, doppler, vault, aws_sm.
// Config holds encrypted JSON with provider-specific credentials.
type OrgSecretProvider struct {
	OrgID     string    `json:"org_id"    gorm:"column:org_id;primaryKey"`
	Provider  string    `json:"provider"  gorm:"column:provider;not null"`
	Config    []byte    `json:"-"         gorm:"column:config"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

func (OrgSecretProvider) TableName() string { return "org_secret_providers" }

// OAuthClient is a registered OAuth 2.0 / OIDC client (e.g. a Forgejo instance).
// The client secret is stored only as a bcrypt hash; the plaintext is returned
// once at creation time and never again.
// RoleID, when set, is used for client_credentials permission checks.
type OAuthClient struct {
	ClientID     string    `json:"client_id"            gorm:"column:client_id;primaryKey"`
	Name         string    `json:"name"                 gorm:"column:name"`
	SecretHash   string    `json:"-"                    gorm:"column:secret_hash"`
	RedirectURIs []string  `json:"redirect_uris"        gorm:"column:redirect_uris;serializer:json"`
	OrgID        string    `json:"org_id"               gorm:"column:org_id;default:''"`
	RoleID       *string   `json:"role_id,omitempty"    gorm:"column:role_id"`
	Active       bool      `json:"active"               gorm:"column:active;default:true"`
	CreatedAt    time.Time `json:"created_at"           gorm:"column:created_at"`
}

func (OAuthClient) TableName() string { return "oauth_clients" }

// OAuthCode is a short-lived single-use authorization code issued during the
// OAuth2 authorization_code flow. Codes expire after 10 minutes.
type OAuthCode struct {
	Code        string   `gorm:"column:code;primaryKey"`
	ClientID    string   `gorm:"column:client_id"`
	UserID      string   `gorm:"column:user_id"`
	RedirectURI string   `gorm:"column:redirect_uri"`
	Scopes      []string `gorm:"column:scopes;serializer:json"`
	// CodeChallenge binds the code to the client that STARTED the flow (RFC 7636).
	// Empty means the client did not use PKCE, in which case none is required at
	// exchange — the extension is per-request, so requiring it unconditionally would
	// break every client that predates it.
	CodeChallenge       string    `gorm:"column:code_challenge"`
	CodeChallengeMethod string    `gorm:"column:code_challenge_method"`
	Used                bool      `gorm:"column:used;default:false"`
	ExpiresAt           time.Time `gorm:"column:expires_at"`
	CreatedAt           time.Time `gorm:"column:created_at"`
}

func (OAuthCode) TableName() string { return "oauth_codes" }

// TOTPCredential stores an AES-256-GCM encrypted TOTP secret for a user.
// Confirmed is false until the user verifies the first code after enrollment.
// Only one active confirmed credential per user is permitted.
type TOTPCredential struct {
	CredentialID string `gorm:"column:credential_id;primaryKey"`
	UserID       string `gorm:"column:user_id"`
	EncSecret    []byte `gorm:"column:enc_secret"`
	Confirmed    bool   `gorm:"column:confirmed;default:false"`
	Active       bool   `gorm:"column:active;default:true"`
	// LastUsedCode and LastUsedAt track the most recently accepted TOTP code so
	// the same code cannot be replayed within the same 30-second time step.
	LastUsedCode string    `gorm:"column:last_used_code"`
	LastUsedAt   time.Time `gorm:"column:last_used_at"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// MFAPending is a short-lived token issued after password verification succeeds
// but the user has TOTP enabled. It expires after 2 minutes and is single-use.
// For OAuth flows the originating OAuth parameters are stored here so the TOTP
// completion step can reconstruct the flow.
type MFAPending struct {
	Token            string    `gorm:"column:token;primaryKey"`
	UserID           string    `gorm:"column:user_id"`
	ExpiresAt        time.Time `gorm:"column:expires_at"`
	Used             bool      `gorm:"column:used;default:false"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	OAuthClientID    string    `gorm:"column:oauth_client_id"`
	OAuthRedirectURI string    `gorm:"column:oauth_redirect_uri"`
	OAuthState       string    `gorm:"column:oauth_state"`
	OAuthScope       string    `gorm:"column:oauth_scope"`
	// The PKCE challenge has to survive the MFA detour like every other OAuth
	// parameter: the code is minted after MFA completes, and a challenge dropped here
	// would silently turn PKCE off for exactly the accounts with MFA enabled.
	OAuthCodeChallenge       string `gorm:"column:oauth_code_challenge"`
	OAuthCodeChallengeMethod string `gorm:"column:oauth_code_challenge_method"`
}

// RoleMembership assigns a role to a user in addition to the single role they already
// carry on User.RoleID.
//
// It exists because a namespace owner grants access by ASSIGNING one of their roles,
// and a user has exactly one direct role, one default role and one team — all single
// pointers. Without a membership table, being granted access to someone else's
// repository would mean surrendering your own role. Memberships are additive: the
// existing three sources are evaluated unchanged, and these are unioned on top.
type RoleMembership struct {
	RoleID    string    `json:"role_id"    gorm:"column:role_id;primaryKey"`
	UserID    string    `json:"user_id"    gorm:"column:user_id;primaryKey;index"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	// GrantedBy records who assigned it, so a namespace owner's grants are auditable
	// and revocable without guessing at intent.
	GrantedBy string `json:"granted_by" gorm:"column:granted_by;default:''"`
}

// PersonalToken is the record of a user-minted scoped token. It holds the token's
// LIFECYCLE, never the credential: the JWT is verified against the session's stored
// public key, so gatekeeper has no reason to keep the token itself, hashed or
// otherwise. SessionID is the session the credential is bound to (deactivating it is
// what makes revocation immediate) and RoleID is the attenuated role that decides
// what the token may do.
type PersonalToken struct {
	TokenID    string     `json:"token_id"     gorm:"column:token_id;primaryKey"`
	CreatedAt  time.Time  `json:"created_at"   gorm:"column:created_at;autoCreateTime"`
	UserID     string     `json:"-"            gorm:"column:user_id;index"`
	Name       string     `json:"name"         gorm:"column:name"`
	SessionID  string     `json:"-"            gorm:"column:session_id;index"`
	RoleID     string     `json:"-"            gorm:"column:role_id"`
	ExpiresAt  time.Time  `json:"expires_at"   gorm:"column:expires_at"`
	LastUsedAt *time.Time `json:"last_used_at" gorm:"column:last_used_at"`
	Active     bool       `json:"-"            gorm:"column:active;default:true"`
}

func (PersonalToken) TableName() string { return "personal_tokens" }

// Project is a first-class grouping of resources (repos, pipelines, boards, executions)
// that turns the former free-text "project" view-filter into a permission SCOPE. It
// lives under a namespace exactly like a repo does — "<username>" or "org/<orgName>" —
// so the owner of the namespace owns the project's scope, and confinement checks apply.
//
// Resources join a project by carrying its Slug in their own "project" column; a
// participating service then builds its RBAC resource string with a
// "projects/<slug>" segment (see docs/projects/design.md), so one wildcard permission
// on "<ns>/<service>/projects/<slug>/*" covers every resource the project holds in that
// service. Three built-in roles (viewer/developer/admin) hold exactly those wildcards;
// membership is the ordinary RoleMembership on whichever tier role.
type Project struct {
	ProjectID       string    `json:"project_id"        gorm:"column:project_id;primaryKey"`
	Slug            string    `json:"slug"              gorm:"column:slug;uniqueIndex"`
	// Namespace is "" for an unbound project (its own top-level namespace, addressed as
	// project/<slug>) or "org/<name>" when an org administers it. It is NEVER a username
	// — a project is not bound to a user; the creator is simply its first admin member.
	Namespace       string    `json:"namespace"         gorm:"column:namespace;default:''"`
	Name            string    `json:"name"              gorm:"column:name;default:''"`
	OwnerID         string    `json:"owner_id"          gorm:"column:owner_id;index"`
	ViewerRoleID    string    `json:"viewer_role_id"    gorm:"column:viewer_role_id;default:''"`
	DeveloperRoleID string    `json:"developer_role_id" gorm:"column:developer_role_id;default:''"`
	AdminRoleID     string    `json:"admin_role_id"     gorm:"column:admin_role_id;default:''"`
	CreatedAt       time.Time `json:"created_at"        gorm:"column:created_at;autoCreateTime"`
	Active          bool      `json:"-"                 gorm:"column:active;default:true"`
}

func (Project) TableName() string { return "projects" }
