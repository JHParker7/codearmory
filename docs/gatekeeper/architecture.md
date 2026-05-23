# Architecture

## Overview

Gatekeeper is a stateless HTTP service that provides authentication, session management, and role-based access control (RBAC) for multi-tenant applications. Every request is authenticated with a JWT and authorised against a permission record stored in the database.

```
Client
  │
  ▼
authMiddleware          — verifies JWT signature against stored public key
  │
  ▼
requirePermission       — checks user's role(s) carry the required action+resource
  │
  ▼
Handler                 — executes business logic, reads/writes via db interface
  │
  ▼
PostgreSQL
```

## Entities

```
Org
 ├── Role ──► Permissions (service + actions[] + resources[])
 └── Team ──► Role
      └── User ──► Org?, Role?, Team?
           └── Session (JWT + public key)
                    Invite ──► User (inviter), Org or Team (resource)
```

### User

A user account. Owns zero or one of each: `org_id`, `team_id`, `role_id`. All three are optional — a newly signed-up user has none until explicitly assigned.

`org_id`, `team_id`, and `role_id` cannot be changed through `PUT /users/{id}`; they are set as a side-effect of creating an org/team or accepting an invite.

### Org

A tenant boundary. Created by a user who becomes its owner. On creation the owner's `org_id` is set and they receive a wildcard permission on `gatekeeper/orgs/{org_id}`.

### Team

A group within an org. Created with an optional `role_id`; if omitted, a fresh empty role is created for the team automatically. On creation the owner's `team_id` is set and they receive a wildcard permission on `gatekeeper/teams/{team_id}`.

### Role

A named permission set. Holds an array of `permissions_ids`. Assigned directly to a user (`users.role_id`) or to a team (`teams.role_id`). A user accumulates permissions from both their direct role and their team's role.

### Permissions

A single permission record. Declares which `actions` (e.g. `getUser`, `*`) are allowed on which `resources` (e.g. `gatekeeper/users/abc-123`, `gatekeeper/orgs/*`) for a given `service` (e.g. `gatekeeper`).

### Session

Created on login. Stores the signed JWT, the ECDSA public key used to verify it, and an expiry. The JWT and public key are internal — they are never returned by the API. Sessions are invalidated on logout (`DELETE /sessions/{id}`) and when the owning user is deleted.

### Invite

A pending invitation for a user to join an org or team. `resource_type` is `"org"` or `"team"`; `status` is `"pending"`, `"accepted"`, or `"declined"`. Only the inviter and invitee can see an invite. Accepting an org invite sets `org_id` on the invitee; accepting a team invite sets `team_id`.

## Authentication

JWTs are signed with per-session ES256 (ECDSA P-256) key pairs. The private key is discarded after signing; the public key is stored in the `sessions` row. On each request `authMiddleware`:

1. Parses the JWT header without verification to extract the `jti` (session ID).
2. Loads the session from the database and checks it is active and not expired.
3. Parses the stored PEM public key.
4. Verifies the JWT signature against that key.
5. Injects the `sub` (user ID) into the request context.

This means revoking a session (`DELETE /sessions/{id}`) immediately invalidates the JWT — there is no grace period and no shared secret to rotate.

## Permission Resolution

`requirePermission` enforces access in two stages:

`checkPermissions(ctx, userID, service, action, resource)` is a pure function that:
- Loads the user.
- If `user.role_id` is set, loads the role and all its `Permissions` records.
- If `user.team_id` is set, loads the team's role and its `Permissions` records (stale team references — team was soft-deleted — are silently skipped).
- Iterates the combined permission list looking for a match where:
  - `permission.service == service`
  - `resource` matches any entry in `permission.resources` (exact, `*` wildcard, prefix `gatekeeper/orgs/*`, or segment wildcard `gatekeeper/*/abc`)
  - `action` matches any entry in `permission.actions` (exact, `*`, or prefix)
- Returns `true` on the first match; `false` if none match.

All access is explicitly granted — there is no implicit ownership fallback. Permissions are assigned at resource-creation time (org create, team create) and on invite acceptance.

See [permissions.md](./permissions.md) for matching rules and examples.

## Soft Deletes

Every table has an `active bool` column (default `true`). `Remove()` sets `active = false`; `Get()` filters on `active = true`. Deleted records remain in the database for audit purposes but are invisible to all application queries.

Cascading effects that are handled explicitly (not via FK cascade):
- Deleting a user → invalidates all their sessions
- Deleting a team → clears `team_id` on all members
- Deleting an org → clears `org_id` on all members

## Database

PostgreSQL in production; SQLite (in-memory) for unit tests. The schema is defined entirely via GORM struct tags in `src/types.go`. `main.go` calls `AutoMigrate` (tables, columns, indexes) then `applyForeignKeys` (idempotent `DO $$` blocks) on every startup.

Migration order: `Org → Role → Team → User → Session → Permissions → Invite`

### Read/Write Splitting

Write operations (`Add`, `Update`, `Remove`) use the connection from `DATABASE_URL`. Read operations (`Get`, `List`) use `DATABASE_READ_URL`, falling back to `DATABASE_URL` if it is not set. In tests, both connections share the same in-memory SQLite instance.

## Observability

The local stack (`infra/local/compose.yml`) ships:

| Component | Role |
|-----------|------|
| OpenTelemetry Collector | Receives traces (OTLP) and metrics, routes to backends |
| Tempo | Distributed tracing storage |
| Prometheus | Metrics storage (remote-write from OTel Collector) |
| Loki | Log aggregation |
| Grafana | Dashboards (Tempo, Prometheus, Loki datasources pre-configured) |

The application exports:
- **Traces** — every handler and `authMiddleware` creates a span; `checkPermissions` creates a child span.
- **Metrics** — `auth_middleware_requests_total` (by status), `permission_checks_total` (by authorized bool).
- **Logs** — structured `log/slog` at Debug/Info/Warn/Error.
