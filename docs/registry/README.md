# Registry

Service discovery and endpoint registry. Stores the URL, routing metadata, and endpoint manifest for every backend service. Conductor reads from the Registry every 30 seconds to keep its routing table up to date.

## How it works

```
Services (blueprints, forge, …)
  │  Registered via SERVICES env var, MANIFEST_FILE, or admin API
  │
  ▼
Registry :8084
  │  stores service URL + endpoint manifest in PostgreSQL
  │
  ▼
Conductor :8082
  │  GET /services  (every 30 s, authenticated with READ_KEY)
  │  builds reverse-proxy map + endpoint list
  │
  ▼
  routes requests to the correct backend
```

## Requirements

- Go 1.25+
- PostgreSQL

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | **Required.** PostgreSQL connection string |
| `ADMIN_KEY` | — | **Required.** Bearer token for write operations (create/delete services, update endpoints) |
| `READ_KEY` | — | **Required.** Bearer token for read operations (`GET /services`). Also accepted by all write endpoints. |
| `SERVICES` | — | Comma-separated `name=url` pairs to seed on startup (e.g. `blueprints=http://blueprints:8081,forge=http://forge:8083`). Idempotent — updates the URL if the service already exists. |
| `MANIFEST_FILE` | — | Path to a JSON manifest file that seeds full service definitions (URL, endpoints, `service_key`) on startup. Bypasses SSRF validation — use only for trusted internal service URLs (e.g. Docker Compose or Kubernetes service names). |
| `PORT` | `8084` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `registry` | Service name reported in traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

## Running locally

```bash
cd src/systems/registry
DATABASE_URL=postgres://postgres:pass@localhost:5432/registry \
  ADMIN_KEY=your-admin-key \
  READ_KEY=your-read-key \
  go run .
```

## Docker

```bash
cd src/systems/registry
docker build -t registry:latest .

docker run -p 8084:8084 \
  -e DATABASE_URL=postgres://postgres:pass@db:5432/registry \
  -e ADMIN_KEY=your-admin-key \
  -e READ_KEY=your-read-key \
  registry:latest
```

## API

### Authentication

| Key | Endpoints |
|-----|-----------|
| `READ_KEY` | `GET /services` |
| `ADMIN_KEY` | All endpoints |

Pass the key as `Authorization: Bearer <key>`.

### Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/services` | Read | List all active services with their endpoint manifests |
| `POST` | `/services` | Admin | Register a new service |
| `DELETE` | `/services/{id}` | Admin | Soft-delete a service |
| `PUT` | `/services/{id}/endpoints` | Admin | Replace the endpoint manifest for a service |

### Register a service

```bash
curl -X POST http://registry:8084/services \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "inventory",
    "url": "http://inventory:8085",
    "description": "Inventory service",
    "forward_auth": false,
    "service_key": "optional-shared-secret"
  }'
```

**Fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Unique service name. Used as the routing key in Conductor. |
| `url` | string | Yes | Base URL of the service. Must use `http` or `https`; loopback, link-local, RFC-1918, and IPv6 ULA addresses are rejected. |
| `description` | string | No | Human-readable description |
| `forward_auth` | bool | No | If `true`, Conductor forwards the caller's `Authorization` header to the backend. If `false` (default), Conductor strips the bearer token and injects `X-User-ID` instead. |
| `service_key` | string | No | Optional shared secret for service identity. Bcrypt-hashed before storage; not returned in API responses. |
### Update endpoint manifest

The endpoint manifest tells Conductor which HTTP method/path combinations are valid and what Gatekeeper permission to check for each.

```bash
curl -X PUT http://registry:8084/services/$SERVICE_ID/endpoints \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "http://inventory:8085",
    "endpoints": [
      {
        "method": "GET",
        "path": "/items",
        "action": "listItem",
        "resource": "items",
        "public": false
      },
      {
        "method": "GET",
        "path": "/items/{id}",
        "action": "getItem",
        "resource": "items",
        "public": false
      }
    ]
  }'
```

This call replaces the entire endpoint manifest for the service. `url` and `description` are updated if provided.

**Endpoint fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `method` | string | Yes | HTTP method (`GET`, `POST`, `PUT`, `DELETE`, etc.) |
| `path` | string | Yes | Path pattern. Use `{param}` for path parameters (e.g. `/items/{id}`). |
| `action` | string | Yes | Gatekeeper action string checked against the caller's permissions |
| `resource` | string | Yes | Gatekeeper resource string checked against the caller's permissions |
| `public` | bool | No | If `true`, Conductor skips the permission check for this endpoint |

### List services

Returns all active services with their roles and endpoint manifests. Used by Conductor to build its routing table.

```bash
curl http://registry:8084/services \
  -H "Authorization: Bearer $READ_KEY"
```

## Service URL validation

When registering or updating a service URL, the Registry validates that the target is not a private address to prevent SSRF via the routing table:

- Loopback addresses (`127.0.0.0/8`, `::1`) are rejected
- Link-local addresses (`169.254.0.0/16`, `fe80::/10`) are rejected
- RFC-1918 private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) are rejected
- IPv6 ULA ranges (`fc00::/7`) are rejected
- DNS hostnames are resolved at registration time and all returned addresses are validated; hostnames that cannot be resolved are rejected (fail-closed). Internal service URLs that use Docker or Kubernetes DNS names should be pre-seeded via `SERVICES` env var or `MANIFEST_FILE`, which bypass this check.

## Schema

| Table | Primary Key | Description |
|-------|-------------|-------------|
| `services` | `service_id` | Registered services. Includes `service_key` (bcrypt hash, not returned in API responses). |
| `service_roles` | `role_id` | Named roles associated with a service |
| `service_endpoints` | `endpoint_id` | Endpoint manifests per service |

## Testing

```bash
# Unit tests
cd src/systems/registry
go test ./...

# Integration tests (requires running Registry and PostgreSQL)
pip install -r tests/registry/requirements.txt
REGISTRY_URL=http://localhost:8084 \
  ADMIN_KEY=your-admin-key \
  READ_KEY=your-read-key \
  pytest tests/registry/ -v
```
