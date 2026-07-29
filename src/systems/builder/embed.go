package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// The Helm chart ships only the core services; every non-core service is deployed AND
// registered by builder at runtime. These embedded definitions are the source of truth
// for "how to bring up service X": its registry manifest fragment (endpoints/actions/
// default_grants — moved out of the chart's registry-manifest.json so builder can
// register the routes/RBAC with the registry) plus the deploy metadata (image, port,
// infra deps, derived/admin secrets, inter-service env).
//
//go:embed files/services/*.json
var serviceDefsFS embed.FS

// serviceDef is everything builder needs to deploy and register one non-core service.
type serviceDef struct {
	// RegistryName is the service's identity in the registry, gatekeeper and the DB
	// row. It may contain '_' (e.g. "gitea_integration").
	RegistryName string `json:"registryName"`
	// K8sName is the DNS-1123 name used for ALL cluster objects (Deployment/Service/
	// Secret) and the service URL (e.g. "gitea-integration"). Never derive cluster
	// names from RegistryName — '_' is invalid in a k8s object name.
	K8sName     string `json:"k8sName"`
	ImageRepo   string `json:"imageRepo"` // image repo under the global registry + tag
	Port        int32  `json:"port"`
	ForwardAuth bool   `json:"forward_auth"`
	Description string `json:"description"`
	// UIPath, when set (e.g. "/ui"), is the path the service serves its embedded
	// mini-portal on; registered with the service so conductor advertises it via
	// GET /services and the portal shell renders it in an iframe.
	UIPath string `json:"ui_path"`
	// RegistryAccount: the service also needs a registry READ account to pull from the
	// registry (e.g. workflows → GET /actions). Builder ensures the account + a derived
	// REGISTRY_SERVICE_KEY when true.
	RegistryAccount bool `json:"registryAccount"`
	// GitConnectorBackend, when set, declares that this service is a git host whose
	// repos pipelines should be able to clone. Builder registers it with git_connector
	// as a platform-owned backend once it is deployed, so the link exists without an
	// operator linking it by hand. No credential is involved: git_connector mints a
	// per-clone, per-user gatekeeper token for these backends (see gitconnector.go).
	GitConnectorBackend *svcGitBackend    `json:"gitConnectorBackend,omitempty"`
	Infra               svcInfra          `json:"infra"`
	DerivedSecrets      []derivedSecret   `json:"derivedSecrets"`
	RequiredConfig      []string          `json:"requiredConfig"` // env keys the admin must supply before enable
	OptionalConfig      []string          `json:"optionalConfig"`
	SecretConfig        []string          `json:"secretConfig"` // config keys that are sensitive (stored in the Secret)
	EnvExtras           map[string]string `json:"envExtras"`    // static env; values may contain ${PREFIX}
	Endpoints           []svcEndpoint     `json:"endpoints"`
	// Actions are carried verbatim (json.RawMessage) so body_transforms/async pass
	// through to the registry untouched.
	Actions       []json.RawMessage `json:"actions"`
	DefaultGrants []svcDefaultGrant `json:"default_grants"`
}

// svcGitBackend is a service's registration as a clone source in git_connector.
// Name is the backend's display name there; it defaults to the service's k8s name.
type svcGitBackend struct {
	Name string `json:"name"`
}

type svcInfra struct {
	// ManagedRedis: when the admin supplies no REDIS_URL, deploy a stateless in-cluster
	// Redis and point the service at it. Supplying an external REDIS_URL opts out (and
	// tears any builder-managed Redis back down). The store is ephemeral — fit for a
	// cache, not durable state. REDIS_URL must stay in secretConfig (not requiredConfig)
	// so the env is wired but enable is not blocked when it is absent.
	ManagedRedis bool `json:"managedRedis"`
	// Persistence, when set, declares that the service keeps durable state on a volume:
	// builder creates a PVC (create-if-absent — never updated, never deleted) and mounts
	// it at MountPath. Declaring it changes how the workload is rolled out: see
	// persistenceFor / strategyFor / replicasFor.
	Persistence *svcPersistence `json:"persistence,omitempty"`
}

// svcPersistence is a service's durable-volume claim. AccessMode is the design
// decision underneath it: ReadWriteOnce is one writer — replicas are clamped to 1, the
// rollout becomes Recreate (brief downtime on deploy) and periodic rotation is off,
// because a second pod can never attach the volume the first still holds.
// ReadWriteMany allows N replicas and keeps the surge rollout, but only if the backing
// store gives real atomic creates and working flock.
type svcPersistence struct {
	MountPath    string `json:"mountPath"`    // where the volume is mounted in the container
	Size         string `json:"size"`         // requested capacity, e.g. "20Gi"
	StorageClass string `json:"storageClass"` // "" => cluster default
	AccessMode   string `json:"accessMode"`   // ReadWriteOnce (default) | ReadWriteMany
}

// accessMode resolves the declared mode, defaulting to the safe single-writer one.
func (p svcPersistence) accessMode() corev1.PersistentVolumeAccessMode {
	if strings.EqualFold(strings.TrimSpace(p.AccessMode), string(corev1.ReadWriteMany)) {
		return corev1.ReadWriteMany
	}
	return corev1.ReadWriteOnce
}

// singleWriter reports whether the volume admits only one pod at a time — the flag
// every rollout decision downstream keys off.
func (p svcPersistence) singleWriter() bool {
	return p.accessMode() != corev1.ReadWriteMany
}

type derivedSecret struct {
	EnvVar string `json:"envVar"` // env var wired (via secretKeyRef) on the workload
	Kind   string `json:"kind"`   // "shared" (cross-service) or "private" (per-service)
	Name   string `json:"name"`   // secret key name
}

type svcEndpoint struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Public   bool   `json:"public,omitempty"`
}

type svcDefaultGrant struct {
	GrantOn   string   `json:"grant_on"`
	Actions   []string `json:"actions"`
	Resources []string `json:"resources"`
}

var (
	embeddedDefsOnce sync.Once
	embeddedDefs     map[string]serviceDef // keyed by RegistryName
	embeddedDefsErr  error
)

func loadEmbeddedDefs() (map[string]serviceDef, error) {
	embeddedDefsOnce.Do(func() {
		entries, err := serviceDefsFS.ReadDir("files/services")
		if err != nil {
			embeddedDefsErr = err
			return
		}
		defs := make(map[string]serviceDef, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			raw, err := serviceDefsFS.ReadFile("files/services/" + e.Name())
			if err != nil {
				embeddedDefsErr = fmt.Errorf("read %s: %w", e.Name(), err)
				return
			}
			var d serviceDef
			if err := json.Unmarshal(raw, &d); err != nil {
				embeddedDefsErr = fmt.Errorf("parse %s: %w", e.Name(), err)
				return
			}
			switch {
			case d.RegistryName == "":
				embeddedDefsErr = fmt.Errorf("%s: missing registryName", e.Name())
				return
			case d.K8sName == "":
				embeddedDefsErr = fmt.Errorf("%s: missing k8sName", e.Name())
				return
			case coreServices[d.RegistryName]:
				embeddedDefsErr = fmt.Errorf("%s: core service must not be in the builder catalog", d.RegistryName)
				return
			}
			defs[d.RegistryName] = d
		}
		embeddedDefs = defs
	})
	return embeddedDefs, embeddedDefsErr
}

// embeddedServiceDef returns the builder-embedded definition for a service, if any.
func embeddedServiceDef(registryName string) (serviceDef, bool) {
	defs, err := loadEmbeddedDefs()
	if err != nil {
		return serviceDef{}, false
	}
	d, ok := defs[registryName]
	return d, ok
}
