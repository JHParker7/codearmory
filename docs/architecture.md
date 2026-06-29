# CodeArmory — System Architecture

## Services

| Service | Port | Role |
|---------|------|------|
| [Gatekeeper](gatekeeper/architecture.md)             | 8081 | Authentication, session management, RBAC, OIDC provider |
| [Conductor](conductor/architecture.md)               | 8080 | API gateway — identity check, RBAC, reverse proxy |
| [Registry](registry/architecture.md)                 | 8082 | Service catalogue polled by Conductor |
| Builder                                              | 8095 | Org control plane + runtime deployer/registrar of optional service modules |
| Portal                                               | —    | Web UI — React SPA + Express BFF proxying to Conductor |
| [Forge](forge/architecture.md)                       | 8083 | Sandboxed container execution |
| [Git](git/README.md)                                 | 8093 | Git credential broker — mints/brokers clone credentials for linked backends |
| [Workflows](workflows/architecture.md)               | 8085 | CI/CD pipeline orchestrator |
| [Hooks](hooks/architecture.md)                       | 8087 | Webhook receiver and pipeline trigger |
| [Outpost Gateway](outpost-gateway/README.md)         | 8092 | Outpost-facing connection point + event backbone |
| [Outpost](outpost/README.md)                         | —    | User-deployed in-cluster agent (pluggable integration modules) |

The platform is modular. The services above are the core that ships in this repo; additional capabilities are deployed and registered at runtime as **modules by Builder**.

## Service topology

```
+------------------------------------------------------------------+
|                       External traffic                           |
+----------------------------------+-------------------------------+
|        REST API clients          |     Git / CI webhooks         |
+----------------------------------+-------------------------------+
        |                                       |
        | Bearer JWT                            | POST /hooks
        v                                       v
+---------------+                      +------------------+
|   Conductor   |                      |     Hooks        |
|   :8080       |                      |     :8087        |
|   API gateway |                      |  Rule match      |
|   RBAC proxy  |                      |  + dispatch      |
+-------+-------+                      +--------+---------+
        |                                       |
        | routes to                             | HMAC-signed trigger
        |                                       v
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
      table (in-memory)                 (Forge, modules, ...)

  All services call POST /check_permissions on Gatekeeper
  to verify the caller's Bearer JWT and action+resource pair.
  Forge additionally calls Gatekeeper directly, bypassing
  Conductor entirely, to prevent X-User-ID header spoofing.

  Shared infrastructure
    PostgreSQL  — one isolated database per service
    Redis       — Gatekeeper (permission cache, session store)
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

### 2. Direct Gatekeeper auth (Forge, Hooks, Workflows)

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

Drive your own clusters from the control plane without granting it any inbound access or cluster credentials. A user-deployed **outpost** runs in (or against) each target cluster and dials out to the **outpost-gateway** over HTTPS — one outpost per cluster, so a single pipeline can drive actions across your whole fleet and **pipelines unify across clusters**. For a single-cluster setup the outpost runs in the **same cluster as CodeArmory itself** (against the in-cluster gateway); for many clusters, run one in each. The gateway is a Postgres event backbone: a command queue and an event outbox, distinct from Conductor. Integrations are pluggable modules loaded by the outpost.

```
 your cluster (co-located or remote)            control plane
 ┌─ outpost ───────────────┐   HTTPS    ┌─ outpost-gateway :8092 ─────────────┐
 │ pluggable modules       │  outbound  │ enroll · long-poll commands ·       │
 │ (least-priv RBAC each)  │ ◄────────► │ ingest events (outpost-key auth)    │
 │                         │  long-poll └───────────┬─────────────────────────┘
 │                         │   + POST     Postgres backbone
 └─────────────────────────┘             outpost_commands  (queue, SKIP LOCKED)
                                          outpost_events    (outbox + dead-letter retry)
                                                  │ dispatch to consumer (HTTP, HMAC)
                          user ─Conductor─►  consumer service  ─► Workflows / Hooks / Portal
```

- **Commands** (control → outpost): a consumer service enqueues `{outpost_id, integration, type, payload}` via the gateway's internal API (shared-key HMAC); the outpost long-polls with `SKIP LOCKED` claiming and routes each to the matching module.
- **Events** (outpost → control): a module emits an event; the outpost POSTs it; the gateway outboxes it and the dispatcher delivers it to the integration's consumer (`/internal/events`, HMAC-signed) with dead-letter retry. Consumers dedupe by ID and correlate by a stable key.

The control plane holds **zero** cluster credentials; all Kubernetes/CRD code lives in the outpost's modules. Single-cluster and multi-cluster setups use the identical mechanism — the difference is only whether the outpost shares the cluster with the control plane or runs in a remote one. Adding an integration is one outpost module + one consumer service + manifest entries; the outpost core, gateway, and backbone are untouched. See [outpost/README.md](outpost/README.md).

## Service-to-service trust

| Caller | Target | Mechanism |
|--------|--------|-----------|
| Conductor | Gatekeeper | Bearer JWT forwarded from original caller |
| Conductor | Registry | Static `REGISTRY_READ_KEY` |
| Hooks | Workflows | HMAC-SHA256 (`HOOKS_TRIGGER_KEY`) on `X-Hooks-Token` |
| Workflows | step services | Bearer JWT forwarded from original run trigger |
| All services | Gatekeeper | `X-Service-Key: name:key` (rotated every 25 min) |

## Database

Each service owns an isolated PostgreSQL database. Cross-service references (e.g. `workflow_id` in a run record) are plain text — no cross-database FK constraints exist.

| Service | Database | Schema management |
|---------|----------|-------------------|
| Gatekeeper | `gatekeeper` | GORM AutoMigrate + idempotent FK constraints |
| Registry | `registry` | Raw SQL `CREATE TABLE IF NOT EXISTS` |
| Forge | `forge` | Raw SQL `CREATE TABLE IF NOT EXISTS` |
| Workflows | `workflows` | GORM AutoMigrate |
| Hooks | `hooks` | GORM AutoMigrate |

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
