# Registry — Architecture

## Overview

Registry is the platform's service catalogue. It stores the URL, routing metadata, and endpoint manifest for every backend service. Conductor polls it every 30 seconds to rebuild its in-memory routing table and proxy map.

```
Operator / CI pipeline
  |
  +-- POST /services                   (ADMIN_KEY)
  +-- PUT  /services/{id}/endpoints
  +-- DELETE /services/{id}
  v
Registry :8084
  |
  +-- services table
  |     name, url, description, forward_auth, service_key (bcrypt hash)
  |
  +-- service_roles table
  |     named roles operators should provision in Gatekeeper
  |
  +-- service_endpoints table
        method, path, action, resource, public
              |
              | GET /services (READ_KEY)  every 30 s
              v
        Conductor :8082
          routing table (in-memory)
```

## Authentication

Registry uses two static API keys — no JWT or Gatekeeper involvement:

| Key | Accepted on |
|-----|-------------|
| `READ_KEY` | `GET /services` |
| `ADMIN_KEY` | All endpoints (also accepted on read endpoints) |

Both comparisons use `crypto/subtle.ConstantTimeCompare`. The two checks in `requireReadKey` are always evaluated (no short-circuit) to prevent timing-based enumeration of which key was tested first.

## Service registration

### Via API (`POST /services`)

The submitted URL is validated before storage to prevent SSRF attacks through the Conductor routing table:

- Must use `http` or `https`.
- Must not resolve to loopback (`127.0.0.0/8`), link-local (`169.254.0.0/16`, `fe80::/10`), RFC-1918 private ranges (`10/8`, `172.16/12`, `192.168/16`), or IPv6 ULA (`fc00::/7`).
- DNS hostnames are resolved at registration time; all returned addresses are validated. Unresolvable hostnames are rejected (fail-closed).

### Via `SERVICES` env var

`SERVICES=forge=http://forge:8083,blueprints=http://blueprints:8081` seeds services on startup without URL validation. Intended for internal Docker / Kubernetes DNS names.

### Via `MANIFEST_FILE`

A JSON file containing full service definitions (URL, roles, endpoints, service key). Also bypasses URL validation. Used to seed an initial catalogue in a single operation.

## Endpoint manifest

Conductor reads `GET /services` and receives a flat structure:

```json
[
  {
    "name": "forge",
    "url": "http://forge:8083",
    "forward_auth": false,
    "roles": [{ "name": "executor", "description": "..." }],
    "endpoints": [
      { "method": "POST", "path": "/executions",
        "action": "createExecution", "resource": "forge/executions",
        "public": false },
      { "method": "GET", "path": "/executions/{id}",
        "action": "getExecution", "resource": "forge/executions/{id}",
        "public": false }
    ]
  }
]
```

Conductor compiles each `path` into a regexp (`{id}` → `([^/]+)`) and stores `resource` as a template for per-request parameter substitution (e.g. `forge/executions/{id}` becomes `forge/executions/abc-123`).

## Service key

An optional `service_key` can be set on a service. It is bcrypt-hashed (cost 12) before storage and is never returned by the API. It is available for backend services that want to verify requests originated from a known, trusted counterpart.

## `PUT /services/{id}/endpoints`

Replaces the complete endpoint manifest (roles + endpoints) for a service atomically in a single transaction:

```
BEGIN
  DELETE FROM service_roles    WHERE service_id = $id
  INSERT INTO service_roles    ...
  DELETE FROM service_endpoints WHERE service_id = $id
  INSERT INTO service_endpoints ...
  optionally UPDATE services SET url, description
COMMIT
```

Entries with an empty `method`, `path`, `action`, or `resource` are silently skipped.

## Database

Three tables, created with raw SQL on startup:

| Table | Purpose |
|-------|---------|
| `services` | Service URL, name, forwarding config, service key hash |
| `service_roles` | Named roles associated with a service |
| `service_endpoints` | Method / path / action / resource manifest |

Soft-delete pattern: `DELETE /services/{id}` sets `active = false`. All queries filter `WHERE active = true`.
