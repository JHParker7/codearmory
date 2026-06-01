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
	OrgName   string    `json:"org_name"   gorm:"column:org_name"`
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
type User struct {
	UserID         string    `gorm:"column:user_id;primaryKey"`
	HashedPassword string    `gorm:"column:hashed_password"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
	Firstname      string    `gorm:"column:firstname"`
	Lastname       string    `gorm:"column:lastname"`
	Email          string    `gorm:"column:email;uniqueIndex"`
	OrgID          *string   `gorm:"column:org_id"`
	RoleID         *string   `gorm:"column:role_id"`
	TeamID         *string   `gorm:"column:team_id"`
	Username       string    `gorm:"column:username;uniqueIndex"`
	Active         bool      `gorm:"column:active;default:true"`
}

// Session holds the per-session ECDSA public key used to verify the JWT signature.
// The JWT itself is never stored — authMiddleware re-validates the signature on
// each request using the stored PubKey, so retaining the token would be redundant
// and would expose all active sessions on a database breach.
// ScopedRoleID, when set, restricts permission checks to only the permissions in
// that role — regardless of the user's own role or team membership. Used by
// run-scoped tokens to enforce the workflow's minimal permission set.
type Session struct {
	SessionID    string    `gorm:"column:session_id;primaryKey"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
	UserID       string    `gorm:"column:user_id"`
	ExpiresAt    time.Time `gorm:"column:expires_at"`
	PubKey       string    `gorm:"column:pub_key"`
	Active       bool      `gorm:"column:active;default:true"`
	ScopedRoleID *string   `gorm:"column:scoped_role_id"`
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
	ServiceAccountID       string   `json:"service_account_id"        gorm:"column:service_account_id;primaryKey"`
	CreatedAt              time.Time `json:"created_at"               gorm:"column:created_at"`
	UpdatedAt              time.Time `json:"updated_at"               gorm:"column:updated_at"`
	ServiceName            string   `json:"service_name"              gorm:"column:service_name;uniqueIndex"`
	HashedKey              string   `json:"-"                         gorm:"column:hashed_key"`
	RoleID                 *string  `json:"role_id"                   gorm:"column:role_id"`
	AllowedCIDRs           []string `json:"allowed_cidrs,omitempty"   gorm:"column:allowed_cidrs;serializer:json"`
	ClientCertFingerprints []string `json:"client_cert_fingerprints,omitempty" gorm:"column:client_cert_fingerprints;serializer:json"`
	Active                 bool     `json:"active"                    gorm:"column:active;default:true"`
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
type OAuthClient struct {
	ClientID     string    `json:"client_id"     gorm:"column:client_id;primaryKey"`
	Name         string    `json:"name"          gorm:"column:name"`
	SecretHash   string    `json:"-"             gorm:"column:secret_hash"`
	RedirectURIs []string  `json:"redirect_uris" gorm:"column:redirect_uris;serializer:json"`
	OrgID        string    `json:"org_id"        gorm:"column:org_id;default:''"`
	Active       bool      `json:"active"        gorm:"column:active;default:true"`
	CreatedAt    time.Time `json:"created_at"    gorm:"column:created_at"`
}

func (OAuthClient) TableName() string { return "oauth_clients" }

// OAuthCode is a short-lived single-use authorization code issued during the
// OAuth2 authorization_code flow. Codes expire after 10 minutes.
type OAuthCode struct {
	Code        string    `gorm:"column:code;primaryKey"`
	ClientID    string    `gorm:"column:client_id"`
	UserID      string    `gorm:"column:user_id"`
	RedirectURI string    `gorm:"column:redirect_uri"`
	Scopes      []string  `gorm:"column:scopes;serializer:json"`
	Used        bool      `gorm:"column:used;default:false"`
	ExpiresAt   time.Time `gorm:"column:expires_at"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (OAuthCode) TableName() string { return "oauth_codes" }
