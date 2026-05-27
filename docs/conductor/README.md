# Conductor

API gateway that routes authenticated requests to backend services registered in the [Registry](../registry/README.md). Every request passes through user authentication before being forwarded; permission checks use the endpoint manifest registered by each backend service.

## How it works

```
Client
  │
  ▼
Conductor :8082
  │
  └── all requests
        │
        ▼
      lookupEndpoint(method, path)
        │  Match against registered endpoint manifests (from Registry cache)
        │  → 404 if no match
        │
        ▼
      auth check (skipped for public endpoints)
        │  POST /check_permissions on Gatekeeper with the caller's Bearer token
        │     • 200 + authorized:true → proceed, extract user_id from response
        │     • 401 → 401 Unauthorized
        │     • anything else → 403 Forbidden
        │
        ▼
      header rewrite
        │  Strip: Authorization (unless forward_auth=true), X-Service-Key,
        │         X-User-ID, X-Forwarded-Host, X-Forwarded-Proto, X-Real-IP
        │  Inject: X-User-ID (authenticated), X-Forwarded-For (client IP)
        │  If CONDUCTOR_FORWARD_KEY set: inject X-Conductor-Token HMAC + X-Conductor-Timestamp
        │
        ▼
      reverse proxy → backend service
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
| `CONDUCTOR_FORWARD_KEY` | — | Shared secret used to sign `X-User-ID` headers forwarded to backend services. When set, Conductor injects `X-Conductor-Token` (HMAC-SHA256) and `X-Conductor-Timestamp` so backends can verify the header was injected by Conductor. Should match the value configured on each backend (e.g. Forge). |
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
- **Forward signing** — When `CONDUCTOR_FORWARD_KEY` is set, every forwarded request carries `X-Conductor-Token` (HMAC-SHA256 of `user_id:timestamp`) and `X-Conductor-Timestamp`. Backend services that set `forward_auth=false` can verify these headers to confirm `X-User-ID` was injected by Conductor and has not been tampered with. The token window is 30 seconds.
- **Permission enforcement** — Every non-public endpoint is permission-checked against Gatekeeper before the request reaches the backend. The action and resource are taken from the registered endpoint manifest, not from the request itself.

## Metrics

| Metric | Description |
|---|---|
| `conductor.requests.allowed.total` | Requests that passed the user-existence check |
| `conductor.requests.rejected.total` | Requests rejected, labelled by `reason`: `no_token`, `unauthorized`, `forbidden`, `gatekeeper_error` |
