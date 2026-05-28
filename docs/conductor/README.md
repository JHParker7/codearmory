# Conductor

Lightweight API gateway. Conductor acts as an identity filter — it verifies that every caller is a real, active user and then forwards the request to the appropriate backend. Permission checks are the responsibility of each backend service, not Conductor.

## How it works

```
Client
  │
  ▼
Conductor :8082
  │
  ├── block list check
  │     Source IP blocked after 3 post-auth failures? → 403 Forbidden
  │
  ▼
  lookupEndpoint(method, path)
    │  Match against registered endpoint manifests (from Registry cache)
    │  → 404 if no match
    │
    ▼
  user auth (skipped for public endpoints)
    │  1. Decode JWT payload → extract user_id (sub claim)
    │  2. GET /users/{id} on Gatekeeper with the caller's Bearer token
    │        Gatekeeper verifies the JWT signature here
    │     • 200 → user exists and token is valid; proceed
    │     • 401 → 401 Unauthorized
    │     • anything else → 403 Forbidden
    │
    ▼
  header rewrite
    │  Strip: Authorization (unless forward_auth=true), X-Service-Key,
    │         X-User-ID, X-Forwarded-Host, X-Forwarded-Proto, X-Real-IP
    │  Inject: X-User-ID (authenticated user_id), X-Forwarded-For (client IP)
    │  If CONDUCTOR_FORWARD_KEY set: inject X-Conductor-Token HMAC + X-Conductor-Timestamp
    │
    ▼
  reverse proxy → backend service
    │
    └── if service returns 401 after conductor auth passed:
          log failure + increment source IP suspect counter
          → block IP after 3 failures (1 hour)
```

Conductor polls the Registry every 30 seconds to refresh its in-memory service and endpoint cache. JWT signature verification is delegated to Gatekeeper via the user-existence call; Conductor never verifies signatures itself. Backend services that declare `forward_auth=true` receive the original `Authorization` header so they can call Gatekeeper for fine-grained permission checks.

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

All requests are matched against registered endpoint patterns. If no match is found, conductor returns `404 Not Found`. If a path matches a registered service prefix but no specific endpoint within that service, conductor returns `404 Not Found` (not 503 — the service is reachable, the path just isn't registered).

### Endpoint matching

Each registered endpoint declares:
- `method` — HTTP method (`GET`, `POST`, etc.)
- `path` — path pattern (supports `{param}` placeholders compiled to regexps)
- `action` + `resource` — the permission the backend service will check with Gatekeeper
- `public` — if `true`, conductor forwards the request without any user auth check

## Security

- **Identity filter** — Conductor's auth check confirms the user exists and the JWT is valid by calling Gatekeeper's `GET /users/{id}`. It does not evaluate permissions; that is the backend service's responsibility.
- **Permission enforcement** — Backend services that need per-resource access control must call Gatekeeper's `POST /check_permissions` themselves, using the forwarded `Authorization` header (set `forward_auth=true` in the registry so Conductor passes it through).
- **Auth header stripping** — For `forward_auth=false` services, `Authorization` is removed before forwarding so backends cannot replay it against other services. `X-Service-Key`, `X-User-ID`, `X-Forwarded-Host`, `X-Forwarded-Proto`, and `X-Real-IP` are always stripped from incoming requests.
- **Forward signing** — When `CONDUCTOR_FORWARD_KEY` is set, every forwarded request carries `X-Conductor-Token` (HMAC-SHA256 of `conductor:{user_id}:{timestamp}`) and `X-Conductor-Timestamp`. Backend services with `forward_auth=false` can verify these to confirm `X-User-ID` was injected by Conductor and not spoofed. The token window is 30 seconds.
- **Source IP block list** — If a source IP's requests pass Conductor's user-auth check but are then rejected by the backend with 401 three times, the IP is blocked for one hour. This catches replay attacks and token-forgery probes that slip past the user-existence filter.

## Metrics

| Metric | Description |
|---|---|
| `conductor.requests.allowed.total` | Requests that passed the user-existence check |
| `conductor.requests.rejected.total` | Requests rejected by the user-existence check, labelled by `reason`: `no_token`, `malformed_token`, `unauthorized`, `user_not_found`, `gatekeeper_error` |
| `conductor.ips.blocked.total` | Source IPs added to the block list, labelled by `source_ip` |
