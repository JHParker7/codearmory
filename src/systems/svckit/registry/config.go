package registry

import "encoding/json"

// RoleDef describes a role that operators should provision in gatekeeper
// for users of this service.
type RoleDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// EndpointDef describes one route that conductor routes and enforces RBAC for.
type EndpointDef struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Action      string `json:"action"`
	Resource    string `json:"resource"`
	Public      bool   `json:"public"`
	Description string `json:"description"`
}

// ServiceConfig is the full service manifest, typically loaded from service.json.
type ServiceConfig struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Roles       []RoleDef     `json:"roles"`
	Endpoints   []EndpointDef `json:"endpoints"`
}

// LoadConfig parses a ServiceConfig from JSON bytes (e.g. from an embedded file).
func LoadConfig(data []byte) (ServiceConfig, error) {
	var cfg ServiceConfig
	return cfg, json.Unmarshal(data, &cfg)
}
