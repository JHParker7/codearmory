# CodeArmory

[![CI](https://github.com/code-armory-app/codearmory/actions/workflows/test_build_release.yml/badge.svg)](https://github.com/code-armory-app/codearmory/actions/workflows/unit_tests.yml)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPL%20v3-blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8)](https://go.dev)
[![Status: Alpha](https://img.shields.io/badge/Status-Alpha-orange)](../../releases)

**A modular, self-hosted CI/CD platform — pipelines that run sandboxed jobs, triggered by platform events and git webhooks, behind one auth/RBAC layer, with a builder that brings new services online as modules and a CLI that keeps you in your terminal.**

---

## What's inside

- **CI/CD pipelines** — sequence steps into runs as sequential/parallel batches; run inputs and prior-step outputs are substituted at execution time, and stuck runs are recovered on restart (Workflows).
- **Sandboxed runners** — run commands in isolated containers (Docker or Kubernetes Jobs) with dropped capabilities, configurable resource tiers (runner classes), and pluggable runtime backends — including kata/Cloud-Hypervisor VMs and gVisor userspace-kernel sandboxes (no `/dev/kvm` needed) — with optional egress allowlisting (Forge).
- **Self-hosted git, built in** — host your repositories on the platform itself: bare repos over Smart HTTP with gatekeeper-backed auth, collaborators, branch protection, pull requests, and pull-through mirrors of upstream repos so CI clones stay in-cluster (git_factory). It is a **core service** — the chart deploys it and the registry manifest registers it, so there is nothing to enable. Repos on GitHub, GitLab or Forgejo keep working through the credential broker (git_connector), so this is an option rather than a migration.
- **Issue tracking** — boards, tickets, comments and custom fields, linkable to pipeline runs and sandboxed executions, so work items and the builds that address them live in one place (Tickets).
- **Container registry** — per-tenant image repositories fronted by a registry proxy (Containers), plus a build-artifact store (Artifacts).
- **Event-driven triggers** — receive pushes and PRs from GitHub, GitLab, or Forgejo/Gitea, or any platform event, and map them to pipeline runs with at-least-once delivery (Events).
- **Auth + RBAC + SSO** — ES256 JWT sessions, orgs, teams, and roles covering every service. Gatekeeper is also an OIDC provider, so it can be your SSO identity source.
- **Unified API gateway** — every request enters through Conductor, which routes by service prefix and verifies permissions with Gatekeeper before forwarding. Services declare their routes, actions, and RBAC in the Registry.
- **Modular by design** — Builder is the per-org control plane *and* the runtime deployer: enable a service for an org and Builder deploys + registers it at runtime, no chart edit. New capabilities ship as **modules** in their own repos, not as forks of the core.
- **Cluster integrations + cross-cluster pipelines** — drive your own clusters from the control plane through **outposts** that dial out over HTTPS (no inbound access, no control-plane cluster credentials). One outpost per cluster means **pipelines unify across clusters** — a single run can act on your whole fleet, gating on the results. Run an outpost in the *same* cluster as CodeArmory for a single-cluster setup, or one per remote cluster to fan out (Outpost + Outpost Gateway).
- **CLI-first** — every platform operation is available from `armory`. Create pipelines, trigger runs, manage runners, inspect logs — without opening a browser.

---

## Why CodeArmory?

Most CI/CD stacks are monolithic: the runner, the trigger system, the auth model, and the API are all baked into one tool, and adding a capability means forking it or bolting on a second system with its own login.

CodeArmory is a **platform, not a monolith**. Every capability — pipelines, sandboxed runners, webhook triggers, cluster integrations — is a service registered behind one gateway and one RBAC model. Adding a new capability means registering an HTTP service (and letting **Builder** deploy it), not patching the core. Write a small service with the [CodeArmory SDK](https://github.com/code-armory-app/codearmory_sdk), register it, and it immediately becomes a first-class pipeline target with auth, routing, and RBAC handled for you.

---

## Architecture

Every request enters through Conductor, which polls Registry for service manifests and verifies permissions with Gatekeeper before forwarding. Backend services delegate auth to Gatekeeper, so permission logic stays in one place. The core ships only the control plane — **Builder** deploys and registers everything else at runtime, so additional capabilities are modules it brings online rather than code baked into the core.

```
 Browser / CLI / Git client
          │
          ▼
    ┌─────────────┐         ┌─────────────┐
    │  Conductor  │ ──────► │  Gatekeeper │  :8081 — auth + RBAC, OIDC/SSO
    │   :8080     │         └─────────────┘
    │ API gateway │
    └──────┬──────┘
           │  polls for routes
           ▼
    ┌─────────────┐
    │  Registry   │  :8082 — service manifests
    └─────────────┘

    Core services Conductor routes to:

    ┌─────────────┐   ┌─────────────┐   ┌─────────────┐
    │  Workflows  │   │    Forge    │   │   Events    │
    │   :8085     │   │   :8083     │   │   :8093     │
    │  pipelines  │   │   runners   │   │  triggers   │
    └─────────────┘   └─────────────┘   └─────────────┘

    ┌─────────────┐   ┌─────────────┐
    │ git_factory │   │git_connector│
    │   :9002     │   │   :8096     │
    │  git host   │   │ clone creds │
    └─────────────┘   └─────────────┘

    ┌─────────────┐   ┌───────────────────────────────┐
    │   Builder   │   │  Portal — React SPA + BFF      │
    │   :8095     │   │  web UI, proxies /api          │
    │ org control │   └───────────────────────────────┘
    │ + deployer  │  ← enables & deploys modules at runtime
    └─────────────┘

    Cluster integrations — outposts dial out, no inbound access.
    One per cluster, so pipelines unify across your whole fleet:

    ┌──────────────────┐
    │ Outpost Gateway  │  :8092 — enroll / commands / events backbone
    └────────┬─────────┘  commands ▲ / events ▼ (HTTPS)
    ┌────────┴─────────┐
    │     Outpost      │  ← in each target cluster, or alongside
    └──────────────────┘     CodeArmory itself for a single-cluster setup
```

---

## Quick start

```bash
git clone https://github.com/code-armory-app/codearmory
cd codearmory/infra/local
export DOCKER_GID=$(stat -c '%g' /var/run/docker.sock)
docker compose up --build
```

Starts PostgreSQL, Redis, and the core services. API gateway at `http://localhost:8080`.

```bash
# Sign up and get a token
armory auth login --url http://localhost:8080 --email you@example.com

# Trigger a sandboxed run from the terminal
armory forge exec run --image alpine:3.19 -- echo hello

# Create a pipeline from existing steps and run it
armory pipelines create pipeline myrepo main "build->test->deploy"
armory pipelines run pipeline <pipeline-id> --input ENV=staging --input VERSION=v1.2
```

**Helm (production):**

```bash
helm install codearmory ./infra/helm/codearmory \
  --set global.domain=armory.example.com \
  --set global.postgresUrl=postgresql://...
```

The chart ships the core control plane; Builder deploys additional service modules at runtime when an org enables them.

---

## Extending the platform with services

Conductor routes to every service listed in the **Registry**. To add a new pipeline
target — an internal deploy tool, a custom API, anything that speaks HTTP — register
it with its routes, any catalog **actions** pipelines can call, and the permissions
granted by default. Core services are seeded from the registry manifest
(`infra/local/registry-manifest.json`, or the Helm equivalent); additional services
are registered at runtime by **Builder** when an org enables them.

```json
{
  "name": "deployer",
  "url": "http://my-deployer:9000",
  "forward_auth": false,
  "service_key": "deployer-registry-secret",
  "endpoints": [
    { "method": "POST", "path": "/deploy", "action": "deploy", "resource": "deployer/deploy" }
  ],
  "actions": [
    { "name": "deployer/deploy", "method": "POST", "path": "/deploy" }
  ],
  "default_grants": [
    { "grant_on": "user", "actions": ["deploy"], "resources": ["{username}/deployer/deploy"] }
  ]
}
```

Once registered, reference the action from a reusable step and drop it into a
pipeline — `${...}` resolves run inputs and prior step outputs at execution time:

```bash
armory pipelines create step run-deployer \
  --action deployer/deploy \
  --with '{"env":"${ENV}","version":"${VERSION}"}'

armory pipelines create pipeline myrepo main "build->test->run-deployer"
armory pipelines run pipeline <pipeline-id> --input ENV=staging --input VERSION=v1.2
```

No code changes to your existing service. With `forward_auth: false` (the default),
Conductor strips the caller's bearer token and injects `X-User-ID` plus signed
`X-Conductor-*` headers, so your service can trust requests arrived through the
gateway. Run `armory pipelines list actions` to see every action the catalog exposes.

---

## CLI — stay in your terminal

The `armory` CLI covers the full platform. You never need to open a browser.

```bash
armory auth login                                    # authenticate
armory pipelines list pipelines                      # see all pipelines
armory pipelines run pipeline <id> --input KEY=VALUE # trigger a run
armory pipelines get run <id>                        # inspect results
armory forge exec run --image node:20 -- npm test    # sandboxed run
armory events triggers create --name ci --match type=repo.push --match subject=myorg/myapp --run-pipeline <id>
armory admin orgs invite <org> colleague@example.com
```

Binaries for Linux, macOS, and Windows are attached to each [GitHub release](../../releases).

---

## Services

| Service | Port | Docs |
|---------|------|------|
| Conductor | 8080 | [API gateway](docs/conductor/README.md) |
| Gatekeeper | 8081 | [Auth + RBAC + OIDC](docs/gatekeeper/README.md) |
| Registry | 8082 | [Service discovery](docs/registry/README.md) |
| Forge | 8083 | [Sandboxed execution](docs/forge/README.md) |
| Egress Proxy | 3128 | [Sandbox egress control](docs/egress-proxy/README.md) |
| Workflows | 8085 | [Pipeline orchestration](docs/workflows/README.md) |
| Tickets | 8086 | Issue tracker — boards, tickets, comments, custom fields |
| Containers | 8089 | Docker registry proxy — per-tenant image repositories |
| Events | 8093 | [Event collector/reactor + webhook adapters](docs/events/README.md) |
| Builder | 8095 | [Org control plane + runtime service deployer](docs/builder/README.md) |
| git_connector | 8096 | [Git credential broker](docs/git/README.md) |
| Artifacts | 8097 | Build artifact store |
| git_factory | 9002 | [The platform's own git host](docs/git-factory/README.md) — registers and routes as `codearmory_git_factory` |
| Outpost Gateway | 8092 | [Cluster integration backbone](docs/outpost-gateway/README.md) |
| Outpost | — | [User-deployed cluster agent](docs/outpost/README.md) |
| Portal | — | Web UI (React SPA + Express BFF) |
| Armory CLI | — | [Command reference](docs/cli/README.md) |

Everything above is **core** — it lives in this repo and none of it is enabled through Builder. The services are deployed by the Helm chart and registered from the registry manifest; the two exceptions are the Outpost, which ships in its own chart for the cluster you point it at, and the CLI, which is a binary you install. Additional capabilities ship as **modules** in their own `codearmory-*` repos and are deployed at runtime by Builder. Full platform guide: [docs/platform-guide.md](docs/platform-guide.md).

---

## Docker images

Pre-built images are published to GHCR on every release (`alpha-latest` for pre-release):

```
ghcr.io/code-armory-app/conductor:alpha-latest
ghcr.io/code-armory-app/gatekeeper:alpha-latest
ghcr.io/code-armory-app/registry:alpha-latest
ghcr.io/code-armory-app/forge:alpha-latest
ghcr.io/code-armory-app/egress-proxy:alpha-latest
ghcr.io/code-armory-app/workflows:alpha-latest
ghcr.io/code-armory-app/tickets:alpha-latest
ghcr.io/code-armory-app/containers:alpha-latest
ghcr.io/code-armory-app/events:alpha-latest
ghcr.io/code-armory-app/builder:alpha-latest
ghcr.io/code-armory-app/git:alpha-latest
ghcr.io/code-armory-app/artifacts:alpha-latest
ghcr.io/code-armory-app/git-factory:alpha-latest
ghcr.io/code-armory-app/outpost-gateway:alpha-latest
ghcr.io/code-armory-app/outpost:alpha-latest
ghcr.io/code-armory-app/portal:alpha-latest
```

---

## Licence

CodeArmory is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0). You can use it, self-host it, modify it, and distribute it freely. If you run a modified version over a network, you must make the source available to users of that service. See the [LICENSE](LICENSE) file for the full terms.

---

## Community

Questions, feedback, or ideas — use [GitHub Discussions](https://github.com/code-armory-app/codearmory/discussions). For bugs, open an issue.

---

## Contributing

Pull requests are welcome. For anything beyond a small fix, open an issue first so we can agree on direction before you invest time in an implementation.

```bash
# Unit tests
cd src/systems/<service> && go test ./...

# Install commit hooks (Conventional Commits + secret scanning)
pip install pre-commit && pre-commit install --hook-type commit-msg
```

**Integration tests** run against a live stack via Docker. Start the stack first, then run any service's tests individually:

```bash
cd infra/local
export DOCKER_GID=$(stat -c '%g' /var/run/docker.sock)
docker compose up --build -d

docker compose --profile test run --rm gatekeeper-integration-tests
docker compose --profile test run --rm registry-integration-tests
docker compose --profile test run --rm conductor-integration-tests
docker compose --profile test run --rm forge-integration-tests
docker compose --profile test run --rm workflows-integration-tests
docker compose --profile test run --rm events-integration-tests
docker compose --profile test run --rm outpost-gateway-integration-tests
docker compose --profile test run --rm portal-integration-tests
```

---

## Status

CodeArmory is in **alpha**. APIs and data models may change between releases.

---

## Security

To report a vulnerability, use [GitHub's private security advisory form](https://github.com/code-armory-app/codearmory/security/advisories/new) — do not open a public issue. We'll respond within 72 hours.
