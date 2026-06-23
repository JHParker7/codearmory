package main

import "time"

const maxBodyBytes = 64 * 1024

// defaultOrgID is the sentinel OrgID for the "default org" baseline. A real org
// inherits every default-scope row unless it carries its own override. The HTTP
// API addresses this scope with the literal path id "default" (see api). We store
// a non-NULL sentinel rather than SQL NULL so the (org_id, service_name) unique
// index behaves — Postgres treats NULLs as distinct.
const defaultOrgID = "default"

// coreServices are control-plane services that are always available to every org
// and can never be toggled off — disabling any of them would sever the platform.
var coreServices = map[string]bool{
	"gatekeeper": true,
	"conductor":  true,
	"registry":   true,
	"builder":    true,
}

// kinds of org-service rows.
const (
	kindPlatform = "platform" // a toggle/override of a registered platform service
	kindCustom   = "custom"   // an org-declared service (reconciled by the Phase 2 controller)
)

// OrgService is the desired state for one service within one org. The default-org
// rows (OrgID == defaultOrgID) form the baseline every org inherits; an org row
// with the same ServiceName overrides the baseline. Enabled deliberately has no
// gorm `default:` tag: a default tag makes Create drop a false value and persist
// the DB default, which would silently re-enable a service the admin disabled.
type OrgService struct {
	OrgServiceID string         `json:"org_service_id" gorm:"column:org_service_id;primaryKey"`
	OrgID        string         `json:"org_id"         gorm:"column:org_id;not null;uniqueIndex:org_services_scope_key"`
	ServiceName  string         `json:"service"        gorm:"column:service_name;not null;uniqueIndex:org_services_scope_key"`
	Enabled      bool           `json:"enabled"        gorm:"column:enabled"`
	Kind         string         `json:"kind"           gorm:"column:kind;default:'platform'"`
	Config       map[string]any `json:"config"         gorm:"column:config;serializer:json"`
	Image        string         `json:"image,omitempty"       gorm:"column:image;default:''"`
	Port         int            `json:"port,omitempty"        gorm:"column:port;default:0"`
	Description  string         `json:"description,omitempty" gorm:"column:description;default:''"`
	// DBURLCiphertext is the admin-supplied per-service database URL, AES-256-GCM
	// encrypted (AAD = service name) and never serialized. DBHost is a redacted
	// host:port/db kept only for display.
	DBURLCiphertext []byte `json:"-"                 gorm:"column:db_url_ct"`
	DBHost          string `json:"-"                 gorm:"column:db_host;default:''"`
	// SecretsCiphertext is the admin-supplied sensitive config (REDIS_URL,
	// GITEA_ADMIN_TOKEN, REGISTRY_PASSWORD, …) as an AES-256-GCM-encrypted JSON map
	// (AAD = service name), never serialized. Builder writes each entry into the
	// service's Secret under its conventional key on provision.
	SecretsCiphertext []byte    `json:"-"                 gorm:"column:secrets_ct"`
	CreatedAt         time.Time `json:"created_at"        gorm:"column:created_at"`
	UpdatedAt       time.Time `json:"updated_at"        gorm:"column:updated_at"`
}

func (OrgService) TableName() string { return "org_services" }

// serviceView is one row in the merged effective view returned to admins. It
// overlays the desired-state rows on top of the live service catalog so the UI
// can list every available service, its effective enabled state, and where that
// state comes from.
type serviceView struct {
	Service     string         `json:"service"`
	Enabled     bool           `json:"enabled"`
	Kind        string         `json:"kind"`
	Source      string         `json:"source"` // "default" | "override" | "custom" | "catalog"
	Config      map[string]any `json:"config,omitempty"`
	Image       string         `json:"image,omitempty"`
	Port        int            `json:"port,omitempty"`
	Description string         `json:"description,omitempty"`
	Core        bool           `json:"core,omitempty"`
	// DBConfigured reports whether a per-service DB URL has been stored; DBHost is
	// the redacted host for display. The URL itself is never returned.
	DBConfigured bool   `json:"db_configured"`
	DBHost       string `json:"db_host,omitempty"`
}

// setServiceRequest is the PUT body for configuring a service for an org. DBUrl is
// write-only: it is encrypted on receipt and never read back.
type setServiceRequest struct {
	Enabled     *bool          `json:"enabled"`
	Kind        string         `json:"kind"`
	Config      map[string]any `json:"config"`
	Image       string         `json:"image"`
	Port        int            `json:"port"`
	Description string         `json:"description"`
	DBUrl       string         `json:"db_url"`
	// Secrets is admin-supplied sensitive config keyed by env var name (e.g.
	// REDIS_URL, GITEA_ADMIN_TOKEN, REGISTRY_PASSWORD). Write-only: encrypted on
	// receipt and never read back. Non-sensitive config goes in Config.
	Secrets map[string]string `json:"secrets"`
}

// effectiveResponse is the internal disabled-set returned to the gatekeeper gate.
type effectiveResponse struct {
	OrgID    string   `json:"org_id"`
	Disabled []string `json:"disabled"`
}
