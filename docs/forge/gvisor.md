# Forge — gVisor kernel-sandboxed runtime

The docker and kubernetes runtimes isolate jobs at the *container* layer: a
shared host kernel with read-only rootfs, dropped capabilities, a non-root UID,
seccomp, and an allowlisted egress proxy. That is a strong boundary, but it is
still the host kernel — a kernel-level escape (a syscall or namespace bug) breaks
out of every container on the node.

The **gvisor** runtime moves the boundary to a *userspace kernel* without changing
how jobs are submitted or scheduled. It is the existing Kubernetes runtime with the
pod's `runtimeClassName` pinned to a [gVisor](https://gvisor.dev) (`runsc`)
RuntimeClass. The kubelet hands the pod to the gVisor runtime, whose **Sentry**
process implements the Linux system-call surface in userspace and services every
syscall the job makes; the host kernel only ever sees the Sentry's own, tightly
seccomp-filtered calls. To Forge it is still a normal Job: the same `RunResult`
contract, timeout, cancel, log collection, and TTL cleanup apply unchanged, and
every container hardening setting (non-root, read-only rootfs, `drop ALL`
capabilities, no service-account token, seccomp) is still set on top of the gVisor
boundary.

**gvisor vs. kata.** Both give the job its *own kernel* rather than the shared host
kernel, so both reach kernel-level isolation and both may run privileged jobs (see
*Privileged jobs* below). The difference is the mechanism and its cost:

| | kata | gvisor |
|---|---|---|
| Boundary | hardware-virtualized microVM (a real guest kernel) | userspace kernel (the Sentry), ptrace/KVM platform |
| Needs `/dev/kvm` / nested virt | **yes** | **no** |
| Confines egress on its own | yes (VM network boundary → egress proxy skipped) | **no** — keeps the egress proxy + NetworkPolicy |
| Privileged runner classes | allowed | allowed |

Choose **gvisor** when you want kernel-level isolation but your nodes lack nested
virtualization / `/dev/kvm` (most managed node pools) — it runs the Sentry on the
default ptrace platform with no special hardware. Choose **kata** when you have
hardware virtualization and want a real guest kernel. To **build container images**,
use forge's built-in daemonless image build (Kaniko on gvisor) rather than a Docker
daemon.

```
POST /executions (runner_class → backend snapshot)
   │
   ▼  worker resolves the execution's backend
KubernetesRuntime.Run  (runtimeClassName = <gVisor RuntimeClass>)
   │  1. create Job in K8S_NAMESPACE (BackoffLimit 0, ActiveDeadlineSeconds)
   │  2. kubelet → gVisor (runsc) → run the runner container under the Sentry
   │  3. poll job status; collect the runner container's logs (1 MB cap)
   │  4. delete the Job (TTLSecondsAfterFinished is the crash safety net)
   ▼
RunResult {stdout, stderr, exit_code}   ← identical contract to docker/k8s
```

## 1. Install gVisor in the cluster (prerequisite)

