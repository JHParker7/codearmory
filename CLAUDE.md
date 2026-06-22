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

### Request flow
Every external request enters through **Conductor** (`:8080`), the API gateway. Conductor polls **Registry** (`:8082`) every ~5 minutes for service manifests that define routes, actions, and RBAC resources. Conductor verifies permissions with **Gatekeeper** (`:8081`) before forwarding each request to the target backend.

```
Client → Conductor → [Gatekeeper: CheckPermissions] → Backend service
```

Two forwarding modes are controlled by `forward_auth` in the registry manifest:
- `forward_auth: true` (Gatekeeper itself): raw `Authorization: Bearer` header is forwarded
- `forward_auth: false` (all other services): bearer token is stripped; conductor injects `X-User-ID`, `X-Conductor-Token`, and `X-Conductor-Timestamp` headers so backends can verify the request came through conductor

### Service manifests
`infra/local/registry-manifest.json` is the source of truth for what routes each service exposes, what RBAC action/resource pairs they map to, and what permissions are granted by default on startup (`default_grants`). The Helm equivalent populates this at deploy time. Any new service must be registered here; conductor will not route to it otherwise.

### Service registration / key rotation
Every backend service calls `registry.StartKeyRotation(ctx, gatekeeperURL, "<service-name>", secret("GATEKEEPER_SERVICE_KEY"), 25*time.Minute)` on startup (from the `codearmory_sdk`). This registers the service with Gatekeeper and rotates the shared key every 25 minutes. The initial key is set in `GATEKEEPER_SERVICE_KEY` and must match the corresponding entry in Gatekeeper's `GATEKEEPER_SERVICES` env var.

### Database pattern
All GORM-based services (`gatekeeper`, `hooks`, `tickets`, `workflows`, `gitea`, `containers`) use the same pattern:
- A `db` interface with `Add / Update / Remove / Get / List` methods implemented on each entity struct
- Lazy-initialized `gormDB` / `gormDBRead` singletons via `connect()` / `connectRead()`
- `CREATE TABLE IF NOT EXISTS` auto-migration on startup — no separate migration step

**Critical:** when modifying role permissions in Gatekeeper, always call `role.Update(ctx)` rather than `db.Save(&role)`. `db.Save` bypasses the Redis permission cache invalidation. Similarly, never initialize a GORM struct with a non-zero primary key before calling `First` — GORM adds the PK as an extra WHERE clause, causing silent misses.

### Secret reading
All services use the pattern `secret("NAME")` which checks `${NAME}_FILE` first (for k8s volume-mounted secrets), then falls back to the env var `NAME`.

### Go workspace
`src/systems/go.work` covers all backend services as a single workspace (including `chaos`, `argo`, `outpost-gateway`, and the `outpost` agent). The CLI (`src/cli/`) is a separate module. Run `go work sync` from `src/systems/` when adding new dependencies shared across services.

### Workflows execution model
The Workflows service (`:8085`) executes pipelines by grouping steps into sequential/parallel batches and making authenticated HTTP calls to the target service for each step. Steps can target any service registered in the registry — forge, blueprints, or custom services. Runs are tracked in Postgres; the worker polls for pending runs and processes them, recovering stuck runs on restart.

### Forge and egress isolation
Forge (`:8083`) runs user commands as isolated containers (Docker or Kubernetes Jobs) with dropped capabilities. In Docker mode, execution containers are placed on the `forge-exec` internal network and route all outbound traffic through the Egress Proxy (`:3128`), which enforces a domain allowlist via `PROXY_ALLOWED_DOMAINS`. In Kubernetes mode, use NetworkPolicy for equivalent isolation.

### Outpost integration framework
Cluster integrations (chaos, argo) never touch a customer cluster from the control plane. A single customer-deployed **outpost** (`src/systems/outpost/`, the only Kubernetes/CRD code) runs in the target cluster and dials out to the **outpost-gateway** (`:8092`) over HTTPS — long-polling a Postgres command queue (`SKIP LOCKED`) and POSTing events into a Postgres outbox that a dispatcher delivers to consumer services (`/internal/events`, shared-key HMAC) with dead-letter retry. Each integration is one outpost **module** + one thin control-plane **consumer service** (`chaos` `:8090`, `argo` `:8091`) that holds no cluster credentials. The internal command/event plane is authenticated by `OUTPOST_INTERNAL_KEY` (shared by the gateway and all consumers); outposts authenticate with per-outpost keys (bcrypt). The outpost ships via a separate chart at `infra/helm/outpost/`. Adding an integration touches neither the outpost core nor the gateway. See `docs/outpost/README.md`.

### MCP server
`src/systems/mcp/` is a stdio-based MCP server, not an HTTP service — it is not deployed to Kubernetes. Users run it locally via `./codearmory-mcp` with `CODEARMORY_URL` pointing at a conductor endpoint. It wraps the full platform API (workflows, forge, hooks, tickets, containers) as MCP tools.

## Source layout

```
src/
  cli/              armory CLI (cobra, separate Go module)
  systems/
    conductor/      API gateway — routing, auth forwarding, key rotation
    gatekeeper/     Auth, RBAC, orgs, teams, roles, sessions, OIDC provider
    registry/       Service manifest store — routes, actions, default grants
    blueprints/     Terraform/OpenTofu HTTP state backend
    forge/          Sandboxed execution (Docker + Kubernetes runtimes)
    workflows/      Pipeline orchestrator — steps, runs, worker
    hooks/          Webhook receiver — rules, event matching, trigger
    tickets/        Task tracker linked to runs and executions
    containers/     OCI registry management proxy
    egress-proxy/   Allowlist-enforcing HTTP CONNECT proxy for forge
    gitea/          Forgejo/Gitea integration — repos, PRs, git proxy
    outpost-gateway/ Outpost-facing connection point + Postgres event backbone
    chaos/          Chaos-engineering control plane (outpost integration)
    argo/           Argo CD sync control plane (outpost integration)
    outpost/        Customer-deployed in-cluster agent (chaos/argo modules; only K8s code)
    mcp/            stdio MCP server (not a deployed service)
    portal/         Web app — React SPA + Express BFF (Node, not Go); proxies /api to conductor
infra/
  local/            Docker Compose stack for local development
    registry-manifest.json   Service route/action/RBAC definitions
  helm/codearmory/  Production Helm chart
  helm/outpost/     Customer-installable chart for the outpost agent
tests/              Python integration tests (pytest) per service
docs/               Per-service READMEs and platform guide
```
