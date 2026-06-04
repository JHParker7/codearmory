# CodeArmory Platform Guide

This guide explains what each CodeArmory service does, how they fit together, and what you can build with them. It is aimed at engineers evaluating or onboarding to the platform.

---

## Table of contents

1. [How the platform fits together](#how-the-platform-fits-together)
2. [Authentication and authorisation — Gatekeeper](#authentication-and-authorisation--gatekeeper)
3. [API Gateway — Conductor](#api-gateway--conductor)
4. [Service Discovery — Registry](#service-discovery--registry)
5. [Infrastructure State — Blueprints](#infrastructure-state--blueprints)
6. [Sandboxed Execution — Forge](#sandboxed-execution--forge)
7. [Pipeline Orchestration — Workflows](#pipeline-orchestration--workflows)
8. [Git Webhooks — Hooks](#git-webhooks--hooks)
9. [Task Tracking — Tickets](#task-tracking--tickets)
10. [Forgejo/Gitea Integration — Gitea Integration](#forgejoitea-integration--gitea-integration)
11. [Container Registry Management — Containers](#container-registry-management--containers)
12. [Egress Proxy](#egress-proxy)
13. [CLI — Armory](#cli--armory)
14. [Observability](#observability)
15. [Running the stack](#running-the-stack)

---

## How the platform fits together

Every user-facing request enters through **Conductor** (the API gateway). Conductor validates the request, checks the caller's permissions with **Gatekeeper**, then proxies to the appropriate backend service. No backend service is exposed directly to users.

```
Browser / CLI / Terraform
        │
        ▼
   Conductor :8080           ← single entry point for all traffic
        │
        ├── POST /signup, POST /login ──► Gatekeeper :8081  (public)
        │
        ├── /state/...            ──────► Blueprints        :8084  (Terraform state)
        ├── /executions/...       ──────► Forge             :8083  (sandboxed runners)
        ├── /workflows/...        ──────► Workflows         :8085  (pipelines)
        ├── /tickets/...          ──────► Tickets           :8086  (task tracker)
        ├── /hooks/...            ──────► Hooks             :8087  (webhook receiver)
        ├── /gitea_integration/...──────► Gitea Integration :8088  (repo / PR management)
        └── /containers/...       ──────► Containers        :8089  (OCI registry proxy)

   Registry :8082  ← Conductor polls this to build its routing table
```

**Auth model.** Every non-public route requires an `Authorization: Bearer <token>` header. The token is an ES256-signed JWT issued by Gatekeeper. Backend services never verify JWTs themselves — they ask Gatekeeper via `POST /check_permissions`, which returns the `user_id`, `org_id`, and whether the permission is granted. This means all permission logic is centralised in one place.

---

## Authentication and authorisation — Gatekeeper

**Port:** 8081

Gatekeeper is the identity and access control service. It handles everything related to who you are and what you are allowed to do.

### Identities

| Concept | Description |
|---------|-------------|
| **User** | An individual account with an email and password. |
| **Organisation** | A shared workspace. Users can belong to one org. |
| **Team** | A named group within an org. Used to assign roles collectively. |
| **Role** | A named collection of permissions, scoped to a resource path. |
| **Invite** | A signed token that adds a user to an org. |

### Sessions

Calling `POST /login` returns a short-lived JWT (default 15 minutes) and a long-lived refresh token (default 7 days). Use `POST /refresh` with the refresh token to get a new JWT without re-entering credentials. Both tokens are ES256-signed; the public key is available at `GET /public-key` for services that wish to verify locally.

### Permissions

Permissions follow a `resource/subresource` path structure. A role grants named permissions (e.g. `createTicket`, `triggerRun`) on a resource path. Roles can be attached to individual users or to teams.

When a backend service calls `POST /check_permissions`, it passes the Bearer token and the permission + resource it wants to check. Gatekeeper returns the resolved `user_id` and `org_id` alongside the allow/deny result. All authorisation cache entries are invalidated immediately when a role is updated.

### Key example flows

```bash
# Sign up
curl -X POST http://localhost:8080/signup \
  -d '{"email":"alice@example.com","password":"hunter2","username":"alice"}'

# Log in
curl -X POST http://localhost:8080/login \
  -d '{"email":"alice@example.com","password":"hunter2"}'
# → {"token":"<jwt>","refresh_token":"<refresh>"}

# Create an org
curl -X POST http://localhost:8080/orgs \
  -H "Authorization: Bearer <token>" \
  -d '{"name":"acme","display_name":"Acme Corp"}'

# Invite a user to the org
curl -X POST http://localhost:8080/orgs/acme/invites \
  -H "Authorization: Bearer <token>" \
  -d '{"email":"bob@example.com"}'
```

---

## API Gateway — Conductor

**Port:** 8080

Conductor is the single entry point for all API traffic. Its responsibilities are:

1. **Route resolution** — it polls Registry to learn which backend services exist and which path prefixes they own.
2. **Authentication** — it validates the Bearer JWT on every non-public request.
3. **User-existence check** — it confirms the calling user is still active in Gatekeeper before forwarding the request.
4. **Reverse proxy** — it forwards the request to the resolved backend, stripping its own path prefix where needed.

### Why route through Conductor?

You never need to know each service's internal port. External clients talk to `:8080` only. Internal services can change ports or hosts without clients updating their config — only Registry needs updating.

### Public routes

Only two routes bypass auth: `POST /signup` and `POST /login`. Everything else requires a valid JWT.

### Routing table

Conductor rebuilds its routing table every 30 seconds by polling `GET /services` on Registry. Each service registration includes a base URL and the path prefixes it handles. New services become available automatically; removed services stop receiving traffic at the next poll.

---

## Service Discovery — Registry

**Port:** 8082

Registry is a lightweight service manifest store. It stores one record per backend service (name, base URL, endpoints, and auth config). Conductor is the primary consumer — it polls Registry to learn where to route requests.

Registry is an internal admin service. It is accessed directly on port 8082 with a static admin key, not via Conductor.

### Registering a service

```bash
curl -X POST http://localhost:8082/services \
  -H "Authorization: Bearer <admin-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-internal-tool",
    "url": "http://internal-tool:9000",
    "description": "My internal deployment tool",
    "forward_auth": true,
    "service_key": "shared-secret-for-gatekeeper"
  }'
```

Once registered, Conductor picks up the new service on its next poll (every 30 seconds). Requests to `http://conductor:8080/my-internal-tool/...` are then authenticated and proxied to `http://internal-tool:9000/...`. Endpoints for the service are registered separately via `PUT /services/{id}/endpoints`.

---

## Infrastructure State — Blueprints

**Port:** 8084

Blueprints implements the [Terraform HTTP backend protocol](https://developer.hashicorp.com/terraform/language/settings/backends/http). It stores OpenTofu/Terraform workspace state in PostgreSQL with locking support to prevent concurrent state writes.

### Using Blueprints as a Terraform backend

In your Terraform configuration:

```hcl
terraform {
  backend "http" {
    address        = "http://conductor:8080/blueprints/state/alice/my-project"
    lock_address   = "http://conductor:8080/blueprints/state/alice/my-project"
    unlock_address = "http://conductor:8080/blueprints/state/alice/my-project"
    username       = "alice"
    password       = "<your-jwt>"
  }
}
```

No other changes to your Terraform code are needed. State is versioned and stored in Postgres; a complete audit trail of all state reads, writes, and lock operations is kept.

### State paths

| Scope | Path pattern |
|-------|-------------|
| User-scoped | `/state/{username}/{workspace}` |
| Org-scoped | `/{org}/state/{team}/{workspace}` |

Locking and unlocking use `LOCK` and `UNLOCK` HTTP methods on the same path, following the Terraform HTTP backend specification.

### Why self-hosted state?

- State files often contain secrets. With Blueprints, they never leave your network.
- You control the database backup strategy.
- No per-workspace costs or seat limits.

---

## Sandboxed Execution — Forge

**Port:** 8083

Forge runs user-submitted commands inside isolated Docker containers. It is the execution engine that Workflows steps can call to run build scripts, deployment tools, or arbitrary automation.

### How it works

An execution request specifies a Docker image, a command, optional environment variables, and optional file inputs. Forge pulls the image, starts a container, runs the command, streams stdout/stderr, and records the exit code. Containers run with a read-only root filesystem, no network access, and dropped Linux capabilities.

```bash
curl -X POST http://localhost:8080/forge/executions \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "image": "alpine:3.19",
    "command": ["sh", "-c", "echo Hello from Forge"],
    "timeout_secs": 60
  }'
# → {"execution_id":"uuid","status":"pending"}
```

### Execution lifecycle

```
pending → running → completed
                 → failed
                 → timeout
```

Poll `GET /executions/{id}` to check status. The full log output is available on the execution object once the run reaches a terminal state.

### Security model

Each container is isolated at the OS level. Forge enforces:

- **Read-only root filesystem** — the container cannot write to its own image layers.
- **No network** — containers cannot make outbound connections unless explicitly configured.
- **Dropped capabilities** — all Linux capabilities are dropped; only the minimum required to run the command are re-added.
- **Resource limits** — CPU and memory limits are set per execution.
- **Timeout enforcement** — containers that exceed `timeout_secs` are forcibly stopped.

### Integration with Workflows

Forge is registered as a named service in the Workflows `SERVICES` env var. A workflow step that targets `"service": "forge"` with `"path": "/executions"` will trigger a Forge run, forwarding the caller's auth token so the execution is attributed to the right user.

---

## Pipeline Orchestration — Workflows

**Port:** 8085

Workflows runs sequences of HTTP steps — in order, one at a time — against registered backend services. It is the glue that connects Forge, Blueprints, and any other service into a repeatable pipeline.

### Anatomy of a workflow

A workflow is a named list of steps. Each step specifies:

| Field | What it does |
|-------|-------------|
| `service` | Which registered service to call (e.g. `forge`, `blueprints`) |
| `method` | HTTP method (default `POST`) |
| `path` | Path on the target service. Supports `${KEY}` substitution. |
| `body` | JSON body. Supports `${KEY}` substitution. |
| `expected_status` | Exact status code required for success. Omit to accept any 2xx. |
| `timeout_secs` | Per-step timeout (default 30s, max 3600s). |

A maximum of 50 steps per workflow is enforced.

### Input substitution

`${KEY}` placeholders in `path`, `body`, and `headers` are replaced at run time with values from the `inputs` map passed when triggering the run. This lets one workflow definition serve multiple environments, versions, or parameters.

```json
{
  "steps": [
    {
      "name": "build",
      "service": "forge",
      "path": "/executions",
      "body": {"image": "node:20", "command": ["npm", "run", "build:${ENV}"]}
    }
  ]
}
```

```bash
# Trigger for staging
curl -X POST http://localhost:8080/workflows/workflows/{id}/runs \
  -H "Authorization: Bearer <token>" \
  -d '{"inputs": {"ENV": "staging"}}'
```

### How runs are executed

Runs are stored in PostgreSQL as `pending` and picked up by a background worker pool (5 goroutines). Workers claim pending runs using `SELECT FOR UPDATE SKIP LOCKED`, so there is no external queue required. Each step's response body (up to 1 MB) is stored for inspection.

The caller's Bearer token is stored with the run and forwarded to each step — steps execute with the same permissions as the person who triggered the run. The token is removed from the database as soon as the run reaches a terminal state.

### Cancellation

`DELETE /runs/{id}` cancels a pending or running run. The cancellation is authoritative at the database level; a best-effort signal is also sent to the in-progress goroutine. A run that completes between the cancel request and the signal is recorded as `completed`, not `cancelled`.

### Triggering from Hooks

Workflows exposes an internal endpoint (`POST /internal/workflows/{id}/runs`) that the Hooks service uses to trigger pipeline runs from Git events. This endpoint bypasses JWT auth and instead validates an HMAC-SHA256 token signed with a shared secret (`HOOKS_TRIGGER_KEY`), with a ±5 minute timestamp window to prevent replay attacks.

---

## Git Webhooks — Hooks

**Port:** 8087

Hooks receives incoming Git webhook payloads (from GitHub, GitLab, Gitea, or any compatible source) and triggers Workflows pipeline runs based on configurable pipeline rules.

### Pipeline rules

A pipeline rule specifies:

| Field | Description |
|-------|-------------|
| `event_type` | Webhook event to match (e.g. `push`, `pull_request`) |
| `repository` | Repository name or pattern to match |
| `branch` | Branch or pattern to match (supports wildcards) |
| `workflow_id` | Which workflow to trigger |
| `inputs` | Static inputs to pass to the triggered run |

When an incoming webhook matches a rule, Hooks calls `POST /internal/workflows/{id}/runs` on the Workflows service, signing the request with the shared HMAC key.

### Registering a webhook

```bash
# Create a pipeline rule
curl -X POST http://localhost:8080/hooks/rules \
  -H "Authorization: Bearer <token>" \
  -d '{
    "name": "deploy-on-push",
    "event_type": "push",
    "repository": "my-org/my-app",
    "branch": "main",
    "workflow_id": "uuid-of-deploy-workflow",
    "inputs": {"ENV": "production"}
  }'
```

Configure your Git provider to send webhook payloads to `http://conductor:8080/hooks/hooks`. Hooks verifies the webhook signature before processing the payload.

### Supported event types

Hooks processes any webhook payload format that includes repository and branch information. Standard events include `push`, `pull_request`, `tag`, and `release`. Multiple pipeline rules can match the same event.

---

## Task Tracking — Tickets

**Port:** 8086

Tickets is a lightweight task tracker scoped to organisations. Its distinguishing feature is that tickets can carry references to related Workflows runs and Forge executions, linking deployment tasks to the pipeline runs that executed them.

### Creating and managing tickets

```bash
# Create a ticket linked to a workflow run
curl -X POST http://localhost:8080/tickets/tickets \
  -H "Authorization: Bearer <token>" \
  -d '{
    "title": "Deploy v2.1.0 to production",
    "description": "Coordinate the production release",
    "priority": "high",
    "assignee_id": "user-uuid",
    "workflow_id": "wf-uuid",
    "run_id": "run-uuid"
  }'
```

### Status and priority

| Status | Meaning |
|--------|---------|
| `open` | Default — not started |
| `in_progress` | Actively being worked |
| `resolved` | Work complete |
| `closed` | No further action |

Priority values: `low`, `medium` (default), `high`, `critical`. Any status transition in any direction is permitted.

### Visibility

Tickets are visible to their creator and to any user in the same org. Requests from out-of-org users receive `404 Not Found` rather than `403 Forbidden` to avoid leaking whether a ticket ID exists. List queries are filtered at the database layer.

### Comments

Comments are threaded under tickets and follow the same visibility rules as the parent ticket. Comment deletion is soft (`active = false`). The comment author and any org member with access to the ticket can delete a comment.

### Linked resources

The `workflow_id`, `run_id`, and `forge_execution_id` fields are plain-text references — Tickets stores them but does not validate them against Workflows or Forge. They are useful for tracing which pipeline run corresponds to a given task.

---

## Forgejo/Gitea Integration — Gitea Integration

**Port:** 8088

The Gitea Integration service connects CodeArmory to a Forgejo (or Gitea) instance. Users link their CodeArmory account to their Forgejo identity with a one-time token verification, then use the platform API to manage repositories and pull requests without leaving the platform.

### Account linking

Before using any repository or PR endpoint, a user links their Forgejo account:

```bash
curl -X PUT http://localhost:8080/gitea_integration/account \
  -H "Authorization: Bearer <token>" \
  -d '{"gitea_username": "alice", "gitea_token": "<forgejo-pat>"}'
```

The `gitea_token` is a personal access token from Forgejo. It is used once to verify that the user controls the claimed Forgejo account and is never stored. Subsequent operations use the admin token with per-user `Sudo`.

### Repositories and pull requests

```bash
# Create a repository
curl -X POST http://localhost:8080/gitea_integration/repos \
  -H "Authorization: Bearer <token>" \
  -d '{"name": "my-app", "auto_init": true}'

# List open pull requests
curl http://localhost:8080/gitea_integration/repos/alice/my-app/pulls \
  -H "Authorization: Bearer <token>"

# Merge a pull request
curl -X POST http://localhost:8080/gitea_integration/repos/alice/my-app/pulls/1/merge \
  -H "Authorization: Bearer <token>" \
  -d '{"Do": "merge"}'
```

### Git smart protocol

The service also proxies the Git HTTP smart protocol, so `git clone`, `git push`, and `git pull` routed through the platform work transparently. Git clients authenticate with their Forgejo credentials directly.

---

## Container Registry Management — Containers

**Port:** 8089

The Containers service provides authenticated management access to an external OCI registry (Docker Hub, GHCR, ECR, or any distribution-spec registry). It adds RBAC-enforced visibility and deletion on top of the registry's native API, and proxies the OCI distribution protocol so `docker push`/`pull` can be routed through the platform.

```bash
# List repositories
curl http://localhost:8080/containers/repositories \
  -H "Authorization: Bearer <token>"

# List tags for an image
curl http://localhost:8080/containers/repositories/myorg/myapp/tags \
  -H "Authorization: Bearer <token>"

# Delete a manifest by digest
curl -X DELETE \
  http://localhost:8080/containers/repositories/myorg/myapp/manifests/sha256:abc123 \
  -H "Authorization: Bearer <token>"
```

The management API (`/repositories`, `/tags`, `/manifests`) is gated by Gatekeeper RBAC. The `/v2/...` OCI distribution proxy passes requests to the upstream registry without RBAC interception — Docker clients authenticate directly with their registry credentials.

---

## Egress Proxy

**Port:** 3128

The Egress Proxy is an allowlist-enforcing HTTP CONNECT proxy used by Forge execution containers. It provides controlled outbound internet access for CI/CD workloads (package downloads, module fetches) without opening broad internet access.

It is not a user-facing service — it sits on an internal Docker/Kubernetes network and is configured via Forge's `FORGE_EGRESS_PROXY` environment variable. Containers route outbound HTTP and HTTPS traffic through it automatically when `HTTP_PROXY`/`HTTPS_PROXY` are set.

Connections to hosts not in `PROXY_ALLOWED_DOMAINS` are refused before any data is exchanged. See the [Egress Proxy README](egress-proxy/README.md) and [Forge README](forge/README.md) for setup details.

---

## CLI — Armory

The Armory CLI is a command-line client for the full platform. It covers the complete API surface of all services, with consistent auth handling (reads a stored token or prompts for credentials), output formatting, and `--help` documentation for every command.

### Installation

Pre-built binaries for Linux, macOS, and Windows are attached to each [GitHub release](../../../releases). Download and place the binary on your `PATH`.

### Authentication

```bash
armory login --url http://conductor:8080 --email alice@example.com
# Prompts for password; stores token locally
```

The CLI stores the JWT and refresh token and renews them transparently.

### Common operations

```bash
# Org management
armory orgs create --name acme --display-name "Acme Corp"
armory orgs invite --org acme --email bob@example.com

# Trigger a workflow run
armory workflows run <workflow-id> --input ENV=staging --input VERSION=v1.2

# Watch a run
armory runs get <run-id>

# Manage tickets
armory tickets create --title "Deploy v2" --priority high
armory tickets update <ticket-id> --status in_progress

# Register a service with the platform
armory services register \
  --name my-tool \
  --url http://my-tool:9000 \
  --prefix /tools
```

---

## Observability

All services emit OpenTelemetry traces and metrics. Set `OTEL_EXPORTER_OTLP_ENDPOINT` on any service to export to your collector. Omit the variable to disable telemetry with no other changes required.

### Key metrics

| Service | Metric | Labels |
|---------|--------|--------|
| Gatekeeper | `gatekeeper.logins.total` | — |
| Gatekeeper | `gatekeeper.permission_checks.total` | `result` (allowed/denied) |
| Workflows | `workflows.runs.triggered.total` | `workflow.id` |
| Workflows | `workflows.runs.completed.total` | `workflow.id`, `status` |
| Workflows | `workflows.steps.completed.total` | `workflow.id`, `status` |
| Tickets | `tickets.created.total` | `priority` |
| Tickets | `tickets.resolved.total` | `status` |

Each service exposes a `GET /healthz` endpoint that returns `200 OK` when the service and its database connection are healthy. Use this for liveness and readiness probes.

---

## Running the stack

### Local development (Docker Compose)

```bash
cd infra/local
docker compose up --build
```

This starts PostgreSQL, Redis, and all eight services. Services are available at their respective ports; Conductor at `:8080` is the entry point for API calls.

### Production (Helm)

A Helm chart is included in the repository under `infra/helm`. Each service is configurable via `values.yaml`. Secrets (database passwords, service keys) can be provided via Kubernetes Secrets using the `_FILE` suffix on environment variables:

```yaml
env:
  DATABASE_URL_FILE: /var/run/secrets/db-url
```

### Minimal subset

You can run a subset of services depending on your use case:

| Use case | Required services |
|----------|------------------|
| Terraform state only | Gatekeeper, Conductor, Registry, Blueprints |
| CI/CD pipelines only | Gatekeeper, Conductor, Registry, Forge, Workflows, Hooks |
| Forge with egress control | Add Egress Proxy; set `FORGE_NETWORK_MODE` and `FORGE_EGRESS_PROXY` on Forge |
| Forgejo/Gitea integration | Add Gitea Integration; requires a running Forgejo instance |
| OCI registry management | Add Containers; requires an upstream OCI registry |
| Full platform | All services |

Gatekeeper, Conductor, and Registry are required by any configuration. The remaining services are independently deployable.

### Environment variable conventions

All services share these conventions:

- Every secret variable supports a `_FILE` suffix that reads the value from a file path (Docker secrets / Kubernetes secret mounts).
- `LOG_LEVEL=debug` enables verbose structured logging on any service.
- `OTEL_EXPORTER_OTLP_ENDPOINT` enables telemetry export; omitting it disables telemetry entirely.
- `PORT` overrides the default listen port on every service.
