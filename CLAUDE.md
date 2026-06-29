# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

### Unit tests
Each service is tested independently. The Go workspace is at `src/systems/`; the CLI is a separate module at `src/cli/`.

```bash
cd src/systems/<service> && go test .      # e.g. gatekeeper, workflows, hooks
cd src/cli && go test .

# Run a single test
cd src/systems/gatekeeper && go test -run TestCreateOrg_Success .
```

### Build
```bash
cd src/systems/<service> && go build ./...
cd src/cli && go build -o armory .
```

### Local stack
```bash
cd infra/local
export DOCKER_GID=$(stat -c '%g' /var/run/docker.sock)
docker compose up --build
```

### Integration tests
Requires the compose stack to be running. Integration tests are in `tests/` (Python/pytest) and also runnable as Docker Compose profiles:

```bash
# Via Docker (matches CI)
docker compose --profile test run --rm gatekeeper-integration-tests
docker compose --profile test run --rm workflows-integration-tests
# ... same pattern for other services

# Directly with pytest (stack must be running)
pip install -r tests/gatekeeper/requirements.txt
pytest tests/gatekeeper -v
```

### Helm
```bash
# Validate chart renders without errors
helm template test-release infra/helm/codearmory -f <values-file>
```

### Commit hooks
```bash
pip install pre-commit && pre-commit install --hook-type commit-msg
```
Commits must follow Conventional Commits format (`feat:`, `fix:`, `chore:`, etc.) and are scanned for secrets by gitleaks. **Commit messages must be single-line — no body or bullet points.**

## Architecture

### Repository scope (core monorepo + spun-off services)
This monorepo holds the **core control plane**: conductor, gatekeeper, registry, builder, portal, workflows, forge, hooks, the forge **egress-proxy**, and the outpost pair (`outpost` + `outpost-gateway`). The remaining services were spun out into their own `codearmory-<svc>` repos — **argo, blueprints, chaos, containers, gitea_integration, mcp, notifications, tickets** — but are still part of the platform: **builder** deploys and registers them at runtime from the images those repos publish (their deploy defs stay in `src/systems/builder/files/services/*.json`). References to these services below describe platform behavior even though their source now lives elsewhere.

### Request flow
Every external request enters through **Conductor** (`:8080`), the API gateway. Conductor polls **Registry** (`:8082`) every ~5 minutes for service manifests that define routes, actions, and RBAC resources. Conductor verifies permissions with **Gatekeeper** (`:8081`) before forwarding each request to the target backend.

```
Client → Conductor → [Gatekeeper: CheckPermissions] → Backend service
```

Two forwarding modes are controlled by `forward_auth` in the registry manifest:
- `forward_auth: true` (Gatekeeper itself): raw `Authorization: Bearer` header is forwarded
- `forward_auth: false` (all other services): bearer token is stripped; conductor injects `X-User-ID`, `X-Conductor-Token`, and `X-Conductor-Timestamp` headers so backends can verify the request came through conductor

### Service manifests
`infra/local/registry-manifest.json` is the source of truth for what routes the **core** services expose, what RBAC action/resource pairs they map to, and what permissions are granted by default on startup (`default_grants`). The Helm equivalent populates this at deploy time. The spun-off services are **not** in the manifest — **builder** registers them with the registry at runtime when an admin enables them (it PUTs their endpoints/grants from its embedded defs, then notifies conductor). A new core service must be registered in this manifest; conductor will not route to it otherwise.

### Service registration / key rotation
Every backend service calls `registry.StartKeyRotation(ctx, gatekeeperURL, "<service-name>", secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)` on startup (from the `codearmory_sdk`). This registers the service with Gatekeeper and rotates the shared key every 25 minutes. The initial key is set in `GATEKEEPER_SERVICE_KEY` and must match the corresponding entry in Gatekeeper's `GATEKEEPER_SERVICES` env var.

### Database pattern
All GORM-based services in this repo (`gatekeeper`, `hooks`, `workflows`) — and the spun-off `tickets`/`gitea_integration`/`containers` — use the same pattern:
- A `db` interface with `Add / Update / Remove / Get / List` methods implemented on each entity struct
- Lazy-initialized `gormDB` / `gormDBRead` singletons via `connect()` / `connectRead()`
- `CREATE TABLE IF NOT EXISTS` auto-migration on startup — no separate migration step

**Critical:** when modifying role permissions in Gatekeeper, always call `role.Update(ctx)` rather than `db.Save(&role)`. `db.Save` bypasses the Redis permission cache invalidation. Similarly, never initialize a GORM struct with a non-zero primary key before calling `First` — GORM adds the PK as an extra WHERE clause, causing silent misses.

### Secret reading
All services use the pattern `secret("NAME")` which checks `${NAME}_FILE` first (for k8s volume-mounted secrets), then falls back to the env var `NAME`.

### Go workspace
`src/systems/go.work` covers the in-repo backend services as a single workspace (including `outpost-gateway` and the `outpost` agent; spun-off services live in their own repos and are not in this workspace). The CLI (`src/cli/`) is a separate module. Run `go work sync` from `src/systems/` when adding new dependencies shared across services.

### Workflows execution model
The Workflows service (`:8085`) executes pipelines by grouping steps into sequential/parallel batches and making authenticated HTTP calls to the target service for each step. Steps can target any service registered in the registry — forge, the spun-off services builder deploys (e.g. blueprints), or custom services. Runs are tracked in Postgres; the worker polls for pending runs and processes them, recovering stuck runs on restart.

