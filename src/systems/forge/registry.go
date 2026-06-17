package main

import (
	"context"
	"fmt"
	"sync"
)

// runtimeRegistry resolves a backend name to a concrete Runtime, building it
// lazily from the backend's stored config and caching the result. It replaces
// the single process-wide runtime: each execution names the backend it runs on
// (snapshotted at submit), and the worker asks the registry for it.
//
// The build func is a seam so tests can supply runtimes without a live docker or
// kubernetes client; production uses buildRuntime.
type runtimeRegistry struct {
	mu    sync.Mutex
	cache map[string]Runtime
	build func(RuntimeBackend) (Runtime, error)
}

func newRuntimeRegistry() *runtimeRegistry {
	return &runtimeRegistry{cache: map[string]Runtime{}, build: buildRuntime}
}

// Get returns the Runtime for the named backend, building and caching the
// concrete runtime on first use. An empty name resolves to "default". A missing
// or disabled backend, or one that fails to build, returns an error — the worker
// turns that into a failed execution rather than crashing the process.
//
// The enabled/exists check (runtimeBackendSpec) is re-read from the database on
// every call, deliberately not cached: a backend disabled or deleted out-of-band
// (another replica, a direct DB change) stops serving immediately rather than
// living on in a stale cache. Only the expensive concrete runtime is cached. The
// DB read is intentionally outside the mutex so a slow query can't stall every
// other worker's resolution.
func (r *runtimeRegistry) Get(ctx context.Context, name string) (Runtime, error) {
	if name == "" {
		name = "default"
	}
	backend, err := runtimeBackendSpec(ctx, name)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if rt, ok := r.cache[name]; ok {
		return rt, nil
	}
	rt, err := r.build(backend)
	if err != nil {
		return nil, fmt.Errorf("build runtime backend %q: %w", name, err)
	}
	r.cache[name] = rt
	return rt, nil
}

// Evict drops the cached Runtime for a backend so a subsequent Get rebuilds it
// from current config. Called by the update/delete handlers so admin edits apply
// to new jobs without a process restart.
func (r *runtimeRegistry) Evict(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, name)
}

// buildRuntime constructs the concrete Runtime for a backend. docker and
// kubernetes keep reading their existing env vars (FORGE_NETWORK_MODE,
// K8S_NAMESPACE, …) so an existing single-runtime deployment is unchanged; the
// proxmox case (added in a later phase) reads backend.Config / SecretRefs.
func buildRuntime(b RuntimeBackend) (Runtime, error) {
	switch b.Type {
	case "kubernetes":
		return newKubernetesRuntime()
	case "docker":
		return newDockerRuntime()
	case "proxmox":
		return newProxmoxRuntime(b)
	default:
		return nil, fmt.Errorf("unknown runtime type %q: expected docker, kubernetes or proxmox", b.Type)
	}
}
