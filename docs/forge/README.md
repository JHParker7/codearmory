# Forge

Sandboxed code execution service. Accepts execution requests from authenticated users, runs them in isolated containers (Docker or Kubernetes), and records stdout, stderr, and exit code.

## How it works

```
Client (Bearer JWT)
  │
  └── POST /executions ─────────────────────────► Forge :8083
        │  1. Verify token via Gatekeeper /check_permissions
        │  2. Validate image against ALLOWED_IMAGES allowlist
        │  3. Reject any blocked env keys (LD_PRELOAD, PYTHONPATH, etc.)
        │  4. Resolve runner class (resource limits) from database
        │  5. Enqueue execution record in PostgreSQL
        │
        ▼
      Worker pool (10 goroutines)
        │  1. Dequeue pending execution
        │  2. Run container via selected runtime (docker or kubernetes)
        │  3. Capture stdout / stderr (capped at 1 MB each)
        │  4. Write result back to PostgreSQL
```

Forge calls Gatekeeper directly to verify the Bearer token on every request. It does not trust the `X-User-ID` header injected by Conductor, which prevents privilege escalation via a compromised gateway.

## Runtime backends

The runtime that runs a job is selected per-execution. Admins define **runtime backends** (`/runtime-backends`, admin-only CRUD; users get read-only list/get) of type `docker`, `kubernetes`, `proxmox`, or `kata`, and point a runner class at one via its `backend` field. The execution snapshots the class's backend at submit time. A `default` backend is seeded from the legacy `RUNTIME` env, so a single-runtime deployment needs no change.

- **docker / kubernetes** — container-level sandbox (read-only rootfs, dropped caps, egress proxy). Best for untrusted code.
- **kata** — the kubernetes runtime pinned to a Kata Containers `RuntimeClass`, so each job runs in a lightweight VM (a real kernel, hardware-virtualization boundary) while keeping the same Job lifecycle and container hardening. Stronger isolation than a plain container with no new runtime to operate. See [kata.md](kata.md).
- **proxmox** — a throwaway VM per job with full root and a real Docker daemon, for CI work that needs `apt`/`docker build`. See [proxmox.md](proxmox.md).

## Requirements

- Go 1.25+
- PostgreSQL
- Docker daemon (for `RUNTIME=docker`) **or** a Kubernetes cluster (for `RUNTIME=kubernetes`)

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/forge` | PostgreSQL connection string |
| `RUNTIME` | `kubernetes` | Container runtime. Set to `docker` or `kubernetes`. |
| `ALLOWED_IMAGES` | — | **Required.** Comma-separated list of permitted container images. Forge denies all submissions when unset. |
| `GATEKEEPER_URL` | `http://localhost:8080` | URL of the Gatekeeper service used for permission checks and service key rotation. |
| `GATEKEEPER_SERVICE_KEY` | — | Shared service key registered with Gatekeeper. Required for service-to-service authentication in production. |
| `PORT` | `8083` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `forge` | Service name reported in traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `DATABASE_URL_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

### TLS variables

| Variable | Default | Description |
|---|---|---|
| `TLS_CERT_FILE` | — | Path to TLS certificate for the server. When set alongside `TLS_KEY_FILE`, the server listens with TLS. |
| `TLS_KEY_FILE` | — | Path to TLS private key for the server. |
| `TLS_CLIENT_AUTH` | — | Set to `require` to enforce mutual TLS on incoming connections, or `request` to request but not require a client certificate. `TLS_CLIENT_CA_FILE` must be set when using `require`. |
| `TLS_CLIENT_CA_FILE` | — | CA certificate used to verify client certificates when `TLS_CLIENT_AUTH=require`. |
| `TLS_CLIENT_CERT_FILE` | — | Client certificate presented on outbound TLS connections (e.g. to Gatekeeper). |
| `TLS_CLIENT_KEY_FILE` | — | Private key for the outbound client certificate. |
| `TLS_CA_FILE` | — | CA bundle used to verify outbound TLS connections. |

### Docker runtime variables

| Variable | Default | Description |
|---|---|---|
| `FORGE_NETWORK_MODE` | `none` | Docker network mode for execution containers. Defaults to `none` (no network access). Set to the name of a custom bridge network to enable controlled outbound access. `host` and `bridge` are explicitly rejected. |
| `FORGE_EGRESS_PROXY` | — | HTTP proxy URL injected as `HTTP_PROXY`/`HTTPS_PROXY` into every execution container. When set, forge injects the proxy vars automatically so tools like `tofu`, `npm`, and `curl` route through it without per-execution configuration. |

Resource limits (memory, CPU, pids, tmpfs) are controlled per execution by runner classes — see [Runner classes](#runner-classes).

### Egress proxy (controlled outbound access)

By default, execution containers have no network access (`FORGE_NETWORK_MODE=none`). For CI/CD workloads that need to download modules or packages, use the `egress-proxy` sidecar from `src/systems/egress-proxy` instead of opening broad internet access.

The recommended setup:

1. Create a Docker bridge network with `internal: true` (no direct internet routing):
   ```bash
   docker network create --internal forge-exec
   ```
2. Run `egress-proxy` on both `forge-exec` and a network that has internet access. Configure `PROXY_ALLOWED_DOMAINS` with a comma-separated list of permitted hostnames. Wildcards (`*.github.com`) are supported.
3. Set `FORGE_NETWORK_MODE=forge-exec` and `FORGE_EGRESS_PROXY=http://<proxy-host>:3128` on forge.