### Forge and egress isolation
Forge (`:8083`) runs user commands as isolated containers (Docker or Kubernetes Jobs) with dropped capabilities. The **Egress Proxy** (`src/systems/egress-proxy`, `:3128`) governs exec traffic via `PROXY_ALLOWED_DOMAINS`, which **defaults to the sentinel `*` (public-only mode)** — any public host is allowed, while the always-on dial-time IP guard still blocks loopback/private/link-local/cloud-metadata addresses, so runners reach the public internet but never internal services. Set it to a comma-separated domain allowlist to restrict further (empty = block all). Forge and workflows are **core** services (deployed by the Helm chart, registered via the registry manifest, and never managed by builder — they are in builder's `coreServices` set with no `files/services/*.json` def). The proxy is **enabled by default**: the Helm chart deploys it alongside forge (`forge.egressProxy.enabled`, default true). In Docker mode exec containers join the `forge-exec` internal network and route through it; in Kubernetes mode a NetworkPolicy restricts exec pods to it (plus DNS). It is skipped for runtimes that isolate egress themselves — set `EGRESS_PROXY_ENABLED=false` in forge's config, or select **kata**: `forge.kata.enabled=true` in the chart (which sets `RUNTIME=kata`) runs sandbox jobs in Cloud Hypervisor microVMs that filter egress at the VM level, so the chart skips the proxy + NetworkPolicy, leaves `FORGE_EGRESS_PROXY` unset, and forge seeds its default runner classes **privileged** (root + writable rootfs, safe behind the VM boundary). `forge.kata.enabled` requires `forge.env.k8sRuntimeClass` to name the VM-isolating RuntimeClass. The **gvisor** backend (`forge.gvisor.enabled` / `RUNTIME=gvisor`, also requires `k8sRuntimeClass`, mutually exclusive with kata) is the no-`/dev/kvm` alternative: gVisor's userspace kernel (the Sentry) is *kernel*-isolated so it likewise seeds privileged runners, but it is **not** a network boundary, so unlike kata it **keeps** the egress proxy + NetworkPolicy. The privileged gate is `isKernelIsolatedBackendType` (kata/proxmox/gvisor), not VM-only.

### Outpost integration framework
Cluster integrations (chaos, argo) never touch a user's cluster from the control plane. A user-deployed **outpost** (`src/systems/outpost/`, the only Kubernetes/CRD code) runs in (or against) each target cluster — the same cluster as the control plane for a single-cluster setup, or one per remote cluster so pipelines can span clusters — and dials out to the **outpost-gateway** (`:8092`) over HTTPS — long-polling a Postgres command queue (`SKIP LOCKED`) and POSTing events into a Postgres outbox that a dispatcher delivers to consumer services (`/internal/events`, shared-key HMAC) with dead-letter retry. Each integration is one outpost **module** + one thin control-plane **consumer service** (`chaos` `:8090`, `argo` `:8091` — now spun off into their own repos and deployed by builder) that holds no cluster credentials. The internal command/event plane is authenticated by `OUTPOST_INTERNAL_KEY` (shared by the gateway and all consumers); outposts authenticate with per-outpost keys (bcrypt). The outpost ships via a separate chart at `infra/helm/outpost/`. Adding an integration touches neither the outpost core nor the gateway. See `docs/outpost/README.md`.

### MCP server
The MCP server (spun off to its own `codearmory-mcp` repo) is a stdio-based MCP server, not an HTTP service — it is not deployed to Kubernetes. Users run it locally via `./codearmory-mcp` with `CODEARMORY_URL` pointing at a conductor endpoint. It wraps the full platform API (workflows, forge, hooks, tickets, containers) as MCP tools.

## Source layout

```
src/
  cli/              armory CLI (cobra, separate Go module)
  systems/          (core services only — see "Repository scope")
    conductor/      API gateway — routing, auth forwarding, key rotation
    gatekeeper/     Auth, RBAC, orgs, teams, roles, sessions, OIDC provider
    registry/       Service manifest store — routes, actions, default grants
    builder/        Org control plane + runtime deployer/registrar of non-core services
    forge/          Sandboxed execution (Docker + Kubernetes runtimes)
    egress-proxy/   Allowlist-enforcing HTTP/CONNECT proxy for forge sandbox egress
    workflows/      Pipeline orchestrator — steps, runs, worker
    hooks/          Webhook receiver — rules, event matching, trigger
    outpost-gateway/ Outpost-facing connection point + Postgres event backbone
    outpost/        User-deployed in-cluster agent (only K8s code)
    portal/         Web app — React SPA + Express BFF (Node, not Go); proxies /api to conductor
infra/
  local/            Docker Compose stack for local development
    registry-manifest.json   Service route/action/RBAC definitions
  helm/codearmory/  Production Helm chart (core services; builder deploys the rest)
  helm/outpost/     User-installable chart for the outpost agent
tests/              Python integration tests (pytest) per service
docs/               Per-service READMEs and platform guide
```

**Spun off** into their own `codearmory-<svc>` repos (deployed + registered by builder at runtime, not in this tree): argo, blueprints, chaos, containers, gitea_integration, mcp, notifications, tickets.
