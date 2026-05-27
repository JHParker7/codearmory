# Conductor

API gateway that routes authenticated requests to backend services registered in the [Registry](../registry/README.md). Every request passes through user authentication before being forwarded; permission checks use the endpoint manifest registered by each backend service.

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
        ▼
      permissionMiddleware
        │  1. Match the request path against registered endpoint manifests
        │  2. POST /check_permissions on Gatekeeper
        │     • authorized → forward to backend
        │     • denied → 403 Forbidden
        │     • public endpoint → skip permission check
        │
        ▼
      reverse proxy → backend service (from registry)
```

Conductor polls the Registry every 30 seconds to refresh its in-memory service and endpoint cache. Auth headers (`Authorization`, `X-Service-Key`) are stripped before forwarding to backend services. For the Forge execution service, Conductor signs the `X-User-ID` header with an HMAC-SHA256 token so Forge can verify the request came from Conductor.

## Requirements

- Go 1.25+
- A running Gatekeeper instance
- A running Registry instance

## Configuration

| Variable | Default | Description |
|---|---|---|
| `GATEKEEPER_URL` | `http://localhost:8080` | Base URL of the Gatekeeper service |
| `REGISTRY_URL` | `http://localhost:8084` | Base URL of the Registry service |
| `REGISTRY_READ_KEY` | — | **Required.** Shared secret for authenticating reads from the Registry. |
| `FORGE_INTERNAL_KEY` | — | Shared secret used to sign `X-User-ID` headers forwarded to the Forge service. Should match the value configured on Forge. |
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
REGISTRY_URL=http://localhost:8084 \
REGISTRY_READ_KEY=your-read-key \
go run .
```

## Docker

```bash
cd src/systems/conductor
docker build -t conductor:latest .

docker run -p 8082:8082 \
  -e GATEKEEPER_URL=http://gatekeeper:8080 \
  -e REGISTRY_URL=http://registry:8084 \
  -e REGISTRY_READ_KEY=your-read-key \
  conductor:latest
```

## Routing

Conductor routes requests dynamically based on the endpoint manifests registered by each backend service. On startup and every 30 seconds, it fetches the current service list from the Registry and builds a reverse proxy per service.

Two endpoints bypass the service registry and are handled statically:

| Path | Backend | Auth |
|---|---|---|
| `POST /signup` | Gatekeeper | None |
| `POST /login` | Gatekeeper | None |

All other requests are matched against registered endpoint patterns. If no match is found, conductor returns `404 Not Found`.

### Endpoint matching

Each registered endpoint declares:
- `method` — HTTP method (`GET`, `POST`, etc.)
- `path` — path pattern (supports `{param}` placeholders compiled to regexps)
- `action` + `resource` — the Gatekeeper permission required
- `public` — whether to skip the permission check (auth still required unless the path is also listed as an anonymous bypass)

## Security

- **Auth header stripping** — `Authorization` and `X-Service-Key` headers are removed from requests before forwarding to backend services. Backends must not trust these headers from conductor.
- **Forge request signing** — Requests routed to Forge carry an `X-User-ID` header signed with HMAC-SHA256 using `FORGE_INTERNAL_KEY`. Forge validates this signature to confirm the request was forwarded by Conductor.
- **Permission enforcement** — Every non-public endpoint is permission-checked against Gatekeeper before the request reaches the backend. The action and resource are taken from the registered endpoint manifest, not from the request itself.

## Metrics

| Metric | Description |
|---|---|
| `conductor.requests.allowed.total` | Requests that passed the user-existence check |
| `conductor.requests.rejected.total` | Requests rejected, labelled by `reason`: `no_token`, `malformed_token`, `user_not_found` |
