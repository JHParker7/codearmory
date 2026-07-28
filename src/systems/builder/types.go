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
// They are shipped by the Helm chart (not deployed/registered by builder) and so are
// excluded from the catalog, never reconciled, and rejected by the set-service API.
// forge + workflows are the CI/CD pair: the chart deploys them and registers them via
// the registry manifest, so builder treats them as core like the rest. tickets and
// containers were likewise promoted to core: the chart deploys their published images
// and registers them via the registry manifest. The core git service is the
// backend-agnostic credential broker (src/systems/git); gitea_integration (Forgejo
// repo management) is NOT core — most users do not run Forgejo, so builder deploys it
// on demand from files/services/gitea_integration.json.
var coreServices = map[string]bool{
	"artifacts":     true,
	"gatekeeper":    true,
	"conductor":     true,
	"registry":      true,
	"builder":       true,
	"forge":         true,
	"workflows":     true,
	"tickets":       true,
	"git_connector": true,
	"containers":    true,
}

// comingSoonServices are the platform services whose source was spun out into its
// own codearmory-<svc> repo and so is NOT in this monorepo. This repo's CI does not
// build their images yet, so on the core-only bundle they are surfaced as
// "coming soon": they still appear in the builder catalog, but the effective view
// forces them disabled and the set-service API refuses to enable them until their
// images ship. Keep this in sync with the spun-off set in CLAUDE.md.
var comingSoonServices = map[string]bool{
	"argo":              true,
	"blueprints":        true,
	"chaos":             true,
	"gitea_integration": true,
	"mcp":               true,
	"notifications":     true,
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
	// Registry overrides BUILDER_IMAGE_REGISTRY for this service alone.
	//
	// Without it, Tag is only usable when the platform-wide registry happens to be
	// right for the service, because the two are composed. That assumption does not
	// hold: an install whose BUILDER_IMAGE_REGISTRY points somewhere the service's
	// images are NOT published resolves every tag to an unpullable reference, and the
	// only escape is to restate the whole image on every deploy. Registry + Tag make a
	// service's image fully determined per service, with no dependency on a global
	// that may not match the cluster it runs on.
	Registry string `json:"registry,omitempty" gorm:"column:registry;default:''"`
	// Tag pins the image TAG. Composed with Registry when set, else with
	// BUILDER_IMAGE_REGISTRY. It is the knob a CI pipeline wants: "deploy the build I
	// just pushed" is a tag change, not a new image reference. Ignored when Image is
	// set, which is fully explicit.
	Tag string `json:"tag,omitempty" gorm:"column:tag;default:''"`
	// PullPolicy overrides the container imagePullPolicy (Always|IfNotPresent|Never).
	// Empty leaves it unset, which is Kubernetes' own default — Always for :latest,
	// IfNotPresent otherwise. Two cases need it: an image built straight onto the node
	// (minikube) that must never be pulled, hence Never; and a MUTABLE tag, where
	// IfNotPresent would keep serving the node's cached copy of a previous build, so
	// Always is required for a redeploy to mean anything.
	PullPolicy  string `json:"pull_policy,omitempty" gorm:"column:pull_policy;default:''"`
	Port        int    `json:"port,omitempty"        gorm:"column:port;default:0"`
	Description string `json:"description,omitempty" gorm:"column:description;default:''"`
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
	UpdatedAt         time.Time `json:"updated_at"        gorm:"column:updated_at"`
}

func (OrgService) TableName() string { return "org_services" }

// serviceView is one row in the merged effective view returned to admins. It
// overlays the desired-state rows on top of the live service catalog so the UI
// can list every available service, its effective enabled state, and where that
// state comes from.
type serviceView struct {
	Service string         `json:"service"`
	Enabled bool           `json:"enabled"`
	Kind    string         `json:"kind"`
	Source  string         `json:"source"` // "core" | "default" | "override" | "custom" | "registry" | "catalog"
	Config  map[string]any `json:"config,omitempty"`
	Image   string         `json:"image,omitempty"`
	// Tag/PullPolicy surface the deploy-target overrides so a caller (the portal, or a
	// pipeline reading back its own write) can see which build is actually targeted.
	Registry    string `json:"registry,omitempty"`
	Tag         string `json:"tag,omitempty"`
	PullPolicy  string `json:"pull_policy,omitempty"`
	Port        int    `json:"port,omitempty"`
	Description string `json:"description,omitempty"`
	Core        bool   `json:"core,omitempty"`
	// ComingSoon flags a spun-off service (source not in this repo, see
	// comingSoonServices) that cannot be enabled yet. The view is forced disabled
	// and every UI shows a "coming soon" badge instead of an enable control.
	ComingSoon bool `json:"coming_soon,omitempty"`
	// DBConfigured reports whether a per-service DB URL has been stored; DBHost is
	// the redacted host for display. The URL itself is never returned.
	DBConfigured bool   `json:"db_configured"`
	DBHost       string `json:"db_host,omitempty"`
}

// setServiceRequest is the PUT body for configuring a service for an org. DBUrl is
// write-only: it is encrypted on receipt and never read back.
type setServiceRequest struct {
	Enabled *bool          `json:"enabled"`
	Kind    string         `json:"kind"`
	Config  map[string]any `json:"config"`
	Image   string         `json:"image"`
	// Tag pins only the image tag, leaving registry/repo to the platform defaults.
	// PullPolicy overrides imagePullPolicy. See the OrgService fields of the same name.
	Registry    string `json:"registry"`
	Tag         string `json:"tag"`
	PullPolicy  string `json:"pull_policy"`
	Port        int    `json:"port"`
	Description string `json:"description"`
	DBUrl       string `json:"db_url"`
	// MaintenanceDBUrl is used only by the sql db backend: a CREATEDB(/CREATEROLE)
	// connection builder uses ONCE to provision the per-service database, then discards
	// (it is never stored). Write-only; never read back. Empty falls back to the
	// globally-configured BUILDER_DB_SQL_MAINTENANCE_URL.
	MaintenanceDBUrl string `json:"maintenance_db_url"`
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
