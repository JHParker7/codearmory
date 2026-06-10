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
Conductor :8080
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
| `SERVICES` | — | Comma-separated `name=url` pairs to seed on startup (e.g. `blueprints=http://blueprints:8084,forge=http://forge:8083`). Idempotent — updates the URL if the service already exists. |
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

The registry API uses service account keys rather than Gatekeeper tokens. Keys are passed as `X-Service-Key: name:key`.

| Role | Endpoints |
|------|-----------|
| Read (any valid service key) | `GET /services`, `GET /actions`, `GET /default-grants` |
| Admin service key | All endpoints |

### Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/services` | Read | List all active services with their endpoint manifests |
| `POST` | `/services` | Admin | Register a new service |
| `DELETE` | `/services/{id}` | Admin | Soft-delete a service |
| `PUT` | `/services/{id}/endpoints` | Admin | Replace the endpoint manifest, actions, and default grants for a service |
| `GET` | `/actions` | Read | List all active workflow actions across all services |
| `GET` | `/default-grants` | Read | List all active default permission grants across all services |

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

The endpoint manifest tells Conductor which HTTP method/path combinations are valid and what Gatekeeper permission to check for each. The same call also accepts `actions` and `default_grants` arrays.

```bash
curl -X PUT http://registry:8084/services/$SERVICE_ID/endpoints \
  -H "X-Service-Key: conductor:$ADMIN_KEY" \
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
    ],
    "actions": [
      {
        "name": "forge/run",
        "method": "POST",
        "path": "/executions",
        "body_transforms": [...],
        "async": {...}
      }
    ],
    "default_grants": [
      {
        "grant_on": "user",
        "actions": ["createExecution"],
        "resources": ["forge/executions"]
      }
    ]
  }'
```

This call replaces the entire endpoint manifest, actions list, and default grants for the service atomically. `url` and `description` are updated if provided.

**Endpoint fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `method` | string | Yes | HTTP method (`GET`, `POST`, `PUT`, `DELETE`, etc.) |
| `path` | string | Yes | Path pattern. Use `{param}` for path parameters (e.g. `/items/{id}`). |
| `action` | string | Yes | Gatekeeper action string checked against the caller's permissions |
| `resource` | string | Yes | Gatekeeper resource string checked against the caller's permissions |
| `public` | bool | No | If `true`, Conductor skips the permission check for this endpoint |

**Action fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Unique action name (e.g. `forge/run`). Used by the Workflows service to look up callable actions. |
| `method` | string | Yes | HTTP method to call on the service |
| `path` | string | Yes | Path on the service to call |
| `body_transforms` | array | No | JSONB — field transform rules applied before the HTTP call |
| `async` | object | No | JSONB — async polling configuration for long-running actions |

**Default grant fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `grant_on` | string | Yes | Principal type to grant on creation: `user`, `org`, or `team` |
| `actions` | []string | Yes | Gatekeeper action strings to grant |
| `resources` | []string | Yes | Gatekeeper resource strings to grant |

### List services

Returns all active services with their roles and endpoint manifests. Used by Conductor to build its routing table.

```bash
curl http://registry:8084/services \
  -H "X-Service-Key: conductor:$READ_KEY"
```

### List workflow actions

Returns all active workflow actions across all services, enriched with the service URL. Consumed by the Workflows service to resolve action names to concrete HTTP calls.

```bash
curl http://registry:8084/actions \
  -H "X-Service-Key: workflows:$READ_KEY"
```

Response is an array of action objects: `action_id`, `service_id`, `service_name`, `service_url`, `name`, `method`, `path`, `body_transforms`, `async`, `active`, plus `gk_service`, `gk_action`, `gk_resource` when a matching `service_endpoint` record exists. These three fields carry the Gatekeeper permission triple required to call the action, used by the Workflows service to build scoped roles for workflow runs.

### List default grants

Returns all active default permission grants declared by services. Consumed by Gatekeeper at startup to know which permissions to apply when creating new users, orgs, or teams.

```bash
curl http://registry:8084/default-grants \
  -H "X-Service-Key: gatekeeper:$READ_KEY"
```

Response is an array of grant objects: `grant_id`, `service_id`, `service_name`, `grant_on`, `actions[]`, `resources[]`.

## Service URL validation

When registering or updating a service URL, the Registry validates that the target is not a private address to prevent SSRF via the routing table:

- Loopback addresses (`127.0.0.0/8`, `::1`) are rejected
- Link-local addresses (`169.254.0.0/16`, `fe80::/10`) are rejected
- RFC-1918 private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) are rejected
- IPv6 ULA ranges (`fc00::/7`) are rejected
- DNS hostnames are resolved at registration time and all returned addresses are validated; hostnames that cannot be resolved are rejected (fail-closed). Internal service URLs that use Docker or Kubernetes DNS names should be pre-seeded via `SERVICES` env var or `MANIFEST_FILE`, which bypass this check.

## Action catalog and default grants

### Action catalog (`service_actions`)

Services declare callable actions when they register their endpoint manifest. The Workflows service queries `GET /actions` at startup to build its action catalog — a map of action name to the concrete HTTP call to make against the originating service.

Actions support optional `body_transforms` (field mapping rules) and `async` config (polling behaviour for long-running operations). Both are stored as JSONB and passed through to the Workflows engine unchanged.

### Default grants (`service_default_grants`)

Services declare permission templates in `default_grants` when registering their manifest. Each template specifies which actions and resources should be granted to a principal (user, org, or team) at the moment of creation. Gatekeeper reads `GET /default-grants` at startup and applies the matching grants whenever a new user, org, or team is created, so that the baseline permissions needed to use each registered service are automatically provisioned.

## Schema

| Table | Primary Key | Description |
|-------|-------------|-------------|
| `services` | `service_id` | Registered services. Includes `service_key` (bcrypt hash, not returned in API responses). |
| `service_roles` | `role_id` | Named roles associated with a service |
| `service_endpoints` | `endpoint_id` | Endpoint manifests per service |
| `service_actions` | `action_id` | Callable workflow actions (`action_id`, `service_id`, `name`, `method`, `path`, `body_transforms`, `async_config`) |
| `service_default_grants` | `grant_id` | Permission templates applied on principal creation (`grant_id`, `service_id`, `grant_on`, `actions`, `resources`) |

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
