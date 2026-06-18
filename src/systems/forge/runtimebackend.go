package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// validRuntimeTypes is the set of backend types buildRuntime knows how to
// construct. Kept in sync with the buildRuntime switch in registry.go.
var validRuntimeTypes = map[string]bool{
	"docker":     true,
	"kubernetes": true,
	"proxmox":    true,
	"kata":       true,
}

// requiredProxmoxConfigKeys must be present (non-empty) in a proxmox backend's
// config before it will be accepted.
var requiredProxmoxConfigKeys = []string{pmKeyURL, pmKeyNode, pmKeyTemplate, pmKeyStorage, pmKeyBridge}

// validateProxmoxBackend checks the config/secret shape a proxmox backend needs
// so a misconfiguration is rejected at create/update time rather than surfacing
// only when a job tries to run.
func validateProxmoxBackend(b runtimeBackendBody) error {
	for _, k := range requiredProxmoxConfigKeys {
		if strings.TrimSpace(b.Config[k]) == "" {
			return fmt.Errorf("proxmox config key %q is required", k)
		}
	}
	if _, err := strconv.Atoi(strings.TrimSpace(b.Config[pmKeyTemplate])); err != nil {
		return fmt.Errorf("proxmox config key %q must be an integer VMID", pmKeyTemplate)
	}
	if strings.TrimSpace(b.SecretRefs[pmSecretToken]) == "" {
		return fmt.Errorf("proxmox secret_ref %q is required", pmSecretToken)
	}
	return nil
}

// validateKataBackend checks a kata backend names a RuntimeClass. Kata is the
// kubernetes runtime pinned to a VM-isolating RuntimeClass (e.g. kata-qemu,
// kata-fc); without one the pod would silently fall back to the cluster's default
// runtime (runc) and run with no VM isolation at all, so an empty runtime_class is
// a configuration error rather than a permissive default.
func validateKataBackend(b runtimeBackendBody) error {
	if strings.TrimSpace(b.Config[k8sKeyRuntimeClass]) == "" {
		return fmt.Errorf("kata config key %q is required (the Kubernetes RuntimeClass, e.g. kata-qemu)", k8sKeyRuntimeClass)
	}
	return nil
}

// migrateAndSeedRuntimeBackends creates the runtime_backends table and seeds the
// "default" backend from the legacy RUNTIME env var. An existing RUNTIME-only
// deployment thus comes up with one backend named "default" pointing at the same
// runtime it always used — no config change required.
func migrateAndSeedRuntimeBackends() error {
	g := connect()
	if err := g.AutoMigrate(&RuntimeBackend{}); err != nil {
		return fmt.Errorf("migrate runtime_backends: %w", err)
	}
	def := RuntimeBackend{
		Name:       "default",
		Type:       defaultRuntimeType(),
		Enabled:    true,
		Config:     map[string]string{},
		SecretRefs: map[string]string{},
	}
	g.Where(RuntimeBackend{Name: def.Name}).FirstOrCreate(&def)
	return nil
}

// runtimeBackendSpec fetches the named backend from the database. Returns an
// error if the backend does not exist or is disabled — mirrors runnerClassSpec.
func runtimeBackendSpec(ctx context.Context, name string) (RuntimeBackend, error) {
	row, err := (RuntimeBackend{Name: name}).Get(ctx)
	if err != nil {
		return RuntimeBackend{}, fmt.Errorf("runtime backend %q not found", name)
	}
	b := row.(RuntimeBackend)
	if !b.Enabled {
		return RuntimeBackend{}, fmt.Errorf("runtime backend %q is disabled", name)
	}
	return b, nil
}
