# Forge — Kata Containers VM-isolated runtime

The docker and kubernetes runtimes isolate jobs at the *container* layer: a
shared host kernel with read-only rootfs, dropped capabilities, a non-root UID,
seccomp, and an allowlisted egress proxy. That is a strong boundary, but it is
still the host kernel — a kernel-level escape (a syscall or namespace bug) breaks
out of every container on the node.

The **kata** runtime moves the boundary to a *lightweight VM* without changing how
jobs are submitted or scheduled. It is the existing Kubernetes runtime with the
pod's `runtimeClassName` pinned to a [Kata Containers](https://katacontainers.io)
RuntimeClass. The kubelet hands the pod to the Kata runtime, which boots a minimal
microVM (real guest kernel, hardware-virtualization boundary via QEMU, Firecracker,
or Cloud Hypervisor) and runs the container inside it. To Forge it is still a normal
Job: the same `RunResult` contract, timeout, cancel, log collection, and TTL cleanup
apply unchanged, and every container hardening setting (non-root, read-only rootfs,
`drop ALL` capabilities, no service-account token, seccomp) is still set on top of
the VM boundary.

Compared to the **proxmox** backend, kata needs no template, no guest agent, and no
PVE API token — Kata builds the guest from the submitted image. By default the job
runs as the same locked-down non-root container it would under plain Kubernetes, but
because the microVM (not the container) is the security boundary, a runner class may
set `privileged: true` to run the job as **root with a writable rootfs** so package
managers (`apt`/`pacman`/`dnf`) work — see *Privileged jobs* below. Use **kata** for
strongly-isolated workloads that need either a locked-down sandbox or root + package
installs; use **proxmox** for CI that needs an in-VM **Docker daemon** / `docker build`.

```
POST /executions (runner_class → backend snapshot)
   │
   ▼  worker resolves the execution's backend
KubernetesRuntime.Run  (runtimeClassName = <kata RuntimeClass>)
   │  1. create Job in K8S_NAMESPACE (BackoffLimit 0, ActiveDeadlineSeconds)
   │  2. kubelet → Kata runtime → boot microVM, run the runner container inside
   │  3. poll job status; collect the runner container's logs (1 MB cap)
   │  4. delete the Job (TTLSecondsAfterFinished is the crash safety net)
   ▼
RunResult {stdout, stderr, exit_code}   ← identical contract to docker/k8s
```

## 1. Install Kata in the cluster (prerequisite)

Kata is a *cluster* component, installed by the operator — Forge does not create
the `RuntimeClass`. The usual path is `kata-deploy`, now published as a Helm chart
(an OCI artifact) that installs the Kata binaries on each node *and* registers the
RuntimeClass objects:

```bash
export VERSION=$(curl -sSL https://api.github.com/repos/kata-containers/kata-containers/releases/latest | jq -r .tag_name)
export CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
helm install kata-deploy "$CHART" --version "$VERSION"   # pin a fixed version for reproducibility

# verify the RuntimeClass(es) exist:
kubectl get runtimeclass
# NAME         HANDLER
# kata-qemu    kata-qemu
# kata-fc      kata-fc      (Firecracker — smallest attack surface)
# kata-clh     kata-clh     (Cloud Hypervisor)
```

All shims are enabled by default; to install only some, check the keys with
`helm show values "$CHART" --version "$VERSION"` and pass the shim list via
`--set` (e.g. `--set env.shims="qemu fc" --set env.defaultShim="qemu"`).

Requirements: nodes must support hardware virtualization (`/dev/kvm`) — bare metal
or nested-virt-enabled VMs. Many managed node pools do not have nested virt; check
before relying on this backend. Pick the `RuntimeClass` whose VMM matches your
isolation/feature trade-off (`kata-fc` for the smallest surface, `kata-qemu` for
the broadest device support).

> The forge pod itself does **not** need `/dev/kvm` or any privilege — the kubelet
> on the *worker* node runs the microVM. Forge only sets `runtimeClassName` on the
> Job it already creates.

## 2. Register the backend

Backends are admin-managed (`createRuntimeBackend`). Regular users get read-only
`list`/`get`. The single required config key is `runtime_class`, the name of a
RuntimeClass from step 1:

```bash
curl -X POST "$CONDUCTOR/forge/runtime-backends" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "kata-prod",
    "type": "kata",
    "enabled": true,
    "config": { "runtime_class": "kata-fc" }
  }'
```

### Backend config keys

| Key | Required | Description |
|---|---|---|
| `runtime_class` | yes | Kubernetes RuntimeClass to run jobs under (e.g. `kata-qemu`, `kata-fc`, `kata-clh`). Must already exist in the cluster. |

