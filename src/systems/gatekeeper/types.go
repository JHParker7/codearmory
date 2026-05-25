package main

import "time"

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
type Role struct {
	RoleID         string    `json:"role_id"          gorm:"column:role_id;primaryKey"`
	CreatedAt      time.Time `json:"created_at"       gorm:"column:created_at"`
	UpdatedAt      time.Time `json:"updated_at"       gorm:"column:updated_at"`
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

// Session holds an active JWT for a User along with the public key used to
// verify the token signature.
type Session struct {
	SessionID string    `gorm:"column:session_id;primaryKey"`
	JWT       string    `gorm:"column:jwt"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
	UserID    string    `gorm:"column:user_id"`
	ExpiresAt time.Time `gorm:"column:expires_at"`
	PubKey    string    `gorm:"column:pub_key"`
	Active    bool      `gorm:"column:active;default:true"`
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

// Service is a backend service registered with Gatekeeper. Conductor reads this
// table to discover available services and their internal endpoint URLs.
type Service struct {
	ServiceID string    `json:"service_id" gorm:"column:service_id;primaryKey"`
	Name      string    `json:"name"       gorm:"column:name;uniqueIndex"`
	URL       string    `json:"url"        gorm:"column:url"`
	Active    bool      `json:"active"     gorm:"column:active;default:true"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt time.Time `json:"updated_at" gorm:"column:updated_at"`
}

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
