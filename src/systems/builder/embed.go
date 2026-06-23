package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"sync"
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
	// RegistryAccount: the service also needs a registry READ account to pull from the
	// registry (e.g. workflows → GET /actions). Builder ensures the account + a derived
	// REGISTRY_SERVICE_KEY when true.
	RegistryAccount bool              `json:"registryAccount"`
	Infra           svcInfra          `json:"infra"`
	DerivedSecrets  []derivedSecret   `json:"derivedSecrets"`
	RequiredConfig  []string          `json:"requiredConfig"` // env keys the admin must supply before enable
	OptionalConfig  []string          `json:"optionalConfig"`
	SecretConfig    []string          `json:"secretConfig"` // config keys that are sensitive (stored in the Secret)
	EnvExtras       map[string]string `json:"envExtras"`    // static env; values may contain ${PREFIX}
	Endpoints       []svcEndpoint     `json:"endpoints"`
	// Actions are carried verbatim (json.RawMessage) so body_transforms/async pass
	// through to the registry untouched.
	Actions       []json.RawMessage `json:"actions"`
	DefaultGrants []svcDefaultGrant `json:"default_grants"`
}

type svcInfra struct {
	EgressProxy   bool `json:"egressProxy"`   // deploy forge's egress-proxy workload
	NetworkPolicy bool `json:"networkPolicy"` // deploy a NetworkPolicy in the exec namespace
	ForgeExecRBAC bool `json:"forgeExecRBAC"` // ensure SA+Role+RoleBinding in the forge exec namespace
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
