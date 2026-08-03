# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

### Unit tests
Each service is tested independently. The Go workspace is at `src/systems/`; the CLI is a separate module at `src/cli/`.

```bash
cd src/systems/<service> && go test .      # e.g. gatekeeper, workflows, events
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
This monorepo holds the **core control plane**: conductor, gatekeeper, registry, builder, portal, workflows, forge, **git_connector** (the credential broker), **git_factory** (the platform's own git host), **events** (the event collector/reactor, which absorbed the former `hooks` service), the forge **egress-proxy**, and the outpost pair (`outpost` + `outpost-gateway`). The **git_connector** service (registered/routed as `git_connector`; in-repo directory is still `src/systems/git`) is the core git integration — a backend-agnostic **credential broker** that mints short-lived clone credentials for whatever backend a repo lives on (GitHub, GitLab, Forgejo, generic). **git_factory** (`src/systems/git-factory`, port `9002`) is the git plane itself — repositories plus Smart-HTTP — and is one of git_connector's backends. Mind the naming: its directory and Go module are `git-factory`, but its **service identity is `codearmory_git_factory`** — the name it registers under, the first segment of every RBAC resource (`codearmory_git_factory/repos`), and the key every grant is written against. Never rename the identity; doing so invalidates existing grants. **tickets**, **containers (docker registry)** and **git_factory** are also **core** and **in-repo** (`src/systems/tickets`, `src/systems/containers`, `src/systems/git-factory`) — built by the monorepo CI, the Helm chart deploys those images and they are registered via the registry manifest (they have no builder def). The remaining services were spun out into their own `codearmory-<svc>` repos — **argo, blueprints, chaos, mcp, notifications**, and **gitea_integration** (Forgejo/Gitea repo management — demoted from core because most users do not run Forgejo) — but are still part of the platform: **builder** deploys and registers them at runtime from the images those repos publish (their deploy defs stay in `src/systems/builder/files/services/*.json`). References to these services below describe platform behavior even though their source now lives elsewhere.

### Request flow
Every external request enters through **Conductor** (`:8080`), the API gateway. Conductor polls **Registry** (`:8082`) every ~5 minutes for service manifests that define routes, actions, and RBAC resources. Conductor verifies permissions with **Gatekeeper** (`:8081`) before forwarding each request to the target backend.

```
Client → Conductor → [Gatekeeper: CheckPermissions] → Backend service
```

Forwarding is controlled by `forward_auth` in the registry manifest:
- `forward_auth: true` — the raw `Authorization: Bearer` header is forwarded, so the backend can re-verify the caller with gatekeeper itself. **Every service in both manifests sets this today**, and every backend does exactly that (directly, or via the shared SDK's `gatekeeper.Check`). Conductor's permission check is therefore a first gate, not the only one.
- `forward_auth: false` — the bearer is stripped and conductor injects `X-User-ID` plus `X-Conductor-Token`/`X-Conductor-Timestamp` (HMAC over `conductor:<user_id>:<ts>`, keyed by `CONDUCTOR_FORWARD_KEY`) so a backend can confirm the identity came from conductor. **No service currently uses this mode, and no backend verifies those headers** — `signForwardedUserID` is live code with no consumer. Treat it as unimplemented rather than as an available option: a service switched to `forward_auth: false` today would receive an identity header nothing checks.

The trade-off is worth naming, because forwarding the bearer everywhere is the weaker half of this design: every backend receives a credential it could replay against any other service. That is the cost of letting each service authorise independently, and it is why `X-Conductor-Token` exists — the unfinished half of it.

### Service manifests
`infra/local/registry-manifest.json` is the source of truth for what routes the **core** services expose, what RBAC action/resource pairs they map to, and what permissions are granted by default on startup (`default_grants`). The Helm equivalent populates this at deploy time. It also carries the in-repo core pair (tickets, containers). The **builder-deployed** services (argo, blueprints, chaos, mcp, notifications, gitea_integration) are **not** in the Helm manifest — **builder** registers them with the registry at runtime when an admin enables them (it PUTs their endpoints/grants from its embedded defs, then notifies conductor). (For local-dev convenience the `infra/local` manifest may still list some builder-deployed services, e.g. `gitea_integration`, since compose does not run builder's reconciler.) A new core service must be registered in this manifest; conductor will not route to it otherwise.

### RBAC resource scoping (get this wrong and the check silently passes)
Gatekeeper **rewrites** the requested resource before matching: an *unscoped* resource gets the **caller's username** prefixed (`scopeResource` in `api_helpers.go`). So `forge/executions` asked for by `alice` is matched as `alice/forge/executions`, which is why default grants are templated `{username}/…`. A resource is left alone when it already names an owner: `<username>/<service>/…`, `org/<orgName>/<service>/…`, `project/<slug>/<service>/…`, or `codearmory/<service>/…`.

**`codearmory/` is the platform namespace** — instance-level config owned by no user, org or project (forge runner classes/images/runtime-backends/concurrency-limits, gatekeeper OIDC + audit logs, containers registries, `codearmory/builder/orgs/default`, tickets' global field defs). `codearmory`, `org` and `project` are **reserved usernames**, rejected in gatekeeper's `User.Add`/`User.Update`, and `codearmory` is a real non-loginable account so platform-config changes attribute to a principal that resolves.

Two traps this exists to prevent:
- **Declaring platform config unscoped.** It then gets caller-prefixed on *both* sides — grant and check — so it matches by symmetry rather than ownership: global data modelled as though everyone had a private copy. Admin-only endpoints merely look fine because the admin wildcard matches anything.
- **Templating a per-record resource.** Conductor can only template from **path params**, so `tickets/tickets/{id}` evaluates to `<caller>/tickets/tickets/<id>` — identical for every id, meaning gatekeeper authorises *any* id and only the service's own owner filter protects the row. Prefer the honest **collection** resource in the manifest and make the per-record decision in the service with the row loaded (git_factory is the reference implementation: `resRepoOf` builds `<owner-ns>/codearmory_git_factory/repos/<id>`, handlers load before checking, and a denial is rewritten to 404 so existence does not leak).

Note a resource **leading with the service name** is always unscoped: `tickets/tickets` would otherwise parse as owner `tickets` and the caller's name would never be applied, silently denying every user their own tickets.

### Service registration / key rotation
Every backend service calls `registry.StartKeyRotation(ctx, gatekeeperURL, "<service-name>", secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)` on startup (from the in-repo shared SDK at `src/systems/sdk`). This registers the service with Gatekeeper and rotates the shared key every 25 minutes. The initial key is set in `GATEKEEPER_SERVICE_KEY` and must match the corresponding entry in Gatekeeper's `GATEKEEPER_SERVICES` env var.

### Database pattern
All GORM-based services in this repo (`gatekeeper`, `events`, `workflows`, `git`, `tickets`, `containers`) — plus the builder-deployed `gitea_integration` — use the same pattern:
- A `db` interface with `Add / Update / Remove / Get / List` methods implemented on each entity struct
- Lazy-initialized `gormDB` / `gormDBRead` singletons via `connect()` / `connectRead()`
- `CREATE TABLE IF NOT EXISTS` auto-migration on startup — no separate migration step

**Critical:** when modifying role permissions in Gatekeeper, always call `role.Update(ctx)` rather than `db.Save(&role)`. `db.Save` bypasses the Redis permission cache invalidation. Similarly, never initialize a GORM struct with a non-zero primary key before calling `First` — GORM adds the PK as an extra WHERE clause, causing silent misses.

### Secret reading
All services use the pattern `secret("NAME")` which checks `${NAME}_FILE` first (for k8s volume-mounted secrets), then falls back to the env var `NAME`.

### Go workspace
`src/systems/go.work` covers the in-repo backend services as a single workspace (including `outpost-gateway`, the `outpost` agent, and the shared `sdk` module; spun-off services live in their own repos and are not in this workspace). The CLI (`src/cli/`) is a separate module.

**Do not run `go work sync`.** It rewrites each module's `go.mod` to the workspace-wide build list but leaves the corresponding hashes in `go.work.sum` rather than the module's own `go.sum`. Workspace builds keep working, so it looks harmless — but Docker builds run in module mode (`GOWORK=off`, no `go.work` in the build context) and fail with `missing go.sum entry`. If you need to align a dependency, change it in the module and run `GOWORK=off go mod tidy` there, which is what keeps each `go.sum` self-sufficient. Verify with `GOWORK=off go build ./...`, since a plain `go build` will not catch it.

### Shared SDK
`src/systems/sdk` (module path `github.com/code-armory-app/codearmory_sdk`) holds the code every service shares: gatekeeper permission checks and audit ingest, service-key rotation, and OTel telemetry setup. It lives in this repo and every service `replace`s it to `../sdk`, so a change lands everywhere at once with no publish-and-bump cycle.

That replace is why service Dockerfiles build with **`src/systems` as the context** rather than the service directory — the build needs both the service tree and `sdk/`. They also set `GOWORK=off`, since `go.work` is deliberately not copied (workspace mode would demand every member module be present). `Dockerfile.ci` is unaffected: it only copies a binary the CI test step already built.

### Workflows execution model
The Workflows service (`:8085`) executes pipelines by grouping steps into sequential/parallel batches and making authenticated HTTP calls to the target service for each step. Steps can target any service registered in the registry — forge, the spun-off services builder deploys (e.g. blueprints), or custom services. Runs are tracked in Postgres; the worker polls for pending runs and processes them, recovering stuck runs on restart.

### Forge and egress isolation
Forge (`:8083`) runs user commands as isolated containers (Docker or Kubernetes Jobs) with dropped capabilities. The **Egress Proxy** (`src/systems/egress-proxy`, `:3128`) governs exec traffic via `PROXY_ALLOWED_DOMAINS`, which **defaults to the sentinel `*` (public-only mode)** — any public host is allowed, while the always-on dial-time IP guard still blocks loopback/private/link-local/cloud-metadata addresses, so runners reach the public internet but never internal services. Set it to a comma-separated domain allowlist to restrict further (empty = block all). Forge and workflows are **core** services (deployed by the Helm chart, registered via the registry manifest, and never managed by builder — they are in builder's `coreServices` set with no `files/services/*.json` def). The proxy is **enabled by default**: the Helm chart deploys it alongside forge (`forge.egressProxy.enabled`, default true). In Docker mode exec containers join the `forge-exec` internal network and route through it; in Kubernetes mode a NetworkPolicy restricts exec pods to it (plus DNS). It is skipped for runtimes that isolate egress themselves — set `EGRESS_PROXY_ENABLED=false` in forge's config, or select **kata**: `forge.kata.enabled=true` in the chart (which sets `RUNTIME=kata`) runs sandbox jobs in Cloud Hypervisor microVMs, so the chart skips the egress *proxy* (leaves `FORGE_EGRESS_PROXY` unset — runners aren't forced through it) but still applies a **public-only egress NetworkPolicy** to exec pods (`forge.kata.egress`, default on) that allows all public IPs and blocks every private/internal range (RFC1918, loopback, link-local/cloud-metadata `169.254/16`, CGNAT `100.64/10`) plus a configurable `blocklist`, since a kata microVM is a kernel/VM boundary, **not** a network one. forge seeds its default runner classes **privileged** (root + writable rootfs, safe behind the VM boundary). `forge.kata.enabled` requires `forge.env.k8sRuntimeClass` to name the VM-isolating RuntimeClass. The **gvisor** backend (`forge.gvisor.enabled` / `RUNTIME=gvisor`, also requires `k8sRuntimeClass`, mutually exclusive with kata) is the no-`/dev/kvm` alternative: gVisor's userspace kernel (the Sentry) is *kernel*-isolated so it likewise seeds privileged runners, but it is **not** a network boundary, so unlike kata it **keeps** the egress proxy + NetworkPolicy. The privileged gate is `isKernelIsolatedBackendType` (kata/gvisor), not VM-only.

### Outpost integration framework
Cluster integrations (chaos, argo) never touch a user's cluster from the control plane. A user-deployed **outpost** (`src/systems/outpost/`, the only Kubernetes/CRD code) runs in (or against) each target cluster — the same cluster as the control plane for a single-cluster setup, or one per remote cluster so pipelines can span clusters — and dials out to the **outpost-gateway** (`:8092`) over HTTPS — long-polling a Postgres command queue (`SKIP LOCKED`) and POSTing events into a Postgres outbox that a dispatcher delivers to consumer services (`/internal/events`, shared-key HMAC) with dead-letter retry. Each integration is one outpost **module** + one thin control-plane **consumer service** (`chaos` `:8090`, `argo` `:8091` — now spun off into their own repos and deployed by builder) that holds no cluster credentials. The internal command/event plane is authenticated by `OUTPOST_INTERNAL_KEY` (shared by the gateway and all consumers); outposts authenticate with per-outpost keys (bcrypt). The outpost ships via a separate chart at `infra/helm/outpost/`. Adding an integration touches neither the outpost core nor the gateway. See `docs/outpost/README.md`.

### MCP server
The MCP server (spun off to its own `codearmory-mcp` repo) is a stdio-based MCP server, not an HTTP service — it is not deployed to Kubernetes. Users run it locally via `./codearmory-mcp` with `CODEARMORY_URL` pointing at a conductor endpoint. It wraps the full platform API (workflows, forge, events, tickets, containers) as MCP tools.

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
    git/            Git credential broker — short-lived clone creds for GitHub/GitLab/Forgejo/generic backends
    git-factory/    Platform git host — repos + Smart-HTTP (module git-factory, registered as codearmory_git_factory)
    egress-proxy/   Allowlist-enforcing HTTP/CONNECT proxy for forge sandbox egress
    workflows/      Pipeline orchestrator — steps, runs, worker
    events/         Event collector + reactor — envelope intake, field-filtered triggers, actions, webhook adapters
    tickets/        Issue/ticket tracker — boards, tickets, comments, custom fields
    containers/     Docker registry proxy — per-tenant image repositories
    outpost-gateway/ Outpost-facing connection point + Postgres event backbone
    outpost/        User-deployed in-cluster agent (only K8s code)
    portal/         Web app — React SPA + Express BFF (Node, not Go); proxies /api to conductor
    sdk/            Shared library every service replaces to ../sdk — gatekeeper checks + audit, key rotation, telemetry
infra/
  local/            Docker Compose stack for local development
    registry-manifest.json   Service route/action/RBAC definitions
  helm/codearmory/  Production Helm chart (core services; builder deploys the rest)
  helm/outpost/     User-installable chart for the outpost agent
tests/              Python integration tests (pytest) per service
docs/               Per-service READMEs and platform guide
```

**Spun off** into their own `codearmory-<svc>` repos (source not in this tree): argo, blueprints, chaos, mcp, notifications, and gitea_integration are deployed + registered by **builder** at runtime. **tickets**, **containers** and **git_factory** are **in-repo core** (`src/systems/tickets`, `src/systems/containers`, `src/systems/git-factory`; built by the monorepo CI, Helm-deployed, registered via the registry manifest, no builder def) — they were moved back into the monorepo after being spun off. git_factory's directory and Go module are `git-factory`, but it is registered and routed as **`codearmory_git_factory`** — the module name is not the service identity. The core git integration is the in-repo **git_connector** credential broker (directory `src/systems/git`), not gitea_integration.