`runtime_class` is **required** on purpose: a kata backend with no RuntimeClass
would silently fall back to the cluster's default runtime (runc) and run with no VM
isolation while looking healthy, so Forge rejects it at create/update time.

> The plain `kubernetes` backend type also accepts an optional `runtime_class`
> config key (falling back to the `K8S_RUNTIME_CLASS` env var, then the cluster
> default). `kata` is the same mechanism with the key made mandatory and the intent
> made explicit.

Namespace, sandbox UID, and egress isolation come from the same env/NetworkPolicy
configuration as the `kubernetes` backend (`K8S_NAMESPACE`, `FORGE_SANDBOX_UID`).

## 3. Point a runner class at it

The class couples a backend with backend-shaped resources. kata reads the same
fields as the kubernetes backend: `memory_mb`, `cpu_millicores`, `tmpfs_mb`. CPU and
memory limits also size the microVM. `disk_gb` is not used (the rootfs comes from the
image).

```bash
curl -X POST "$CONDUCTOR/forge/runner-classes" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "kata-standard",
    "memory_mb": 1024,
    "cpu_millicores": 1000,
    "pids_limit": 128,
    "tmpfs_mb": 64,
    "backend": "kata-prod",
    "enabled": true
  }'
```

Users then submit against it as usual — `runner_class: "kata-standard"`. The backend
is snapshotted onto the execution at submit, so re-pointing the class afterward never
moves an already-queued job.

### Privileged jobs (root / package managers)

By default kata jobs run as the locked-down non-root sandbox (read-only rootfs, all
capabilities dropped, no privilege escalation) — so `apt`/`pacman`/`dnf` fail with
permission errors, exactly as on the `kubernetes` backend. Set `privileged: true` on
the runner class to run the job as **root with a writable root filesystem** and
privilege escalation allowed, so package managers and other root operations work:

```bash
curl -X PUT "$CONDUCTOR/forge/runner-classes/kata-standard" \
  -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' \
  -d '{ "name": "kata-standard", "memory_mb": 2048, "cpu_millicores": 1000,
        "pids_limit": 128, "tmpfs_mb": 256, "backend": "kata-prod",
        "enabled": true, "privileged": true }'
```

This is safe because the **microVM**, not the container, is the isolation boundary —
the same model the proxmox backend uses to run jobs as root in a disposable VM. Two
guardrails:

- **VM-isolated backends only.** `privileged` is honoured solely on VM-isolated
  backends — kata and proxmox. The API rejects it for any shared-kernel backend
  (`docker`/`kubernetes`), and forge drops it at runtime if it ever reaches one
  (`buildJob`): root + writable rootfs in a shared-kernel (runc) container would be a
  host-kernel escape risk.
- **PodSecurity.** A privileged pod runs as root, so the `forge` namespace must not
  enforce the PodSecurity `restricted` profile — use `baseline` or `privileged`:
  `kubectl label ns forge pod-security.kubernetes.io/enforce=baseline --overwrite`.
  The pod still stays within `baseline`: it does not set container `privileged`, host
  namespaces, or host paths.

It does **not** provide an in-VM Docker daemon — for `docker build` use the proxmox
backend. The egress proxy / NetworkPolicy isolation applies to privileged jobs too.

## Notes & limits

- **Image and command** work exactly as for the kubernetes backend — the submitted
  `image` is the container image run inside the microVM, subject to the usual
  `ALLOWED_IMAGES` allowlist.
- **Boot latency.** Each job boots a microVM (typically tens to a few hundred ms,
  VMM-dependent) on top of normal pod scheduling — slower to start than a plain
  container, far faster than a full Proxmox clone.
- **Node support is operator responsibility.** If a node lacks `/dev/kvm` or the
  Kata binaries, pods scheduled there fail to start; that surfaces as a per-job
  failure, not a worker crash. Constrain scheduling (taints/affinity) to Kata-capable
  nodes as needed.
- **Firecracker (`kata-fc`) has no filesystem sharing.** Unlike `kata-qemu`/`kata-clh`,
  Firecracker supports no virtio-fs/9p, so a host-shared (node-backed) volume cannot be
  mounted into the microVM. Forge sizes the job's writable `/tmp` (`tmpfs_mb`) as a
  *memory-backed* emptyDir for exactly this reason, so it boots on `kata-fc` as well as
  every other VMM. The cost is that `/tmp` usage counts against the runner class's
  `memory_mb` (same as the docker runtime's tmpfs) — size the two together.
- **Backend resolution failures are per-job**, same as every non-default backend: a
  bad `runtime_class` fails only its own executions, not the worker.
