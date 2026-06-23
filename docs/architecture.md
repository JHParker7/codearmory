# CodeArmory — System Architecture

## Services

| Service | Port | Role |
|---------|------|------|
| [Gatekeeper](gatekeeper/architecture.md)             | 8081 | Authentication, session management, RBAC, OIDC provider |
| Blueprints                                           | 8093 | Self-hosted Terraform HTTP backend (separate repo) |
| [Conductor](conductor/architecture.md)               | 8080 | API gateway — identity check, RBAC, reverse proxy |
| [Forge](forge/architecture.md)                       | 8083 | Sandboxed container execution |
| [Registry](registry/architecture.md)                 | 8082 | Service catalogue polled by Conductor |
| [Workflows](workflows/architecture.md)               | 8085 | CI/CD pipeline orchestrator |
| Tickets                                              | 8086 | Org-scoped task tracker (separate repo) |
| [Hooks](hooks/architecture.md)                       | 8087 | Webhook receiver and pipeline trigger |
| Gitea Integration                                    | 8088 | Forgejo/Gitea repository and PR management (separate repo) |
| Containers                                           | 8089 | OCI registry management proxy (separate repo) |
| Chaos                                                | 8090 | Chaos-engineering control plane (outpost integration) (separate repo) |
| Argo                                                 | 8091 | Argo CD sync control plane (outpost integration) (separate repo) |
| [Outpost Gateway](outpost-gateway/README.md)         | 8092 | Outpost-facing connection point + event backbone |
| [Outpost](outpost/README.md)                         | —    | Customer-deployed in-cluster agent (chaos/argo modules) |
| Egress Proxy                                         | 3128 | Optional allowlist-enforcing HTTP CONNECT proxy for Forge (separate repo) |
| MCP Server                                           | stdio | Local MCP server wrapping the full platform API (separate repo) |

Services marked *(separate repo)* live in their own `codearmory-<svc>` repos and are deployed + registered at runtime by **builder**; the rest are core services in this monorepo.

## Service topology

```
+------------------------------------------------------------------+
|                       External traffic                           |
+-------------------+---------------------+------------------------+
|  REST API clients |  Terraform clients  |  Git / CI webhooks     |
+-------------------+---------------------+------------------------+
        |                    |                       |
        | Bearer JWT         | Bearer or Basic       | POST /hooks
        v                    v                       v
+---------------+    +----------------+    +------------------+
|   Conductor   |    |   Blueprints   |    |     Hooks        |
|   :8080       |    |   :8093        |    |     :8087        |
|   API gateway |    |   Terraform    |    |  Rule match      |
|   RBAC proxy  |    |   state store  |    |  + dispatch      |
+-------+-------+    +-------+--------+    +--------+---------+
        |                    |                       |
        | routes to          | check_permissions     | HMAC-signed trigger
        |                    v                       v
  +-----+------+     +----------------+    +------------------+
  |            |     |   Gatekeeper   |    |   Workflows      |
  v            v     |   :8081        |    |   :8085          |
Gatekeeper  Registry |   Auth / RBAC  |    |   Worker pool    |
:8081       :8082    |   JWT issuance |    |   Step runner    |
            |        +----------------+    +--------+---------+
            |                                       |
            | 30-second poll                        | HTTP steps
            v                                       v
      Conductor routing                  any registered service
      table (in-memory)                 (Forge, Blueprints, ...)

  All services call POST /check_permissions on Gatekeeper
  to verify the caller's Bearer JWT and action+resource pair.
  Forge additionally calls Gatekeeper directly, bypassing
  Conductor entirely, to prevent X-User-ID header spoofing.

  Shared infrastructure
    PostgreSQL  — one isolated database per service
    Redis       — Gatekeeper (permission cache, session store)
                — Blueprints (state read cache)
    OTel        — traces, metrics, and logs from all services
```

## Authentication model

Authentication is centralised in Gatekeeper. Every service delegates identity and permission checking to it rather than verifying JWTs locally. Three patterns are in use:

### 1. Conductor gateway auth (most services)

