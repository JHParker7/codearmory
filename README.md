# CodeArmory

Infrastructure services for developer platforms.

## Services

| Service | Language | Port | Description |
|---------|----------|------|-------------|
| [Conductor](docs/conductor/README.md) | Go | 8082 | API gateway — user-existence check, routes to Gatekeeper and Blueprints |
| [Gatekeeper](docs/gatekeeper/README.md) | Go | 8080 | Authentication, session management, and RBAC |
| [Blueprints](docs/blueprints/README.md) | Go | 8081 | Self-hosted Terraform HTTP backend backed by PostgreSQL |

## Repository Layout

```
src/systems/
  conductor/    — Conductor source and Dockerfile
  gatekeeper/   — Gatekeeper source and Dockerfile
  blueprints/   — Blueprints source and Dockerfile
docs/
  conductor/    — API and deployment docs
  gatekeeper/   — Architecture, API, deployment, and development docs
  blueprints/   — API and deployment docs
tests/
  gatekeeper/   — Python integration tests for Gatekeeper
  blueprints/   — Python integration tests for Blueprints
load-tests/     — Artillery load test scenarios
```

## Quick Start

All services require PostgreSQL. See each service's README for full setup instructions.

```bash
# Gatekeeper
cd src/systems/gatekeeper
go run ./...

# Blueprints (requires a running Gatekeeper)
cd src/systems/blueprints
go run .

# Conductor — API gateway (requires running Gatekeeper and Blueprints)
cd src/systems/conductor
go run .
```

## Pre-commit Hooks

This repo uses [pre-commit](https://pre-commit.com/) for commit-message linting (Conventional Commits) and secret scanning (gitleaks).

```bash
pip install pre-commit
pre-commit install --hook-type commit-msg
```