gVisor is a *cluster* component, installed by the operator — Forge does not create
the `RuntimeClass`. Install the `runsc` containerd shim on each node and register a
RuntimeClass. The usual path is the gVisor `RuntimeClass` + a node installation of
`containerd-shim-runsc-v1` (see the [gVisor Kubernetes
docs](https://gvisor.dev/docs/user_guide/quick_start/kubernetes/)):

```yaml
# A RuntimeClass whose handler matches the containerd runtime you configured.
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
```

```bash
kubectl get runtimeclass
# NAME      HANDLER
# gvisor    runsc
```

Requirements: nodes run the `runsc` shim; **no** `/dev/kvm` or nested virtualization
is required (the default ptrace platform needs none — that is the whole reason to
pick gVisor over kata). If a node also has `/dev/kvm`, gVisor can use its faster KVM
platform, but it is optional.

> The forge pod itself needs no privilege — the kubelet on the *worker* node runs
> the sandbox. Forge only sets `runtimeClassName` on the Job it already creates.

## 2. Register the backend

Backends are admin-managed (`createRuntimeBackend`). Regular users get read-only
`list`/`get`. The single required config key is `runtime_class`, the name of a
RuntimeClass from step 1:

```bash
curl -X POST "$CONDUCTOR/forge/runtime-backends" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "gvisor-prod",
    "type": "gvisor",
    "enabled": true,
    "config": { "runtime_class": "gvisor" }
  }'
```

### Backend config keys

| Key | Required | Description |
|---|---|---|
| `runtime_class` | yes | Kubernetes RuntimeClass whose handler is `runsc` (e.g. `gvisor`). Must already exist in the cluster. |

`runtime_class` is **required** on purpose: a gvisor backend with no RuntimeClass
would silently fall back to the cluster's default runtime (runc) and run with no
gVisor sandbox while looking healthy, so Forge rejects it at create/update time.

> The plain `kubernetes` backend type also accepts an optional `runtime_class`
> config key (falling back to the `K8S_RUNTIME_CLASS` env var, then the cluster
> default). `gvisor` is the same mechanism with the key made mandatory and the intent
> made explicit.

Namespace, sandbox UID, and **egress isolation come from the same env/NetworkPolicy
configuration as the `kubernetes` backend** (`K8S_NAMESPACE`, `FORGE_SANDBOX_UID`,
`FORGE_EGRESS_PROXY`). Unlike kata, gVisor is a kernel boundary, not a network one,
so the egress proxy and its NetworkPolicy are **kept** — they are how a gVisor job's
egress is confined to the allowlist.

## 3. Point a runner class at it

The class couples a backend with backend-shaped resources. gvisor reads the same
fields as the kubernetes backend: `memory_mb`, `cpu_millicores`, `tmpfs_mb`.
`disk_gb` is not used (the rootfs comes from the image).

```bash
curl -X POST "$CONDUCTOR/forge/runner-classes" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "gvisor-standard",
    "memory_mb": 1024,
    "cpu_millicores": 1000,
    "pids_limit": 128,
    "tmpfs_mb": 64,
    "backend": "gvisor-prod",
    "enabled": true
  }'
```

Users then submit against it as usual — `runner_class: "gvisor-standard"`. The
backend is snapshotted onto the execution at submit, so re-pointing the class
afterward never moves an already-queued job.

### Privileged jobs (root / package managers)

By default gvisor jobs run as the locked-down non-root sandbox (read-only rootfs, all
capabilities dropped, no privilege escalation) — so `apt`/`pacman`/`dnf` fail with
permission errors, exactly as on the `kubernetes` backend. Set `privileged: true` on
the runner class to run the job as **root with a writable root filesystem** and
privilege escalation allowed, so package managers and other root operations work:

```bash
curl -X PUT "$CONDUCTOR/forge/runner-classes/gvisor-standard" \
  -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' \
  -d '{ "name": "gvisor-standard", "memory_mb": 2048, "cpu_millicores": 1000,
        "pids_limit": 128, "tmpfs_mb": 256, "backend": "gvisor-prod",
        "enabled": true, "privileged": true }'
```

This is safe because the **gVisor Sentry**, not the container, is the isolation
boundary — the job's "root" makes syscalls to the userspace kernel, never to the host
kernel. Two guardrails:

- **Kernel-isolated backends only.** `privileged` is honoured solely on
  kernel-isolated backends — kata and gvisor. The API rejects it for any
  shared-kernel backend (`docker`/`kubernetes`), and forge drops it at runtime if it
  ever reaches one (`buildJob`): root + writable rootfs in a shared-kernel (runc)
  container would be a host-kernel escape risk.
- **PodSecurity.** A privileged pod runs as root, so the `forge` namespace must not
  enforce the PodSecurity `restricted` profile — use `baseline` or `privileged`:
  `kubectl label ns forge pod-security.kubernetes.io/enforce=baseline --overwrite`.
  The pod still stays within `baseline`: it does not set container `privileged`, host
  namespaces, or host paths.

It does **not** provide an in-VM Docker daemon; to build container images use forge's
built-in daemonless image build (Kaniko on gvisor), which needs no daemon or host
socket. The egress proxy / NetworkPolicy isolation applies to privileged jobs too.

## Helm

Set `forge.gvisor.enabled=true` and point `forge.env.k8sRuntimeClass` at the gVisor
RuntimeClass. The chart then runs forge with `RUNTIME=gvisor`, **keeps** the egress
proxy + NetworkPolicy (unlike `forge.kata.enabled`, which skips them), and seeds the
default runner classes privileged. `forge.gvisor.enabled` and `forge.kata.enabled`
are mutually exclusive.

```bash
helm upgrade --install codearmory infra/helm/codearmory \
  --set forge.gvisor.enabled=true \
  --set forge.env.k8sRuntimeClass=gvisor
```

## Notes & limits

- **Image and command** work exactly as for the kubernetes backend — the submitted
  `image` is the container image run under the Sentry, subject to the usual
  `ALLOWED_IMAGES` allowlist.
- **Syscall compatibility.** gVisor implements most of the Linux syscall surface but
  not all of it; an unusual workload may hit an unimplemented syscall. For typical
  build/test/deploy commands this is rarely an issue.
- **Egress is NOT confined by gVisor.** gVisor sandboxes the *kernel*, not the
  *network* — a gvisor job reaches the network through the pod's normal namespace, so
  the egress proxy + NetworkPolicy (kept by default) are what enforce the domain
  allowlist. Do not disable them for a gvisor backend.
- **Node support is operator responsibility.** If a node lacks the `runsc` shim, pods
  scheduled there fail to start; that surfaces as a per-job failure, not a worker
  crash. Constrain scheduling (taints/affinity) to gVisor-capable nodes as needed.
- **Backend resolution failures are per-job**, same as every non-default backend: a
  bad `runtime_class` fails only its own executions, not the worker.