```
Client --Bearer JWT--> Conductor
                           |
                           +-- GET /users/{id} --> Gatekeeper  (identity check)
                           |                        verifies JWT signature
                           |
                           +-- X-User-ID header injected (signed with HMAC)
                           |
                           +-- forward to backend service
                                   |
                                   +-- POST /check_permissions --> Gatekeeper (RBAC)
```

Services receiving requests through Conductor get a signed `X-User-ID` header and do their own permission check via Gatekeeper. Conductor only verifies that the user exists; RBAC is delegated to each backend.

### 2. Direct Gatekeeper auth (Blueprints, Forge, Hooks, Tickets, Workflows)

Each service calls `POST /check_permissions` on Gatekeeper, forwarding the caller's `Authorization: Bearer` token. Gatekeeper validates the JWT signature, evaluates the user's roles and permissions, and returns `{ authorized, user_id, org_id }`.

Forge uses this exclusively even when called through Conductor — it never trusts `X-User-ID` to prevent a compromised gateway from escalating privileges inside the execution sandbox.

### 3. Service key auth (service-to-service)

Services authenticate to Gatekeeper with `X-Service-Key: name:key`. Keys are bcrypt-hashed in Gatekeeper's database and rotated every 25 minutes via `POST /service-accounts/rotate-key`. The rotation loop is provided by the `codearmory_sdk/registry` package, shared by all services.

## Request flow through Conductor

```
Incoming request
  |
  +-- 1. Block check: (IP, userID) on block list? --> 403
  |
  +-- 2. Routing (hybrid):
  |        a. First path segment is a registered service name?
  |           Strip it, match remainder within that service's endpoints
  |        b. Otherwise, match full path across all registered endpoints
  |           404 if no match found
  |
  +-- 3. Path param validation (UUID/slug) --> 400 on bad input
  |
  +-- 4. Body validation (JSON check, 64 KB cap) --> 400/413
  |
  +-- 5. Identity check (unless endpoint is public):
  |        decode JWT locally --> GET /users/{id} on Gatekeeper
  |        10 failures from same (IP, userID) --> 1-hour block
  |
  +-- 6. Header rewrite:
  |        Strip: X-User-ID, X-Conductor-Token, X-Conductor-Timestamp,
  |               X-Forwarded-Host, X-Forwarded-Proto, X-Real-IP, X-Service-Key
  |        forward_auth=true  --> keep Authorization header
  |        forward_auth=false --> strip Authorization;
  |                               set X-User-ID + HMAC token
  |
  +-- 7. Reverse-proxy to backend service
```

## Event-driven pipeline

```
Git host / CI system
  |
  +-- POST /hooks -----------------------------------------> Hooks :8087
        |
        +-- Parse payload (repo, event, ref, commit, ...)
        |   X-Hook-Event header overrides body.event
        |
        +-- INSERT hook_events (status=received)
        |
        +-- Query matching rules (repo + event_type)
        |
        +-- For each matching rule:
        |     +-- Apply ref_filter
        |     +-- Verify X-Hub-Signature-256 HMAC if rule has a secret
        |     +-- Build inputs: HOOK_* defaults + input_mapping overrides
        |     +-- POST /internal/workflows/{id}/runs (HMAC-signed)
        |                      |
        |                      v
        |              Workflows :8085
        |                INSERT workflow_runs (status=pending)
        |                Worker pool picks up run
        |                Execute steps in sequence:
        |                  - Resolve service URL
        |                  - Substitute ${KEY} in path/body/headers
        |                  - Forward caller's Bearer token
        |                  - Record WorkflowStepRun result
        |
        +-- fire-and-forget: INSERT hook_triggers
        +-- fire-and-forget: UPDATE hook_events SET rules_matched, status
        +-- Return 200 (always, even on partial failure)
```

## Outpost integration framework

Cluster integrations (chaos, argo) never reach into a customer cluster from the control plane. Exactly one customer-deployed **outpost** runs in (or against) the target cluster and dials out to the **outpost-gateway** over HTTPS. The gateway is a Postgres event backbone: a command queue and an event outbox, distinct from Conductor.

