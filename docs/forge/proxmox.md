# Forge — Proxmox VM-per-job runtime

The docker and kubernetes runtimes isolate jobs at the *container* layer:
read-only rootfs, all capabilities dropped, no Docker socket, an allowlisted
egress proxy. That is the right boundary for untrusted code, but it makes ordinary
CI work impossible — you cannot `apt install`, reach a Docker daemon, or
`docker build`.

The **proxmox** runtime moves the isolation boundary to the *VM* layer instead.
For each job Forge clones a throwaway Proxmox VM from a prepared template, sizes it
to the runner class, boots it, runs the command inside as full root via the
qemu-guest-agent, captures stdout/stderr, and then destroys the VM. Inside the VM
the job has a real kernel, real root, and a real Docker daemon — `apt-get update`
and `docker build` just work — while the blast radius stays inside a disposable
VM rather than your control-plane cluster.

It is one of three pluggable runtime *backends*. Admins define backends; users
pick one indirectly by choosing a runner class that points at it (`backend` field
on the runner class). An existing `RUNTIME`-only deployment is unchanged: a
`default` backend is seeded from the legacy `RUNTIME` env and every existing class
points at it.

```
POST /executions (runner_class → backend snapshot)
   │
   ▼  worker resolves the execution's backend
ProxmoxRuntime.Run
   │  1. POST /cluster/nextid                       → free VMID
   │  2. POST .../qemu/{template}/clone  (full=0)    → linked clone "forge-<execID>"
   │  3. POST .../qemu/{id}/config   cores/memory/net0
   │  4. PUT  .../qemu/{id}/resize   scsi0 = <disk_gb>G
   │  5. POST .../qemu/{id}/status/start
   │  6. POST .../qemu/{id}/agent/ping  (until ready, ~up to 90s)
   │  7. POST .../qemu/{id}/agent/exec  (sh -c: export env; exec command)
   │  8. GET  .../qemu/{id}/agent/exec-status?pid=…  (base64 out/err, 1 MB cap)
   │  9. POST .../qemu/{id}/status/stop  +  DELETE .../qemu/{id}?purge=1   (always)
   ▼
RunResult {stdout, stderr, exit_code}   ← identical contract to docker/k8s
```

Forge drives the whole lifecycle synchronously and returns a `RunResult`, so the
same result classification, timeout, and cancel handling used by docker/k8s apply
unchanged. There is no dial-home agent and no new inbound Forge endpoint. On
startup the runtime sweeps and destroys any leftover `forge-*` VMs orphaned by a
crash (Proxmox has no TTL equivalent to a k8s job's `TTLSecondsAfterFinished`).

## 1. Build the job template

On the Proxmox host, build a VM template the clones derive from. A cloud image
plus a small amount of setup:

1. Create a VM from a cloud image (e.g. Ubuntu/Debian cloud image), with a
   `scsi0` disk on the storage you will reference as `storage`.
2. Inside the VM, install what jobs need:
   - `qemu-guest-agent` (**required** — this is how Forge runs the command and
     reads its output) and enable it: `systemctl enable --now qemu-guest-agent`.
   - Docker, if jobs will `docker build` / `docker run`.
   - Any base toolchain you want pre-baked so jobs start fast.
3. Confirm the guest agent is enabled on the VM hardware (Options → QEMU Guest
   Agent → Enabled), and that `agent: 1` is set on the VM config.
4. Attach the VM's NIC to a **locked-down bridge/VLAN** whose only reachable
   egress is the egress proxy (see *Egress isolation* below).
5. Convert the VM to a template (right-click → Convert to template). Note its
   VMID — this is `template_vmid`.

### Egress isolation

The cloned job VM gets full root, so its network must be constrained the same way
the Docker runtime's `forge-exec` network is: it should reach **only** the egress
proxy, which enforces the `PROXY_ALLOWED_DOMAINS` allowlist. This is operator
responsibility — configure the template's bridge/VLAN (e.g. a dedicated VLAN with
firewall rules permitting only the proxy address) and set the backend's
`egress_proxy` config to the proxy URL. Forge injects `HTTP(S)_PROXY` (and their
lowercase forms, plus `NO_PROXY=localhost,127.0.0.1`) into every job, but does not
itself enforce the network boundary — the bridge does.

## 2. Create a Proxmox API token

Create a dedicated API token for Forge (Datacenter → Permissions → API Tokens),
e.g. `forge@pve!ci`. Grant it a role with at least:

| Privilege | Why |
|---|---|
| `VM.Allocate` | clone into a new VMID |
| `VM.Clone` | clone the template |
| `VM.Config.*` (Disk, CPUSet, Memory, Network, Options) | size cores/memory/net, resize disk |
| `VM.PowerMgmt` | start/stop the VM |
| `VM.Monitor` | poll status |
| `VM.Audit` | read VM list (orphan sweep / cancel) |
| `VM.GuestAgent.Audit` | guest-agent ping / exec / exec-status |
| `Datastore.AllocateSpace` | clone/resize disk allocation |

The token value Forge needs is the full string `USER@REALM!TOKENID=SECRET`, e.g.
`forge@pve!ci=00000000-0000-0000-0000-000000000000`.

Provide it to Forge as the `PROXMOX_TOKEN` env/secret (or any name you reference
from the backend's `secret_refs`). In Helm:

```yaml
forge:
  proxmox:
    enabled: true            # mounts the proxmox-token secret into the pod
  secret:
    proxmoxToken: "forge@pve!ci=00000000-0000-0000-0000-000000000000"
```

`secret("PROXMOX_TOKEN")` follows the standard `_FILE` convention, so a
volume-mounted `PROXMOX_TOKEN_FILE` works too. The token value never lives in the
Forge database — the backend stores only the *name* of the env var that holds it.

## 3. Register the backend

Backends are admin-managed (`createRuntimeBackend`). Regular users get read-only
`list`/`get` so a client can show which backend a class targets.

```bash
curl -X POST "$CONDUCTOR/forge/runtime-backends" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "proxmox-prod",
    "type": "proxmox",
    "enabled": true,
    "config": {
      "url": "https://pve.internal:8006/api2/json",
      "node": "pve",
      "template_vmid": "9000",
      "storage": "local-lvm",
      "bridge": "vmbr1",
      "vlan": "",
      "clone_mode": "linked",
      "egress_proxy": "http://egress-proxy:3128",
      "tls_insecure": "false"
    },
    "secret_refs": { "token": "PROXMOX_TOKEN" }
  }'
```

### Backend config keys

| Key | Required | Description |
|---|---|---|
| `url` | yes | PVE API base, e.g. `https://host:8006/api2/json` |
| `node` | yes | Proxmox node name that hosts the template and runs the clones |
| `template_vmid` | yes | VMID of the prepared template (integer) |
| `storage` | yes | Storage for the clone's disk |
| `bridge` | yes | Network bridge for the job VM's NIC |
| `boot_disk` | no | Disk id resized to the class's `disk_gb` (default `scsi0`; set to `virtio0`/`sata0`/etc. to match your template) |
| `vlan` | no | VLAN tag for the NIC (`net0` `tag=`) |
| `clone_mode` | no | `linked` (default, fast) or `full` (standalone copy, stronger isolation) |
| `egress_proxy` | no | HTTP proxy URL injected as `HTTP(S)_PROXY` into the job |
| `tls_insecure` | no | `true` to skip PVE cert verification (homelab/self-signed) |
| `ca_file` | no | Path to a PEM CA bundle for the PVE cert |

`secret_refs.token` is **required** and names the env var holding the API token.

## 4. Point a runner class at it

The class couples a backend with backend-shaped resources, so admins curate valid
combinations. For proxmox: `memory_mb` → VM RAM, `cpu_millicores` → `ceil(/1000)`
vCPU, `disk_gb` → VM disk size (0 = leave the template disk as-is). `tmpfs_mb` /
`pids_limit` are ignored.

```bash
curl -X POST "$CONDUCTOR/forge/runner-classes" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "vm-large",
    "memory_mb": 8192,
    "cpu_millicores": 4000,
    "pids_limit": 64,
    "tmpfs_mb": 64,
    "disk_gb": 40,
    "backend": "proxmox-prod",
    "enabled": true
  }'
```

Users then submit against it as usual — `runner_class: "vm-large"`. The backend is
snapshotted onto the execution at submit, so re-pointing the class afterward never
moves an already-queued job.

## Notes & limits

- **Backend resolution failures are per-job.** A misconfigured non-default backend
  (bad token, unreachable host, missing template) fails only its own executions,
  with the reason in the execution's stderr — it does not crash the worker. Only
  the `default` backend is validated eagerly at startup.
- **Image field.** For proxmox the command runs directly in the VM (the template
  provides the environment), so the submitted `image` is not used to launch a
  container — it remains subject to the submit-time allowlist but is otherwise
  vestigial for this backend. Run containers explicitly with `docker run` if you
  want one.
- **Boot latency.** v1 clones on demand (~15–40s including guest-agent readiness).
  A warm VM pool is future work.
- **Single writer per node.** The orphan sweep reaps every `forge-*` VM on the
  node at startup; run a single Forge writer per Proxmox node in v1.
- **Output cap.** stdout/stderr are captured synchronously and each capped at
  1 MB, matching the docker/k8s runtimes. No live streaming.
```
