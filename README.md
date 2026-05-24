# CodeArmory

Infrastructure services for developer platforms.

## Services

| Service | Language | Port | Description |
|---------|----------|------|-------------|
| [Conductor](docs/conductor/README.md) | Go | 8082 | API gateway — user-existence check, routes to Gatekeeper and Blueprints |
| [Gatekeeper](docs/gatekeeper/README.md) | Go | 8080 | Authentication, session management, and RBAC |
| [Blueprints](docs/blueprints/README.md) | Go | 8081 | Self-hosted Terraform HTTP backend backed by PostgreSQL |
| [Armory CLI](docs/cli/README.md) | Go | — | Command-line client for the platform |

## Repository Layout

```
src/systems/
  conductor/    — Conductor source and Dockerfile
  gatekeeper/   — Gatekeeper source and Dockerfile
  blueprints/   — Blueprints source and Dockerfile
src/cli/        — Armory CLI source
docs/
  conductor/    — API and deployment docs
  gatekeeper/   — Architecture, API, deployment, and development docs
  blueprints/   — API and deployment docs
  cli/          — CLI installation and command reference
tests/
  gatekeeper/   — Python integration tests for Gatekeeper
  blueprints/   — Python integration tests for Blueprints
  conductor/    — Python integration tests for Conductor
  cli/          — Python integration tests for Armory CLI
load-tests/     — Artillery load test scenarios
```

## Quick Start

The fastest way to run the full stack locally is Docker Compose:

```bash
cd infra/local
docker compose up --build
```

This starts PostgreSQL, Redis, Gatekeeper (8080), Blueprints (8081), and Conductor (8082).

To run individual services from source, see each service's README for prerequisites and environment variables.

## Docker Images

Pre-built images are published to GHCR on every release. The current pre-release channel uses the `alpha-` prefix:

```
ghcr.io/code-armory-app/gatekeeper:alpha-latest
ghcr.io/code-armory-app/blueprints:alpha-latest
ghcr.io/code-armory-app/conductor:alpha-latest
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