```
 customer / self-hosted cluster                 control plane
 ┌─ outpost ───────────────┐   HTTPS    ┌─ outpost-gateway :8092 ─────────────┐
 │ modules:                │  outbound  │ enroll · long-poll commands ·       │
 │  chaos → litmus CRDs    │ ◄────────► │ ingest events (outpost-key auth)    │
 │  argo  → Argo CD Apps   │  long-poll └───────────┬─────────────────────────┘
 │ least-priv RBAC/module  │   + POST     Postgres backbone
 └─────────────────────────┘             outpost_commands  (queue, SKIP LOCKED)
                                          outpost_events    (outbox + dead-letter retry)
                                                  │ dispatch by integration (HTTP, HMAC)
                          user ─Conductor─►  Chaos :8090 · Argo :8091  ─► Workflows / Hooks / Portal
```

- **Commands** (control → outpost): a consumer service enqueues `{outpost_id, integration, type, payload}` via the gateway's internal API (shared-key HMAC); the outpost long-polls with `SKIP LOCKED` claiming and routes each to the matching module.
- **Events** (outpost → control): a module emits an event; the outpost POSTs it; the gateway outboxes it and the dispatcher delivers it to the integration's consumer (`/internal/events`, HMAC-signed) with dead-letter retry. Consumers dedupe by ID and correlate by a stable key (chaos: `experiment_id`; argo: `outpost_id`+`app_name`).

The control plane holds **zero** cluster credentials; all Kubernetes/CRD/Argo code lives in the outpost's modules. Self-hosted and SaaS use the identical mechanism — the difference is only whether the outpost shares the cluster with the control plane. Adding an integration is one outpost module + one consumer service + manifest entries; the outpost core, gateway, and backbone are untouched. See [outpost/README.md](outpost/README.md).

## Service-to-service trust

| Caller | Target | Mechanism |
|--------|--------|-----------|
| Conductor | Gatekeeper | Bearer JWT forwarded from original caller |
| Conductor | Registry | Static `REGISTRY_READ_KEY` |
| Hooks | Workflows | HMAC-SHA256 (`HOOKS_TRIGGER_KEY`) on `X-Hooks-Token` |
| Workflows | step services | Bearer JWT forwarded from original run trigger |
| All services | Gatekeeper | `X-Service-Key: name:key` (rotated every 25 min) |

## Database

Each service owns an isolated PostgreSQL database. Cross-service references (e.g. `workflow_id` in a ticket) are plain text — no cross-database FK constraints exist.

| Service | Database | Schema management |
|---------|----------|-------------------|
| Gatekeeper | `gatekeeper` | GORM AutoMigrate + idempotent FK constraints |
| Blueprints | `blueprints` | Raw SQL `CREATE TABLE IF NOT EXISTS` |
| Registry | `registry` | Raw SQL `CREATE TABLE IF NOT EXISTS` |
| Forge | `forge` | Raw SQL `CREATE TABLE IF NOT EXISTS` |
| Workflows | `workflows` | GORM AutoMigrate |
| Tickets | `tickets` | GORM AutoMigrate |
| Hooks | `hooks` | GORM AutoMigrate |
| Gitea Integration | `gitea_integration` | GORM AutoMigrate |
| Containers | — | Stateless — no local database |

## Observability

All services export structured JSON logs, OpenTelemetry traces (OTLP/HTTP), and OTel metrics when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The local stack (`infra/local/compose.yml`) routes them to:

| Component | Role |
|-----------|------|
| OTel Collector | Receives OTLP traces and metrics; routes to Tempo and Prometheus |
| Tempo | Distributed tracing storage |
| Prometheus | Metrics storage (remote-write from OTel Collector) |
| Loki | Log aggregation |
| Grafana | Unified dashboards (Tempo, Prometheus, Loki datasources pre-configured) |

Every handler creates an OTel span. Service-to-service calls propagate the W3C `traceparent` header so a single user request produces a cross-service trace tree visible in Grafana.
