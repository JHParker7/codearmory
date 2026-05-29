# CodeArmory

[![CI](https://github.com/code-armory-app/codearmory/actions/workflows/test_build_release.yml/badge.svg)](https://github.com/code-armory-app/codearmory/actions/workflows/unit_tests.yml)
[![License: ELv2](https://img.shields.io/badge/License-ELv2-blue)](LICENSE)

Infrastructure services for developer platforms.

## Services

| Service | Language | Port | Description |
|---------|----------|------|-------------|
| [Conductor](docs/conductor/README.md) | Go | 8082 | API gateway — authenticates requests, checks permissions, and reverse-proxies to registered backends |
| [Gatekeeper](docs/gatekeeper/README.md) | Go | 8080 | Authentication, session management, and RBAC |
| [Blueprints](docs/blueprints/README.md) | Go | 8081 | Self-hosted Terraform HTTP backend backed by PostgreSQL |
| [Registry](docs/registry/README.md) | Go | 8084 | Service discovery — stores endpoint manifests; Conductor polls it to build its routing table |
| [Forge](docs/forge/README.md) | Go | 8083 | Sandboxed code execution — runs user-submitted commands in isolated containers |
| [Workflows](docs/workflows/README.md) | Go | 8085 | CI/CD pipeline orchestrator — sequences HTTP steps against registered services, triggered by users or the hooks service |
| [Tickets](docs/tickets/README.md) | Go | 8086 | General task tracker — org-scoped tickets with comments and optional links to workflow runs and forge executions |
| [Hooks](docs/hooks/README.md) | Go | 8087 | Git webhook receiver — matches incoming events against pipeline rules and triggers workflow runs |
| [Armory CLI](docs/cli/README.md) | Go | — | Command-line client for the platform |

## Repository Layout

```
src/systems/
  conductor/    — Conductor source and Dockerfile
  gatekeeper/   — Gatekeeper source and Dockerfile
  blueprints/   — Blueprints source and Dockerfile
  registry/     — Registry source and Dockerfile
  forge/        — Forge source and Dockerfile
  workflows/    — Workflows source and Dockerfile
  tickets/      — Tickets source and Dockerfile
  hooks/        — Hooks source and Dockerfile
src/cli/        — Armory CLI source
docs/
  conductor/    — API and deployment docs
  gatekeeper/   — Architecture, API, deployment, and development docs
  blueprints/   — API and deployment docs
  registry/     — API and deployment docs
  forge/        — API, security, and deployment docs
  workflows/    — API and deployment docs
  tickets/      — API and deployment docs
  hooks/        — API and deployment docs
  cli/          — CLI installation and command reference
tests/
  gatekeeper/   — Python integration tests for Gatekeeper
  blueprints/   — Python integration tests for Blueprints
  conductor/    — Python integration tests for Conductor
  registry/     — Python integration tests for Registry
  forge/        — Python integration tests for Forge
  workflows/    — Python integration tests for Workflows
  tickets/      — Python integration tests for Tickets
  hooks/        — Python integration tests for Hooks
  cli/          — Python integration tests for Armory CLI
load-tests/     — Artillery load test scenarios
```

## Quick Start

The fastest way to run the full stack locally is Docker Compose:

```bash
cd infra/local
docker compose up --build
```

This starts PostgreSQL, Redis, Gatekeeper (8080), Blueprints (8081), Conductor (8082), Forge (8083), Registry (8084), Workflows (8085), Tickets (8086), and Hooks (8087).

To run individual services from source, see each service's README for prerequisites and environment variables.

## Docker Images

Pre-built images are published to GHCR on every release. The current pre-release channel uses the `alpha-` prefix:

```
ghcr.io/code-armory-app/gatekeeper:alpha-latest
ghcr.io/code-armory-app/blueprints:alpha-latest
ghcr.io/code-armory-app/conductor:alpha-latest
ghcr.io/code-armory-app/registry:alpha-latest
ghcr.io/code-armory-app/forge:alpha-latest
ghcr.io/code-armory-app/workflows:alpha-latest
ghcr.io/code-armory-app/tickets:alpha-latest
ghcr.io/code-armory-app/hooks:alpha-latest
```

CLI binaries for Linux, macOS, and Windows are attached to each [GitHub release](../../releases).

## CI

The CI pipeline runs on every push and pull request to `main`:

1. Unit tests for each service and the CLI (parallel)
2. Integration tests — full stack deployed via Docker Compose
3. Semantic release (push to `main` only) — creates a GitHub release and publishes Docker images and CLI binaries

## Pre-commit Hooks

This repo uses [pre-commit](https://pre-commit.com/) for commit-message linting (Conventional Commits) and secret scanning (gitleaks).

```bash
pip install pre-commit
pre-commit install --hook-type commit-msg
```
