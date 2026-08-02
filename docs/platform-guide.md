# CodeArmory Platform Guide

This guide explains what each CodeArmory service does, how they fit together, and what you can build with them. It is aimed at engineers evaluating or onboarding to the platform.

---

## Table of contents

1. [How the platform fits together](#how-the-platform-fits-together)
2. [Authentication and authorisation — Gatekeeper](#authentication-and-authorisation--gatekeeper)
3. [API Gateway — Conductor](#api-gateway--conductor)
4. [Service Discovery — Registry](#service-discovery--registry)
5. [Sandboxed Execution — Forge](#sandboxed-execution--forge)
6. [Git Credentials — Git](#git-credentials--git)
7. [Git Hosting — git_factory](#git-hosting--git_factory)
8. [Pipeline Orchestration — Workflows](#pipeline-orchestration--workflows)
9. [Git Webhooks — Hooks](#git-webhooks--hooks)
10. [Issue Tracking — Tickets](#issue-tracking--tickets)
11. [Images and Artifacts — Containers and Artifacts](#images-and-artifacts--containers-and-artifacts)
12. [Cluster Integrations — Outposts](#cluster-integrations--outposts)
13. [CLI — Armory](#cli--armory)
14. [Observability](#observability)
15. [Running the stack](#running-the-stack)

---

The platform is modular. The sections below cover the core services that ship in this repo; additional capabilities are deployed and registered at runtime as modules by **Builder**.

---

## How the platform fits together

Every user-facing request enters through **Conductor** (the API gateway). Conductor validates the request, checks the caller's permissions with **Gatekeeper**, then proxies to the appropriate backend service. No backend service is exposed directly to users.

```
Browser / CLI
        │
        ▼
   Conductor :8080           ← single entry point for all traffic
        │
        ├── POST /signup, POST /login ──► Gatekeeper :8081  (public)
        │
        ├── /executions/...       ──────► Forge      :8083  (sandboxed runners)
        ├── /git_connector/...    ──────► git_connector :8096  (clone-credential broker)
        ├── /workflows/...        ──────► Workflows  :8085  (pipelines)
        └── /hooks/...            ──────► Hooks      :8087  (webhook receiver)

   Registry :8082  ← Conductor polls this to build its routing table
   Builder  :8095  ← org control plane; deploys + registers optional service modules
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

Permissions follow a `resource/subresource` path structure. A role grants named permissions (e.g. `createExecution`, `triggerRun`) on a resource path. Roles can be attached to individual users or to teams.

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
- **No network** — containers cannot make outbound connections unless explicitly configured. When outbound access is needed, Forge supports optional egress allowlisting so workloads can reach only an approved set of domains.
- **Dropped capabilities** — all Linux capabilities are dropped; only the minimum required to run the command are re-added.
- **Resource limits** — CPU and memory limits are set per execution.
- **Timeout enforcement** — containers that exceed `timeout_secs` are forcibly stopped.

### Integration with Workflows

Forge is registered as a named service in the Workflows `SERVICES` env var. A workflow step that targets `"service": "forge"` with `"path": "/executions"` will trigger a Forge run, forwarding the caller's auth token so the execution is attributed to the right user.

---

## Git Credentials — Git

**Port:** 8096

The **git_connector** service (in-repo directory `src/systems/git`) is the core **credential broker** for source control. Users link one or more git backends — **GitHub, GitLab, Forgejo, or a generic git server** — and the broker mints (or just-in-time brokers) clone credentials on demand for whatever backend a repository belongs to. It is *not* a repository-management service; it does not create repos or pull requests. Its sole job is answering *"give me an authenticated clone URL for this repo"* — for a user directly, or for Forge/Workflows running a job on their behalf.

### How it works

Link a backend once; then any caller asks for a credential by **repository URL** and the broker derives the host, finds the caller's backend for that host, and mints or brokers a credential appropriate to its auth mode. It returns the username/secret plus an authenticated HTTPS `clone_url`.

Like Forge, Git verifies every request **directly with Gatekeeper** (`forward_auth: true`) — the resolved `user_id` is the credential owner, so a compromised gateway cannot mint credentials by spoofing `X-User-ID`.

### Backends and auth modes

The broker mints **short-lived** credentials where the backend supports it, and otherwise brokers a stored secret just-in-time:

| Type | Mode | Behaviour |
|------|------|-----------|
| `github` | `app` | GitHub App → ~1h installation token (genuinely short-lived) |
| `github` | `pat` | Brokers a stored personal access token |
| `gitlab` | `oauth` | Refresh token → ~2h access token (short-lived; refresh token rotated/persisted) |
| `gitlab` | `token` | Brokers a stored access token |
| `forgejo` | `admin` | Admin token mints a per-user, repo-scoped, revoke-on-reuse token |
| `forgejo` | `token` | Brokers a stored personal access token |
| `generic` | `basic` | Brokers a stored username + password/token over HTTPS basic auth |

The minting modes (`github` `app`, `gitlab` `oauth`, `forgejo` `admin`) reduce credential blast radius; the `pat`/`token`/`basic` modes broker a long-lived secret you supplied — held encrypted and released only at mint time.

### Encryption at rest

All credential material is sealed with **AES-256-GCM** under a key derived from `GIT_ENCRYPTION_KEY` before it touches the database. The service refuses to start without that key, and the sealed credential is never returned by backend reads or logged.

### Integration with Forge

A Forge job's `secret_ref` of the form `git:<https-repo-url>` makes Forge call the broker's internal `clone-token` endpoint at dispatch time and inject an authenticated clone URL into the job env — never persisted on the execution record. This works across all backend types. (The older `gitea:<owner>/<repo>` scheme still works but targets the optional `gitea_integration` service.) See [Git README](git/README.md).

### Not the same as `gitea_integration`

`gitea_integration` is a separate, **optional non-core** service for Forgejo/Gitea **repository management** (repos, branches, pull requests), deployed by Builder on demand. Git (this service) only brokers clone credentials and is core. Use either, both, or neither.

---

## Git Hosting — git_factory

**Port:** 9002 · **Service identity:** `codearmory_git_factory`

Where [git_connector](#git-credentials--git) brokers access to *someone else's* git server,
**git_factory is a git server**. It stores bare repositories and serves them over Smart
HTTP, so the platform can host the code it builds. It is also one of git_connector's
backends, so hosting here is an option rather than a migration — repos on GitHub, GitLab
or Forgejo keep working exactly as before.

> **Naming.** The directory and Go module are `git-factory`, but the service registers as
> `codearmory_git_factory`, which is also the first segment of every RBAC resource. The
> module name is not the service identity.

### What it does

- **Repositories** over `git clone` / `git push`, authorised by Gatekeeper. Repos belong
  to a user or an org namespace and are addressed on the wire as `/{ns}/{repo}.git`.
- **Collaborators** — share a repo with another user at a read or write level, expressed
  as gatekeeper namespace roles rather than a private ACL.
- **Visibility** — a public repo authorises reads with no grant at all.
- **Branch protection** and **pull requests** (open, close, merge).
- **Pull-through mirrors** — hold a warm copy of an upstream repo so CI clones stay
  in-cluster instead of paying a full clone against the upstream every run. Opt in per
  backend with git_connector's `prefer_mirror`; nothing in Forge changes, only the URL
  the broker hands back.
- **Clone tokens** — short-lived HMAC tokens so a runner can clone without a user
  credential.

### Storage

Repositories live under `GIT_STORAGE_ROOT`, and **this is the only copy of pushed
source** — unlike a cache, nothing else can reconstruct it. The on-disk path derives from
a repo's stable id, never its name, so renaming a repo is a metadata-only change.

Because that root is "just a path", pointing it at the wrong kind of filesystem is a
silent catastrophe rather than an error: git's correctness depends on atomic
`O_CREAT|O_EXCL`, atomic `rename()` and enforced advisory locking, and object storage
mounted through FUSE (s3fs, gcsfuse, Mountpoint, rclone) provides none of them and
corrupts repositories instead of failing. The service therefore **verifies all three at
startup and refuses to start** when one is missing (`GIT_STORAGE_PREFLIGHT`).

The service runs as a **single replica** by default: one pod owns the bytes and git has
no cross-node write locking. Scaling out requires shared storage — see
[ARCHITECTURE §5](git-factory/ARCHITECTURE.md) — and the chart refuses at render time to
produce a configuration that scales without it.

### Per-record authorisation

git_factory is the reference implementation of the platform's **owner-first** resource
convention. A per-record resource names the repo's *owner* —
`<namespace>/codearmory_git_factory/repos/<id>` — rather than being scoped to whoever
asked, so a permission check is a real statement about that repo. Handlers load the repo
*before* checking, and a denial is rewritten to **404** so existence does not leak.

See [git_factory docs](git-factory/README.md).

---

## Pipeline Orchestration — Workflows

**Port:** 8085

Workflows runs sequences of HTTP steps — in order, one at a time — against registered backend services. It is the glue that connects Forge and any other registered service into a repeatable pipeline.

### Anatomy of a workflow

A workflow is a named list of steps. Each step specifies:

| Field | What it does |
|-------|-------------|
| `service` | Which registered service to call (e.g. `forge`) |
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

## Issue Tracking — Tickets

**Port:** 8086

Tickets is the platform's issue tracker: **boards**, **tickets**, **comments**, and
**custom fields**. It exists so work items and the builds that address them live in one
system — a ticket can carry a linked pipeline (`workflow_id`), a specific run (`run_id`),
and a sandboxed execution (`forge_execution_id`), which is what lets a pipeline open,
update or close a ticket as a step rather than through an external integration.

### Boards and field definitions

A board groups tickets and owns its own **status columns** and **priority options** —
they are not merged across boards, so one team's workflow states do not leak into
another's. A board with none configured falls back to the instance-wide defaults.

Those instance-wide defaults are **platform-owned** (`codearmory/tickets/field-defs`),
not owned by any user or org: deleting them would remove the default statuses for
everyone and break ticket creation platform-wide, so changing them requires an admin
grant rather than an ordinary user's own permissions.

### Projects

A board may be filed into a gatekeeper **Project**, in which case access is granted by a
project role as well as by ownership — the "owner OR project" gate. That is how a board
is shared with a team without handing over the owner's namespace.

---

## Images and Artifacts — Containers and Artifacts

**Containers — port 8089.** A Docker **registry proxy** giving each tenant its own image
repositories, so images built by a pipeline have somewhere to live inside the platform.
Registry configuration itself is platform-owned (`codearmory/containers/registries`), so
only an admin can add or retarget a registry, while ordinary users work within their own
namespace.

**Artifacts — port 8097.** Stores build artifacts produced by runs — the non-image
outputs a pipeline needs to keep or hand to a later step.

---

## Cluster Integrations — Outposts

**Ports:** Outpost Gateway 8092. Outpost: no port (dials out).

Some capabilities require acting *inside* a Kubernetes cluster. CodeArmory never reaches into a cluster from the control plane. Instead, a user-deployed **outpost** runs in (or against) each cluster and **dials out** to the control plane — no inbound access to your cluster and no control-plane cluster credentials. Run **one outpost per cluster** and a single pipeline can act across your whole fleet, so **pipelines unify across clusters**. For a single-cluster setup, run the outpost in the **same cluster as CodeArmory itself** (pointing at the in-cluster gateway); for many clusters, run one in each. Integrations are pluggable modules loaded by the outpost.

```
your cluster                                     control plane
┌─ outpost ───────────────┐   HTTPS    ┌─ Outpost Gateway :8092 ─────────────┐
│ pluggable modules       │  outbound  │ enroll · long-poll commands ·       │
│ (you choose which)      │ ◄────────► │ ingest events  (Postgres backbone)  │
└─────────────────────────┘  long-poll └──────────┬──────────────────────────┘
                              + POST     dispatch to consumer
                                         consumer service → Workflows / Hooks / Portal
```

**How it flows.** A consumer service enqueues a *command* for an outpost via the gateway. The outpost long-polls, the right module performs the action in-cluster, and reports *events* back. The gateway delivers each event to the owning consumer service, which updates its records and weaves the result into workflows, hooks, and the portal. Everything is at-least-once and idempotent, backed by Postgres queues — no message broker.

### Deploying an outpost

1. In the portal's **Outposts** page, add an outpost and choose its modules. Copy the single-use enrollment token.
2. Install the outpost in your cluster with the dedicated chart:

   ```bash
   helm install my-outpost infra/helm/outpost \
     --namespace codearmory-outpost --create-namespace \
     --set controlPlaneURL=https://gateway.example.com \
     --set enrollmentToken=<token>
   ```

   The chart grants least-privilege RBAC per module (see the [chart README](../infra/helm/outpost/README.md)). For a **single-cluster setup**, install the outpost alongside CodeArmory and point `controlPlaneURL` at the in-cluster gateway service (e.g. `http://codearmory-outpost-gateway:8092`).
3. The outpost's status moves to `connected` once it enrolls and heartbeats.

Integrations are pluggable — adding the next one is a new outpost module plus a thin consumer service, nothing in the core. Full detail: [outpost/README.md](outpost/README.md).

---

## CLI — Armory

The Armory CLI is a command-line client for the full platform. It covers the complete API surface of all services, with consistent auth handling (reads a stored token or prompts for credentials), output formatting, and `--help` documentation for every command.

### Installation

Pre-built binaries for Linux, macOS, and Windows are attached to each [GitHub release](../../../releases). Download and place the binary on your `PATH`.

### Authentication

```bash
armory auth login --url http://conductor:8080 --email alice@example.com
# Prompts for password; stores token locally
```

The CLI stores the JWT and refresh token and renews them transparently.

### Common operations

```bash
# Org management (admin)
armory admin orgs create acme
armory admin orgs invite acme bob@example.com

# Trigger a pipeline run
armory pipelines run pipeline <pipeline-id> --input ENV=staging --input VERSION=v1.2

# Watch a run
armory pipelines get run <run-id>

# Run a sandboxed execution
armory forge run --image alpine:3.19 -- sh -c "echo hello"
```

Services are registered with the platform through the **Registry manifest**
(`infra/local/registry-manifest.json`, or the Helm equivalent), not via the CLI —
see [Registry](registry/README.md). Conductor will not route to a service that is
not in the manifest.

---

## Observability

All services emit OpenTelemetry traces and metrics. Set `OTEL_EXPORTER_OTLP_ENDPOINT` on any service to export to your collector. Omit the variable to disable telemetry with no other changes required.

**The value must include a scheme and a non-empty host**, e.g. `http://otel-collector:4318`. A scheme-only value such as `http://` (empty host) will be accepted by the env-var guard but will produce runtime errors of the form `Post "http:///v1/logs": http: no Host in request URL` as the exporter tries to flush. If you see this in the logs, check that `otelEndpoint` in your Helm values (or `OTEL_EXPORTER_OTLP_ENDPOINT` in your env) contains a full URL.

### Key metrics

| Service | Metric | Labels |
|---------|--------|--------|
| Conductor | `conductor.requests.allowed.total` | — |
| Conductor | `conductor.requests.rejected.total` | `reason` (no_token/malformed_token/unauthorized/gatekeeper_error/user_not_found) |
| Conductor | `conductor.ips.blocked.total` | `source_ip` |
| Forge | `forge.executions.submitted.total` | `image` |
| Forge | `forge.executions.completed.total` | `status` |
| Forge | `forge.executions.cancelled.total` | — |
| Gatekeeper | `gatekeeper.logins.total` | — |
| Gatekeeper | `gatekeeper.permission_checks.total` | `result` (allowed/denied) |
| Hooks | `hooks.received.total` | `repo` |
| Hooks | `hooks.rules.matched.total` | `repo`, `workflow.id` |
| Hooks | `hooks.runs.triggered.total` | `workflow.id` |
| Workflows | `workflows.runs.triggered.total` | `workflow.id` |
| Workflows | `workflows.runs.completed.total` | `workflow.id`, `status` |
| Workflows | `workflows.steps.completed.total` | `workflow.id`, `status` |

Each service exposes a `GET /healthz` endpoint that returns `200 OK` when the service and its database connection are healthy. Use this for liveness and readiness probes.

---

## Running the stack

### Local development (Docker Compose)

```bash
cd infra/local
docker compose up --build
```

This starts PostgreSQL, Redis, and the core services. Services are available at their respective ports; Conductor at `:8080` is the entry point for API calls.

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
| Control plane | Gatekeeper, Conductor, Registry, Builder, Portal |
| CI/CD pipelines | Add Forge, Git, Workflows, Hooks |
| Cluster integrations | Add Outpost Gateway, and deploy an outpost in the target cluster |
| Optional capabilities | Deployed and registered at runtime as modules by Builder (e.g. `gitea_integration` for Forgejo/Gitea repo management) |

Gatekeeper, Conductor, Registry, Builder, and Portal are the core required by any configuration. The remaining services are independently deployable.

### Environment variable conventions

All services share these conventions:

- Every secret variable supports a `_FILE` suffix that reads the value from a file path (Docker secrets / Kubernetes secret mounts).
- `LOG_LEVEL=debug` enables verbose structured logging on any service.
- `OTEL_EXPORTER_OTLP_ENDPOINT` enables telemetry export; omitting it disables telemetry entirely. Must be a full URL with host (e.g. `http://otel-collector:4318`) — a scheme-only value like `http://` causes silent exporter errors.
- `PORT` overrides the default listen port on every service.
