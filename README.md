# CodeArmory

[![CI](https://github.com/code-armory-app/codearmory/actions/workflows/test_build_release.yml/badge.svg)](https://github.com/code-armory-app/codearmory/actions/workflows/unit_tests.yml)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPL%20v3-blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8)](https://go.dev)
[![Status: Alpha](https://img.shields.io/badge/Status-Alpha-orange)](../../releases)

**A self-hosted developer platform — remote Terraform state, extensible CI pipelines, Forgejo/Gitea integration, container registry management, and a CLI that keeps you in your terminal.**

---

## What's inside

- **Remote Terraform/OpenTofu state** — drop-in HTTP backend with workspace locking, backed by your own Postgres. Point any existing `tofu` or `terraform` config at it with two lines of config.
- **Extensible pipelines** — register any HTTP service and use it as a pipeline step. Your internal tools, build systems, and deployment scripts become first-class pipeline targets without any code changes.
- **Sandboxed runners** — run commands in isolated containers with dropped capabilities, configurable resource tiers (runner classes), and optional egress control via an allowlist proxy (Forge + Egress Proxy).
- **Git webhook triggers** — receive pushes and PRs from GitHub, GitLab, or Gitea and map them to pipeline runs with at-least-once delivery (Hooks).
- **Auth + RBAC + SSO** — ES256 JWT sessions, orgs, teams, and roles covering every service. Gatekeeper also acts as an OIDC provider so Forgejo can use CodeArmory as its SSO identity source.
- **Forgejo/Gitea integration** — link CodeArmory user accounts to Forgejo identities and manage repos, branches, commits, and pull requests through the platform API (Gitea Integration).
- **Container registry management** — authenticated RBAC-enforced visibility and deletion on top of any OCI registry, plus transparent `docker push`/`pull` proxying (Containers).
- **Task tracker** — tickets linked directly to pipeline runs and forge executions, so deployment tasks and their outcomes live together (Tickets).
- **Cluster integrations via outposts** — run chaos-engineering experiments (Litmus) and drive Argo CD syncs in your own clusters through a single customer-deployed **outpost** that dials out to the control plane. No inbound cluster access and no control-plane cluster credentials; one mechanism for self-hosted and SaaS, with pipelines that gate on experiment verdicts and healthy syncs (Outpost Gateway + Chaos + Argo).
- **CLI-first** — every platform operation is available from `armory`. Create workflows, trigger runs, manage tickets, inspect logs — without opening a browser.
- **MCP server** — expose the full platform API as MCP tools so AI assistants can trigger pipelines, inspect runs, and manage tickets directly.

---

## Why CodeArmory?

The DevOps toolchain is fragmented. GitHub Actions handles CI but has no state management. Terraform Cloud manages state but is separate from your pipelines. Atlantis brings state into CI but doesn't run arbitrary jobs. You end up stitching together multiple tools, multiple auth systems, and multiple places to look when something breaks.

CodeArmory runs state, pipelines, sandboxed runners, webhook triggers, Forgejo integration, container registry management, and task tracking in one place, with one auth layer covering everything. Your Terraform runs, CI jobs, and deployment tickets all live under the same RBAC model, accessed through the same API and the same SSO session.

It's also built to grow with you. Every service in the platform is registered through a common interface — so integrating a new tool means registering an HTTP endpoint, not forking the platform. Write a small service using the [CodeArmory SDK](https://github.com/code-armory-app/codearmory_sdk), register it, and it immediately becomes a first-class pipeline step with auth, routing, and RBAC handled for you.

---

## Architecture

Every request enters through Conductor. Backend services delegate auth to Gatekeeper — permission logic stays in one place across the whole platform.

Non-core services (Blueprints, Tickets, Containers, Gitea Integration, Notifications, Chaos, Argo, the Egress Proxy, and the MCP server) live in their own `codearmory-<svc>` repos and are deployed at runtime by Builder.

```
 Browser / CLI / Terraform / Git client
          │
          ▼
    ┌─────────────┐
    │  Conductor  │  :8080 — API gateway, auth, routing
    └──────┬──────┘
           │  polls for routes
           ▼
    ┌─────────────┐
    │  Registry   │  :8082 — service manifests
    └─────────────┘

    Routes traffic to:

    ┌─────────────┐   ┌─────────────┐   ┌──────────────┐
    │ Gatekeeper  │   │  Blueprints │   │    Forge     │
    │   :8081     │   │   :8093     │   │    :8083     │
    │ auth + RBAC │   │ Tofu state  │   │   runners    │
    │ OIDC/SSO    │   └─────────────┘   └──────┬───────┘
    └─────────────┘                            │ egress
                                               ▼
    ┌─────────────┐   ┌─────────────┐   ┌──────────────┐
    │  Workflows  │   │    Hooks    │   │ Egress Proxy │
    │   :8085     │   │   :8087     │   │    :3128     │
    │  pipelines  │   │  webhooks   │   │  allowlist   │
    └─────────────┘   └─────────────┘   └──────────────┘

    ┌─────────────┐   ┌─────────────────┐   ┌────────────┐
    │   Tickets   │   │Gitea Integration│   │ Containers │
    │   :8086     │   │     :8088       │   │   :8089    │
    │   tasks     │   │ repos + PRs     │   │ OCI proxy  │
    └─────────────┘   └─────────────────┘   └────────────┘

    Cluster integrations — the outpost dials out, no inbound access:

    ┌──────────────────┐   ┌─────────────┐   ┌─────────────┐
    │ Outpost Gateway  │   │    Chaos    │   │    Argo     │
    │     :8092        │   │   :8090     │   │   :8091     │
    │ enroll/commands/ │   │ experiments │   │  app sync   │
    │ events backbone  │   └─────────────┘   └─────────────┘
    └────────┬─────────┘   commands ▲ / events ▼ (HTTPS)
    ┌────────┴─────────┐
    │     Outpost      │  ← in your cluster: Litmus CRDs / Argo CD
    └──────────────────┘
```

---

## Quick start

```bash
git clone https://github.com/code-armory-app/codearmory
cd codearmory/infra/local
export DOCKER_GID=$(stat -c '%g' /var/run/docker.sock)
export CONTAINER_REGISTRY_URL=https://ghcr.io   # required for Containers service
docker compose up --build
```

Starts PostgreSQL, Redis, and all services. API gateway at `http://localhost:8080`.

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

---

## Terraform / OpenTofu state

Blueprints implements the standard [Terraform HTTP backend protocol](https://developer.hashicorp.com/terraform/language/settings/backends/http). Change two lines in your backend config and your state moves to your own Postgres — with workspace locking, soft deletes, and a full audit trail.

```hcl
terraform {
  backend "http" {
    address        = "http://conductor:8080/blueprints/state/alice/my-project"
    lock_address   = "http://conductor:8080/blueprints/state/alice/my-project"
    unlock_address = "http://conductor:8080/blueprints/state/alice/my-project"
    username       = "alice"
    password       = "<your-token>"
  }
}
```

State is scoped per user: `/blueprints/state/{username}/{workspace}`.

---

## Extending pipelines with custom services

Conductor routes to every service listed in the **Registry**. To add a new
pipeline target — an internal deploy tool, a notification service, a custom API —
register it in the Registry manifest (`infra/local/registry-manifest.json`, or the
Helm equivalent) with its routes, any catalog **actions** pipelines can call, and
the permissions granted by default:

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

Conductor picks the service up on its next Registry poll. Now reference the action
from a reusable step and drop it into a pipeline — `${...}` resolves run inputs
and prior step outputs at execution time:

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
gateway. Run `armory pipelines list actions` to see every action the catalog
exposes.

---

## CLI — stay in your terminal

The `armory` CLI covers the full platform. You never need to open a browser.

```bash
armory auth login                                    # authenticate
armory pipelines list pipelines                      # see all pipelines
armory pipelines run pipeline <id> --input KEY=VALUE # trigger a run
armory pipelines get run <id>                        # inspect results
armory forge exec run --image node:20 -- npm test    # sandboxed run
armory tickets create --title "Deploy v2" --priority high
armory tickets update <id> --status in_progress
armory hooks rules create --name ci --repo myorg/myapp --events push --workflow <id> --secret <hmac>
armory containers list repos
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
| Blueprints | 8093 | Terraform state |
| Workflows | 8085 | [Pipeline orchestration](docs/workflows/README.md) |
| Tickets | 8086 | Task tracker |
| Hooks | 8087 | [Webhook receiver](docs/hooks/README.md) |
| Gitea Integration | 8088 | Forgejo/Gitea repos + PRs |
| Containers | 8089 | OCI registry management |
| Chaos | 8090 | Chaos engineering |
| Argo | 8091 | Argo CD sync |
| Outpost Gateway | 8092 | [Cluster integration backbone](docs/outpost-gateway/README.md) |
| Outpost | — | [Customer-deployed cluster agent](docs/outpost/README.md) |
| Egress Proxy | 3128 | Optional allowlist proxy for Forge |
| Armory CLI | — | [Command reference](docs/cli/README.md) |

Full platform guide with worked examples: [docs/platform-guide.md](docs/platform-guide.md)

---

## Docker images

Pre-built images are published to GHCR on every release (`alpha-latest` for pre-release):

```
ghcr.io/code-armory-app/conductor:alpha-latest
ghcr.io/code-armory-app/gatekeeper:alpha-latest
ghcr.io/code-armory-app/blueprints:alpha-latest
ghcr.io/code-armory-app/registry:alpha-latest
ghcr.io/code-armory-app/forge:alpha-latest
ghcr.io/code-armory-app/workflows:alpha-latest
ghcr.io/code-armory-app/hooks:alpha-latest
ghcr.io/code-armory-app/tickets:alpha-latest
ghcr.io/code-armory-app/gitea_integration:alpha-latest
ghcr.io/code-armory-app/containers:alpha-latest
ghcr.io/code-armory-app/chaos:alpha-latest
ghcr.io/code-armory-app/argo:alpha-latest
ghcr.io/code-armory-app/outpost-gateway:alpha-latest
ghcr.io/code-armory-app/outpost:alpha-latest
ghcr.io/code-armory-app/egress-proxy:alpha-latest
ghcr.io/code-armory-app/mcp:alpha-latest
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
export CONTAINER_REGISTRY_URL=https://ghcr.io
docker compose up --build -d

docker compose --profile test run --rm gatekeeper-integration-tests
docker compose --profile test run --rm blueprints-integration-tests
docker compose --profile test run --rm registry-integration-tests
docker compose --profile test run --rm conductor-integration-tests
docker compose --profile test run --rm forge-integration-tests
docker compose --profile test run --rm workflows-integration-tests
docker compose --profile test run --rm tickets-integration-tests
docker compose --profile test run --rm hooks-integration-tests
docker compose --profile test run --rm outpost-gateway-integration-tests
docker compose --profile test run --rm chaos-integration-tests
docker compose --profile test run --rm argo-integration-tests
```

Gitea integration tests require a live Forgejo instance and run directly with pytest — see [tests/gitea_integration/](tests/gitea_integration/).

---

## Status

CodeArmory is in **alpha**. APIs and data models may change between releases.

---

## Security

To report a vulnerability, use [GitHub's private security advisory form](https://github.com/code-armory-app/codearmory/security/advisories/new) — do not open a public issue. We'll respond within 72 hours.

