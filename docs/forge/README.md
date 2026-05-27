# Forge

Sandboxed code execution service. Accepts execution requests from authenticated users, runs them in isolated containers (Docker or Kubernetes), and records stdout, stderr, and exit code.

## How it works

```
Conductor :8082
  │
  └── POST /executions ─────────────────────────► Forge :8083
        │  1. Verify X-Conductor-Token HMAC (when CONDUCTOR_FORWARD_KEY is set)
        │  2. Validate image against ALLOWED_IMAGES allowlist
        │  3. Reject any blocked env keys (LD_PRELOAD, PYTHONPATH, etc.)
        │  4. Enqueue execution record in PostgreSQL
        │
        ▼
      Worker pool (10 goroutines)
        │  1. Dequeue pending execution
        │  2. Run container via selected runtime (docker or kubernetes)
        │  3. Capture stdout / stderr (capped at 1 MB each)
        │  4. Write result back to PostgreSQL
```

Forge never verifies JWTs directly. The authenticated user identity arrives via the `X-User-ID` header, signed by Conductor with HMAC-SHA256 when `CONDUCTOR_FORWARD_KEY` is configured.

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
| `CONDUCTOR_FORWARD_KEY` | — | Shared secret used to verify `X-Conductor-Token` HMAC on incoming requests. Should match the value configured on Conductor. Set this in all production deployments. |
| `PORT` | `8083` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `forge` | Service name reported in traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `DATABASE_URL_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

### Docker runtime variables

| Variable | Default | Description |
|---|---|---|
| `CONTAINER_MEMORY_LIMIT` | `256m` | Memory limit per container |
| `CONTAINER_CPU_QUOTA` | `50000` | CPU quota (100000 = one full core) |

### Kubernetes runtime variables

| Variable | Default | Description |
|---|---|---|
| `K8S_NAMESPACE` | `forge` | Namespace to create Jobs in |
| `K8S_RUNTIME_CLASS` | — | RuntimeClass name (e.g. `gvisor` for stronger isolation) |
| `CONTAINER_MEMORY_LIMIT` | `256Mi` | Memory limit per container |
| `CONTAINER_CPU_LIMIT` | `500m` | CPU limit per container |
| `KUBECONFIG` | `~/.kube/config` | Kubeconfig path (falls back to in-cluster credentials) |

## Running locally

```bash
cd src/systems/forge
DATABASE_URL=postgresql://postgres:pass@localhost:5432/forge \
  RUNTIME=docker \
  ALLOWED_IMAGES=alpine:3.19,ubuntu:22.04,python:3.12-slim \
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
  -e CONDUCTOR_FORWARD_KEY=your-shared-secret \
  forge:latest
```

## API

All endpoints require authentication via `X-User-ID` (injected by Conductor). The routes are registered in Conductor's service registry and require the `forge` service permissions.

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/executions` | `createExecution` on `executions` | Submit a new execution |
| `GET` | `/executions` | `listExecution` on `executions` | List the caller's executions |
| `GET` | `/executions/{id}` | `getExecution` on `executions` | Get a single execution |
| `DELETE` | `/executions/{id}` | `deleteExecution` on `executions` | Cancel a pending or running execution |

### Submit an execution

```bash
curl -X POST http://conductor:8082/executions \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "image": "python:3.12-slim",
    "command": ["python", "-c", "print(\"hello\")"],
    "env": {"MY_VAR": "value"},
    "timeout": 30
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

### Execution object

```json
{
  "execution_id": "uuid",
  "user_id": "uuid",
  "image": "python:3.12-slim",
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
- `NetworkMode: none` — no network access
- `ReadonlyRootfs: true` — read-only root filesystem
- `/tmp` — writable tmpfs (64 MB)
- `CapDrop: ALL` — all Linux capabilities dropped
- `no-new-privileges` security option
- Memory limit: 256 MB
- CPU quota: 50% of one core
- PID limit: 64

### Container sandbox (Kubernetes runtime)

Every Job runs with:
- `AutomountServiceAccountToken: false` — no cluster credentials
- `RunAsNonRoot: true`
- `AllowPrivilegeEscalation: false`
- `ReadOnlyRootFilesystem: true`
- All capabilities dropped
- Seccomp profile: `RuntimeDefault`
- `/tmp` EmptyDir (64 Mi)
- `BackoffLimit: 0` — failures are not retried

### Image allowlist

Forge denies all submissions when `ALLOWED_IMAGES` is not configured. In production, set this to a minimal list of vetted images.

### Environment variable denylist

The following env keys are always rejected regardless of case:

`LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`, `PYTHONSTARTUP`, `PYTHONPATH`, `NODE_OPTIONS`, `NODE_PATH`, `RUBYOPT`, `RUBYLIB`, `PERL5LIB`, `PERLLIB`, `JAVA_TOOL_OPTIONS`, `JAVA_OPTIONS`, `_JAVA_OPTIONS`, `DYLD_INSERT_LIBRARIES`, `DYLD_LIBRARY_PATH`

### Request authentication

Forge trusts the `X-User-ID` header injected by Conductor. When `CONDUCTOR_FORWARD_KEY` is set, Forge verifies the accompanying `X-Conductor-Token` HMAC-SHA256 signature and rejects requests where the token is missing, expired (> 30 seconds), or invalid. Configure this key in all production deployments to prevent identity spoofing from any other service on the internal network.

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
