# Forge — Architecture

## Overview

Forge runs user-submitted commands in isolated containers. Jobs are queued in PostgreSQL and executed asynchronously by a worker pool. Two container runtimes are supported: Kubernetes (creates a Job per execution) and Docker (direct container run).

```
Client (Bearer JWT)
  |
  v
POST /executions
  |
  +-- checkGatekeeper() --> POST /check_permissions  (direct, bypasses Conductor)
  |
  +-- Validate image against ALLOWED_IMAGES allowlist
  |
  +-- Validate env keys (POSIX names + blocked-key denylist)
  |
  +-- Resolve runner class from database (resource limits)
  |
  +-- INSERT INTO executions (status=pending, runner_class=...)
  |
  v
WorkerPool (goroutines)
  |
  +-- Pick next pending execution
  |
  +-- kubernetes runtime --> create Kubernetes Job
      docker runtime    --> docker run (via socket)
  |
  +-- Wait for container exit
  |
  +-- UPDATE executions SET status, exit_code, stdout, stderr
```

## Why Forge calls Gatekeeper directly

Forge verifies permissions against `POST /check_permissions` on Gatekeeper using the caller's Bearer token, even when called through Conductor. This prevents a compromised or misconfigured Conductor from escalating privileges by spoofing the `X-User-ID` header inside the execution sandbox. It is the only service in the platform that does not trust Conductor's identity injection.

## Security controls

### Image allowlist

The operator configures `ALLOWED_IMAGES` (comma-separated). Any submission referencing an unlisted image is rejected `400 Bad Request`. When `ALLOWED_IMAGES` is unset, all submissions are rejected — deny-all is the default. Images are pulled automatically from the registry when not already present on the Docker host.

### Environment variable denylist

User-supplied `env` keys must match `[A-Za-z_][A-Za-z0-9_]*` and must not be in the blocked-key list:

```
LD_PRELOAD, LD_LIBRARY_PATH, LD_AUDIT
PYTHONSTARTUP, PYTHONPATH
NODE_OPTIONS, NODE_PATH
RUBYOPT, RUBYLIB
PERL5LIB, PERLLIB
JAVA_TOOL_OPTIONS, JAVA_OPTIONS, _JAVA_OPTIONS
DYLD_INSERT_LIBRARIES, DYLD_LIBRARY_PATH
```

These names can redirect interpreter or dynamic linker behaviour in user containers, enabling sandbox escapes.

## Runner classes

Runner classes define the resource limits for an execution tier. They are stored in a `runner_classes` table (GORM AutoMigrate), seeded with three defaults on startup, and managed at runtime via the `/runner-classes` API.

| Name | Memory | CPU | Pids | Tmpfs |
|------|--------|-----|------|-------|
| `standard` | 256 MB | 500m | 64 | 64 MB |
| `large` | 2048 MB | 2000m | 256 | 512 MB |
| `xlarge` | 8192 MB | 4000m | 512 | 2048 MB |

When a submission arrives, `runnerClassSpec()` fetches the named class from the database and passes the limits to the container runtime. Disabled classes are rejected at submission time with `400 Bad Request`. Changes to runner classes take effect immediately for new submissions — no restart required.

## Execution lifecycle

```
pending   -- inserted; waiting for a worker slot
running   -- container started
completed -- container exited successfully
failed    -- container exited with a non-zero / unexpected status
timed_out -- execution exceeded timeout_secs
cancelled -- cancelled by the user
             (pending: DB update; running: pool.Cancel signal)
```

## Data model

### executions table

Created with raw SQL (`pgxpool`) on startup:

```
executions
  execution_id  TEXT   PRIMARY KEY
  user_id       TEXT
  image         TEXT
  command       JSONB  ([]string)
  env           JSONB  (map[string]string)
  timeout_secs  INT    DEFAULT 30
  runner_class  TEXT   DEFAULT 'standard'
  status        TEXT   DEFAULT 'pending'
  exit_code     INT    (nullable)
  stdout        TEXT   (nullable, capped at 1 MB)
  stderr        TEXT   (nullable, capped at 1 MB)
  created_at    TIMESTAMPTZ
  started_at    TIMESTAMPTZ (nullable)
  ended_at      TIMESTAMPTZ (nullable)
```

`stdout` and `stderr` are populated only on `GET /executions/{id}` — the list endpoint omits them to keep payloads small.

### runner_classes table

Managed by GORM AutoMigrate, seeded on startup:

```
runner_classes
  name            TEXT   PRIMARY KEY
  memory_mb       INT    NOT NULL
  cpu_millicores  INT    NOT NULL
  pids_limit      INT    NOT NULL  DEFAULT 64
  tmpfs_mb        INT    NOT NULL  DEFAULT 64
  enabled         BOOL   NOT NULL  DEFAULT true
```

## Worker pool

The pool is a fixed set of goroutines sharing a work queue. Each goroutine calls the configured runtime to start the container, blocks until it exits, then writes the result to the DB. When `DELETE /executions/{id}` is called for a running execution, `pool.Cancel(id)` sends a cancellation signal via a stored `context.CancelFunc`.

## Docker runtime

With `RUNTIME=docker`, Forge runs each execution as a short-lived container on the local Docker daemon. Container configuration:
- Network mode from `FORGE_NETWORK_MODE` (default `none`); `host` and `bridge` are rejected at startup
- Read-only root filesystem with a writable tmpfs at `/tmp` (size from runner class)
- Memory, CPU quota, and PID limit from the runner class
- All capabilities dropped, `no-new-privileges` secopt
- Egress proxy vars injected when `FORGE_EGRESS_PROXY` is set
- Image pulled automatically when not present on the host

## Kubernetes runtime

With `RUNTIME=kubernetes`, Forge creates a Kubernetes `Job` per execution in `K8S_NAMESPACE`. Configuration:
- Resource limits (memory, CPU) and `/tmp` EmptyDir size from the runner class
- Optional `RuntimeClass` (e.g. `gvisor`, or Kata Containers) for stronger isolation — set per-backend via the `runtime_class` config key (the `kata` backend type requires it) or process-wide via `K8S_RUNTIME_CLASS`; see [kata.md](kata.md)
- `AutomountServiceAccountToken: false`, `RunAsNonRoot: true`, all capabilities dropped, seccomp `RuntimeDefault`
- `BackoffLimit: 0` — failures are not retried
- Jobs are deleted immediately after logs are collected

The Forge service account requires `create`, `get`, `delete` on `Jobs` and `Pods` in the forge namespace — see `infra/helm/codearmory/templates/forge-rbac.yaml`.

## Metrics

| Metric | Labels |
|--------|--------|
| `forge.executions.submitted.total` | `image` |
| `forge.executions.completed.total` | `status` (completed / failed / timed_out / cancelled) |
| `forge.executions.cancelled.total` | — |