Execution containers are then isolated to the `forge-exec` network (no direct internet) but can reach the allowlisted domains through the proxy. The compose file at `infra/local/compose.yml` ships a ready-to-use configuration.

**Default allowed domains** (overridable via `FORGE_PROXY_ALLOWED_DOMAINS` in compose):

| Domain | Purpose |
|--------|---------|
| `registry.terraform.io` | OpenTofu/Terraform module registry |
| `releases.hashicorp.com` | Provider binary downloads |
| `github.com`, `*.github.com` | GitHub |
| `raw.githubusercontent.com`, `objects.githubusercontent.com` | GitHub raw content |
| `registry.npmjs.org` | npm packages |
| `pypi.org`, `files.pythonhosted.org` | Python packages |
| `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com` | Go modules |

### Kubernetes runtime variables

| Variable | Default | Description |
|---|---|---|
| `K8S_NAMESPACE` | `forge` | Namespace to create Jobs in |
| `K8S_RUNTIME_CLASS` | — | RuntimeClass name (e.g. `gvisor` for stronger isolation) |
| `KUBECONFIG` | `~/.kube/config` | Kubeconfig path (falls back to in-cluster credentials) |

Resource limits are controlled per execution by runner classes — see [Runner classes](#runner-classes).

## Runner classes

Runner classes define the resource limits applied to execution containers. They are stored in PostgreSQL and can be updated at runtime via the API without restarting Forge.

### Default runner classes

| Name | Memory | CPU | Pids | Tmpfs |
|------|--------|-----|------|-------|
| `standard` | 256 MB | 500m (0.5 core) | 64 | 64 MB |
| `large` | 2048 MB | 2000m (2 cores) | 256 | 512 MB |
| `xlarge` | 8192 MB | 4000m (4 cores) | 512 | 2048 MB |

These three classes are seeded automatically on startup if absent. Operators can edit them or add custom classes via the API. Changes take effect immediately for new submissions — no restart required.

### Selecting a runner class

Set `runner_class` in the submit request body. Defaults to `standard` when omitted. Submitting with a disabled or nonexistent class returns `400 Bad Request`.

### Managing runner classes

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/runner-classes` | `listRunnerClass` on `forge/runner-classes` | List all runner classes |
| `POST` | `/runner-classes` | `createRunnerClass` on `forge/runner-classes` | Create a new runner class |
| `GET` | `/runner-classes/{name}` | `getRunnerClass` on `forge/runner-classes/{name}` | Get a single runner class |
| `PUT` | `/runner-classes/{name}` | `updateRunnerClass` on `forge/runner-classes/{name}` | Update a runner class |
| `DELETE` | `/runner-classes/{name}` | `deleteRunnerClass` on `forge/runner-classes/{name}` | Delete a runner class |

By default, all authenticated users can list and get runner classes. Creating, updating, and deleting requires an operator-granted permission.

## Running locally

```bash
cd src/systems/forge
DATABASE_URL=postgresql://postgres:pass@localhost:5432/forge \
  RUNTIME=docker \
  ALLOWED_IMAGES=alpine:3.19,ubuntu:22.04,python:3.12-slim \
  GATEKEEPER_URL=http://localhost:8081 \
  go run .
```

The Docker socket must be accessible at `/var/run/docker.sock`.

## Docker

```bash
cd src/systems/forge
docker build -t forge:latest .

docker run -p 8083:8083 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e DATABASE_URL=postgresql://postgres:pass@db:5432/forge \
  -e RUNTIME=docker \
  -e ALLOWED_IMAGES=alpine:3.19,ubuntu:22.04,python:3.12-slim \
  -e GATEKEEPER_URL=http://gatekeeper:8081 \
  -e GATEKEEPER_SERVICE_KEY=your-service-key \
  forge:latest
```

## API

All endpoints require a Gatekeeper-issued Bearer token (`Authorization: Bearer <token>`). Forge verifies permissions directly with Gatekeeper on every request.

### Executions

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/executions` | `createExecution` on `forge/executions` | Submit a new execution |
| `GET` | `/executions` | `listExecution` on `forge/executions` | List the caller's executions |
| `GET` | `/executions/{id}` | `getExecution` on `forge/executions/{id}` | Get a single execution |
| `DELETE` | `/executions/{id}` | `deleteExecution` on `forge/executions/{id}` | Cancel a pending or running execution |

### Submit an execution

```bash
curl -X POST http://conductor:8080/executions \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "image": "python:3.12-slim",
    "command": ["python", "-c", "print(\"hello\")"],
    "env": {"MY_VAR": "value"},
    "timeout": 30,
    "runner_class": "standard"
  }'
# → {"execution_id": "uuid"}
```

### Request body

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `image` | string | Yes | Container image. Must be in `ALLOWED_IMAGES`. |
| `command` | `[]string` | Yes | Command and arguments passed directly to the container (not a shell). |
| `env` | `map[string]string` | No | Additional environment variables. Keys must be valid POSIX names. Dangerous interpreter keys (`LD_PRELOAD`, `PYTHONPATH`, `NODE_OPTIONS`, etc.) are rejected. |
| `timeout` | int | No | Execution timeout in seconds. Defaults to 30, capped at 3600. |
| `runner_class` | string | No | Resource tier to use. Defaults to `standard`. Must be an enabled runner class. |

### Execution object

```json
{
  "execution_id": "uuid",
  "user_id": "uuid",
  "image": "python:3.12-slim",
  "runner_class": "standard",
  "status": "completed",
  "exit_code": 0,
  "stdout": "hello\n",
  "stderr": "",
  "created_at": "2026-05-27T12:00:00Z",
  "started_at": "2026-05-27T12:00:01Z",
  "ended_at": "2026-05-27T12:00:02Z"
}
```

**Status values:** `pending` → `running` → `completed` | `failed` | `timed_out` | `cancelled`

### Cancel an execution

`DELETE /executions/{id}` cancels a `pending` execution immediately. A `running` execution is stopped via the container runtime. Returns `204 No Content` on success or `409 Conflict` if the execution has already finished.

## Security

### Container sandbox (Docker runtime)

Every container runs with:
- `NetworkMode` — controlled by `FORGE_NETWORK_MODE` (default `none`, no network access); `host` and `bridge` are rejected at startup
- `ReadonlyRootfs: true` — read-only root filesystem
- `/tmp` — writable tmpfs (size from runner class)
- `CapDrop: ALL` — all Linux capabilities dropped
- `no-new-privileges` security option
- Memory, CPU, and PID limits from the selected runner class

### Container sandbox (Kubernetes runtime)

Every Job runs with:
- `AutomountServiceAccountToken: false` — no cluster credentials
- `RunAsNonRoot: true`
- `AllowPrivilegeEscalation: false`
- `ReadOnlyRootFilesystem: true`
- All capabilities dropped
- Seccomp profile: `RuntimeDefault`
- `/tmp` EmptyDir (size from runner class)
- `BackoffLimit: 0` — failures are not retried
- Memory and CPU limits from the selected runner class

### Image allowlist

Forge denies all submissions when `ALLOWED_IMAGES` is not configured. In production, set this to a minimal list of vetted images. Images are pulled automatically if not already present on the host.

### Environment variable denylist

The following env keys are always rejected regardless of case:

`LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`, `PYTHONSTARTUP`, `PYTHONPATH`, `NODE_OPTIONS`, `NODE_PATH`, `RUBYOPT`, `RUBYLIB`, `PERL5LIB`, `PERLLIB`, `JAVA_TOOL_OPTIONS`, `JAVA_OPTIONS`, `_JAVA_OPTIONS`, `DYLD_INSERT_LIBRARIES`, `DYLD_LIBRARY_PATH`

### Request authentication

Forge calls `POST /check_permissions` on Gatekeeper with the caller's Bearer token to verify authorization on every request. This happens even when the request arrives through Conductor, so a compromised Conductor cannot escalate privileges by forging the `X-User-ID` header.

## Metrics

| Metric | Description |
|--------|-------------|
| `forge.executions.submitted.total` | Executions submitted, labelled by `image` |
| `forge.executions.completed.total` | Executions that reached a terminal state |
| `forge.executions.cancelled.total` | Executions cancelled by the user |

## Testing

```bash
# Unit tests
cd src/systems/forge
go test ./...

# Integration tests (requires running Forge, Conductor, and Gatekeeper)
pip install -r tests/forge/requirements.txt
FORGE_URL=http://localhost:8083 GATEKEEPER_URL=http://localhost:8080 \
  pytest tests/forge/ -v
```
