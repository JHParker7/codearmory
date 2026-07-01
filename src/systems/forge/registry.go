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

// isKernelIsolatedBackendType reports whether a backend type runs each job under
// its OWN kernel rather than the shared host kernel, so the job's root can never
// reach the host kernel. Only these backends may run a privileged runner class
// (root + writable rootfs): on a shared-kernel container backend (docker/kubernetes)
// that would be a host escape. This is the single source of truth consulted by both
// the API gate (validatePrivilegedBackend) and the runtime build below, so the two
// never disagree. "kata" pins a VM-isolating RuntimeClass (a hardware-virtualized
// microVM guest kernel); "gvisor" pins a gVisor RuntimeClass whose userspace kernel
// (the Sentry) services every syscall the job makes — a different mechanism but the
// same guarantee that container-root is contained away from the host kernel. (gVisor
// is a kernel boundary, not a network one — it does not confine egress, so unlike the
// VM backends it keeps the egress proxy; that is decided in Helm/builder, not here.)
func isKernelIsolatedBackendType(t string) bool {
	return t == "kata" || t == "gvisor"
}

// buildRuntime constructs the concrete Runtime for a backend. docker and
// kubernetes keep reading their existing env vars (FORGE_NETWORK_MODE,
// K8S_NAMESPACE, …) so an existing single-runtime deployment is unchanged. kata
// is the kubernetes runtime pinned to a VM-isolating RuntimeClass from config
// (validateKataBackend guarantees it is set).
func buildRuntime(b RuntimeBackend) (Runtime, error) {
	switch b.Type {
	case "kubernetes":
		// Optional per-backend RuntimeClass; empty falls back to K8S_RUNTIME_CLASS.
		// Not kernel-isolated: a shared host kernel, so privileged runner classes are
		// ignored and the locked-down sandbox is always applied.
		return newKubernetesRuntime(b.Config[k8sKeyRuntimeClass], isKernelIsolatedBackendType(b.Type))
	case "kata":
		// Kata is the Kubernetes runtime pinned to a VM-isolating RuntimeClass. The
		// RuntimeClass is mandatory: without one the pod silently falls back to the
		// cluster default (runc) with no VM isolation. Enforce it here, at the point
		// of use — not only in create/update validation — so a kata backend that
		// reaches this code by any other path (the RUNTIME=kata startup seed with no
		// K8S_RUNTIME_CLASS, a direct DB insert, an older replica) fails loudly
		// instead of running unsandboxed.
		if resolveRuntimeClass(b.Config[k8sKeyRuntimeClass]) == nil {
			return nil, fmt.Errorf("kata runtime requires a RuntimeClass: set the backend %q config key or the K8S_RUNTIME_CLASS env var", k8sKeyRuntimeClass)
		}
		// Kernel-isolated: the job runs inside a microVM, so a runner class may opt
		// into running as root (RunnerClass.Privileged) for package managers / apt.
		return newKubernetesRuntime(b.Config[k8sKeyRuntimeClass], isKernelIsolatedBackendType(b.Type))
	case "gvisor":
		// gVisor (handler runsc) is the Kubernetes runtime pinned to a gVisor
		// RuntimeClass: a userspace kernel (the Sentry) intercepts the job's syscalls
		// in software, so it needs NO hardware virtualization (/dev/kvm or nested
		// virt) — the reason to choose it over kata. The RuntimeClass is mandatory,
		// exactly as for kata: without it the pod falls back to runc with no gVisor
		// sandbox at all, so enforce it here at the point of use. Kernel-isolated: the
		// job's root is contained by the Sentry, so a runner class may run privileged
		// (root + writable rootfs) just like kata. gVisor is a kernel boundary, not a
		// network one, so it does NOT confine egress — Helm/builder keep the egress
		// proxy for it (unlike the VM backends).
		if resolveRuntimeClass(b.Config[k8sKeyRuntimeClass]) == nil {
			return nil, fmt.Errorf("gvisor runtime requires a RuntimeClass: set the backend %q config key or the K8S_RUNTIME_CLASS env var", k8sKeyRuntimeClass)
		}
		return newKubernetesRuntime(b.Config[k8sKeyRuntimeClass], isKernelIsolatedBackendType(b.Type))
	case "docker":
		return newDockerRuntime()
	default:
		return nil, fmt.Errorf("unknown runtime type %q: expected docker, kubernetes, gvisor or kata", b.Type)
	}
}
