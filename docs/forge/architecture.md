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
  +-- INSERT INTO executions (status=pending)
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

The operator configures `ALLOWED_IMAGES` (comma-separated). Any submission referencing an unlisted image is rejected `400 Bad Request`. When `ALLOWED_IMAGES` is unset, all submissions are rejected — deny-all is the default.

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

Single table, created with raw SQL on startup:

```
executions
  execution_id  TEXT   PRIMARY KEY
  user_id       TEXT
  image         TEXT
  command       JSONB  ([]string)
  env           JSONB  (map[string]string)
  timeout_secs  INT    DEFAULT 30
  status        TEXT   DEFAULT 'pending'
  exit_code     INT    (nullable)
  stdout        TEXT   (nullable, capped at 1 MB)
  stderr        TEXT   (nullable, capped at 1 MB)
  created_at    TIMESTAMPTZ
  started_at    TIMESTAMPTZ (nullable)
  ended_at      TIMESTAMPTZ (nullable)
```

`stdout` and `stderr` are populated only on `GET /executions/{id}` — the list endpoint omits them to keep payloads small.

## Worker pool

The pool is a fixed set of goroutines sharing a work queue. Each goroutine calls the configured runtime to start the container, blocks until it exits, then writes the result to the DB. When `DELETE /executions/{id}` is called for a running execution, `pool.Cancel(id)` sends a cancellation signal via a stored `context.CancelFunc`.

## Kubernetes runtime

With `RUNTIME=kubernetes`, Forge creates a Kubernetes `Job` per execution in `K8S_NAMESPACE`. An optional `RuntimeClass` (e.g. `gvisor`) can be set for stronger isolation. Resource limits `CONTAINER_MEMORY_LIMIT` and `CONTAINER_CPU_LIMIT` are applied to every sandbox container.

The Forge service account requires `create`, `get`, `delete` on `Jobs` and `Pods` in the forge namespace — see `infra/helm/codearmory/templates/forge-rbac.yaml`.

## Metrics

| Metric | Labels |
|--------|--------|
| `forge.executions.submitted.total` | `image` |
| `forge.executions.completed.total` | `status` (completed / failed / timed_out / cancelled) |
| `forge.executions.cancelled.total` | — |
