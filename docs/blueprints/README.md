# Blueprints

A self-hosted [Terraform HTTP backend](https://developer.hashicorp.com/terraform/language/backend/http) that stores workspace state in PostgreSQL. Authentication and authorization are delegated to [Gatekeeper](../gatekeeper/README.md).

## Features

- Full Terraform backend protocol: GET/POST/DELETE state, LOCK/UNLOCK
- Per-workspace pessimistic locking with lock-ID validation
- Bearer token and HTTP Basic auth (Basic credentials are exchanged for a token via Gatekeeper)
- At-rest encryption of state data with AES-256-GCM (optional)
- mTLS support for mutual client certificate verification
- Prometheus metrics at `/metrics`
- Distributed tracing via OpenTelemetry → Tempo
- Structured JSON logging via Loki

## Requirements

- Go 1.25+
- PostgreSQL
- A running Gatekeeper instance

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@127.0.0.1:5432/blueprints` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Base URL of the Gatekeeper service |
| `REDIS_URL` | — | Redis connection string (`redis://host:6379/1`). Omit to disable caching. |
| `ENCRYPTION_KEY` | — | **Required.** 64-character hex string (32 bytes) for AES-256-GCM at-rest encryption. Generate with: `openssl rand -hex 32`. Blueprints refuses to start without this key. |
| `PORT` | `8081` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `blueprints` | Service name reported in traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `TLS_CERT_FILE` | — | Path to PEM-encoded TLS certificate. Required with `TLS_KEY_FILE` to enable HTTPS. |
| `TLS_KEY_FILE` | — | Path to PEM-encoded TLS private key. Required with `TLS_CERT_FILE` to enable HTTPS. |
| `CA_CERT_FILE` | — | Path to PEM-encoded CA certificate. When set, enables mTLS (requires and verifies client certificates). |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `ENCRYPTION_KEY_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

## Running locally

```bash
cd src/systems/blueprints
DATABASE_URL=postgresql://postgres:pass@localhost:5432/blueprints \
  GATEKEEPER_URL=http://localhost:8080 \
  go run .
```

The server listens on port `8081`.

## Docker

```bash
cd src/systems/blueprints
docker build -t blueprints:latest .

docker run -p 8081:8081 \
  -e DATABASE_URL=postgresql://postgres:pass@db:5432/blueprints \
  -e GATEKEEPER_URL=http://gatekeeper:8080 \
  blueprints:latest
```

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/state/{username}/{workspace}` | Fetch state (204 if none exists) |
| `POST` | `/state/{username}/{workspace}` | Store/update state |
| `DELETE` | `/state/{username}/{workspace}` | Delete state |
| `LOCK` | `/state/{username}/{workspace}` | Acquire workspace lock |
| `UNLOCK` | `/state/{username}/{workspace}` | Release workspace lock |

## Terraform configuration

```hcl
terraform {
  backend "http" {
    address        = "http://blueprints:8081/state/alice/dev"
    lock_address   = "http://blueprints:8081/state/alice/dev"
    unlock_address = "http://blueprints:8081/state/alice/dev"
    username       = "alice@example.com"
    password       = "your-password"
  }
}
```

## Permissions

Blueprints delegates all authorization to Gatekeeper. The required Gatekeeper permission records use `service: "blueprints"`.

Resource paths follow the pattern `states/{username}/{workspace}`.

Available actions: `getState`, `updateState`, `deleteState`, `lockState`, `unlockState`.

On **signup**, Gatekeeper automatically grants the new user full access to `states/{username}/*`.

Example — grant a user full access to their own workspaces:

```json
{
  "service": "blueprints",
  "actions": ["getState", "updateState", "deleteState", "lockState", "unlockState"],
  "resources": ["states/alice/*"]
}
```

## Testing

```bash
# Unit tests (no external services required)
cd src/systems/blueprints
go test ./...

# Integration tests (requires running Blueprints, Gatekeeper, and PostgreSQL)
pip install -r tests/blueprints/requirements.txt
API_URL=http://localhost:8081 GATEKEEPER_URL=http://localhost:8080 \
  pytest tests/blueprints/ -v
```
