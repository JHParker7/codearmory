# Conductor

API gateway that sits in front of [Gatekeeper](../gatekeeper/README.md) and [Blueprints](../blueprints/README.md). Every request except `POST /signup` and `POST /login` passes through a user-existence check before being forwarded to the appropriate backend.

## How it works

```
Client
  │
  ▼
Conductor :8082
  │
  ├── POST /signup  ──────────────────────────────► Gatekeeper :8080
  ├── POST /login   ──────────────────────────────► Gatekeeper :8080
  │
  └── everything else
        │
        ▼
      userMiddleware
        │  1. Require Authorization: Bearer <token>
        │  2. Decode JWT payload → extract sub (user ID)
        │  3. GET /users/{id} on Gatekeeper with the token
        │     • 200 → user exists and token is valid → proceed
        │     • anything else → 401 Unauthorized
        │
        ├── /state/** or /{org}/state/** ─────────► Blueprints :8081
        └── everything else ──────────────────────► Gatekeeper :8080
```

The user check calls Gatekeeper's `GET /users/{id}` with the caller's bearer token. Gatekeeper verifies the JWT signature and checks the user record is active, so conductor rejects requests from deleted users or with invalid tokens before they reach any backend.

## Requirements

- Go 1.25+
- A running Gatekeeper instance
- A running Blueprints instance (for state routes)

## Configuration

| Variable | Default | Description |
|---|---|---|
| `GATEKEEPER_URL` | `http://localhost:8080` | Base URL of the Gatekeeper service |
| `BLUEPRINTS_URL` | `http://localhost:8081` | Base URL of the Blueprints service |
| `PORT` | `8082` | Port the server listens on |
| `TLS_CERT_FILE` | — | Path to PEM-encoded TLS certificate. Required with `TLS_KEY_FILE` to enable HTTPS. |
| `TLS_KEY_FILE` | — | Path to PEM-encoded TLS private key. Required with `TLS_CERT_FILE` to enable HTTPS. |
| `OTEL_SERVICE_NAME` | `conductor` | Service name reported in traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

## Running locally

```bash
cd src/systems/conductor
GATEKEEPER_URL=http://localhost:8080 \
BLUEPRINTS_URL=http://localhost:8081 \
go run .
```

## Docker

```bash
cd src/systems/conductor
docker build -t conductor:latest .

docker run -p 8082:8082 \
  -e GATEKEEPER_URL=http://gatekeeper:8080 \
  -e BLUEPRINTS_URL=http://blueprints:8081 \
  conductor:latest
```

## Routing

| Path pattern | Backend |
|---|---|
| `POST /signup` | Gatekeeper (no auth check) |
| `POST /login` | Gatekeeper (no auth check) |
| `/state/{username}/{workspace}` | Blueprints (after user check) |
| `/{org}/state/{team}/{workspace}` | Blueprints (after user check) |
| everything else | Gatekeeper (after user check) |

## Metrics

| Metric | Description |
|---|---|
| `conductor.requests.allowed.total` | Requests that passed the user-existence check |
| `conductor.requests.rejected.total` | Requests rejected, labelled by `reason`: `no_token`, `malformed_token`, `user_not_found` |
