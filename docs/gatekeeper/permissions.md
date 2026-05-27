# Permissions

## Model

Access control in Gatekeeper has three layers:

1. **JWT verification** (`authMiddleware`) — the token must be valid, unexpired, and match an active session.
2. **Permission check** (`requirePermission`) — the user's role(s) must contain a `Permissions` record that grants the requested action on the requested resource. All access is explicitly granted; there is no implicit ownership fallback.
3. **Ownership checks** on invites — invite endpoints additionally verify the caller is the inviter or invitee.

## Permissions Records

A `Permissions` record grants a set of `actions` on a set of `resources` for a named `service`:

```json
{
  "permissions_id": "perm-uuid",
  "service": "gatekeeper",
  "actions": ["getUser", "updateUser"],
  "resources": ["gatekeeper/users/abc-123"]
}
```

Records are assigned to a `Role` via its `permissions_ids` array. A `Role` is assigned to a user directly (`users.role_id`) or via their team (`teams.role_id`). A user accumulates permissions from **both** sources.

## Resource Path Convention

| Operation | Resource path |
|-----------|--------------|
| Create (collection) | `gatekeeper/{type}s` — e.g. `gatekeeper/orgs` |
| Read / Update / Delete (instance) | `gatekeeper/{type}s/{id}` — e.g. `gatekeeper/orgs/abc-123` |

The `service` field is always `"gatekeeper"` for built-in endpoints. External services may use any string that appears in `PERMITTED_SERVICES`.

## Matching Rules

`checkPermissions` returns `true` on the first `Permissions` record where all three conditions hold:

### Service
Exact match: `permission.service == requested_service`.

### Action
A requested action is allowed if any entry in `permission.actions`:
- equals the requested action exactly (`"getUser"`)
- is `"*"` (allows everything)
- is a prefix wildcard ending in `*` and the action starts with that prefix (`"get*"` matches `"getUser"`)

### Resource
A requested resource is allowed if any entry in `permission.resources`:
- equals the resource exactly (`"gatekeeper/users/abc-123"`)
- is `"*"` (allows everything)
- is a trailing wildcard and the resource starts with the prefix (`"gatekeeper/orgs/*"` matches `"gatekeeper/orgs/any-id"`)
- is a segment-level wildcard where `*` may stand in for any single path segment (`"gatekeeper/*/abc-123"` matches `"gatekeeper/users/abc-123"`)

## Default Permissions

On signup every user automatically receives a `Permissions` record and a `Role` granting:

| Action | Service | Resource |
|--------|---------|---------|
| `getUser`, `updateUser`, `deleteUser` | `gatekeeper` | `gatekeeper/users/{user_id}` |
| `createOrg` | `gatekeeper` | `gatekeeper/orgs` |
| `createTeam` | `gatekeeper` | `gatekeeper/teams` |
| `getState`, `updateState`, `deleteState`, `lockState`, `unlockState` | `blueprints` | `blueprints/states/{user_id}/*` |

All other permissions must be explicitly granted by a user who already holds them.

## Permitted Services

The `service` field in a `Permissions` record must be listed in the `PERMITTED_SERVICES` environment variable (comma-separated). The default allowlist is `gatekeeper,blueprints,forge`. Attempts to create or update a permission record with any other service name are rejected with `400 Bad Request`.

To register a new service:

```bash
PERMITTED_SERVICES=gatekeeper,blueprints,forge,my-service
```

## Org-Scoped Permissions

`Permissions` records may carry an `org_id` to scope them to a specific tenant. Cross-tenant injection is prevented at role-mutation time:

- A `Permissions` record with `org_id = A` cannot be added to a role by a caller whose `org_id` is `B`.
- A caller with no `org_id` cannot use org-scoped permissions at all — only nil-org (personal/system) permissions are accepted.
- A role's `org_id` can only be set to a value that matches the caller's own `org_id`; it cannot be cleared or reassigned to a foreign org.

## Owner Permissions

When a user creates an org or team, they automatically receive a wildcard permission scoped to that resource:

| Event | Permission granted |
|-------|-------------------|
| `POST /orgs` | `actions: ["getOrg","updateOrg","deleteOrg","inviteUser"]`, `resources: ["gatekeeper/orgs/{org_id}"]` |
| `POST /teams` | `actions: ["getTeam","updateTeam","deleteTeam","inviteUser"]`, `resources: ["gatekeeper/teams/{team_id}"]` |
| Accept org invite | `actions: ["getOrg"]`, `resources: ["gatekeeper/orgs/{org_id}"]` |
| Accept team invite | `actions: ["getTeam"]`, `resources: ["gatekeeper/teams/{team_id}"]` |

This permission is added to the owner's direct role. If they have no direct role at creation time (permissions come only via a team role), a new direct role is created for them first.

## Bootstrapping Admin Access

There is no built-in admin account. To bootstrap a user with broad permissions, insert a `Permissions` record and `Role` directly via the database and assign the role to the user:

```sql
INSERT INTO permissions (permissions_id, name, service, actions, resources, active, created_at, updated_at)
VALUES (
  gen_random_uuid(), 'admin', 'gatekeeper',
  '["*"]'::jsonb,
  '["*"]'::jsonb,
  true, now(), now()
);

INSERT INTO roles (role_id, permissions_ids, active, created_at, updated_at)
VALUES (gen_random_uuid(), CAST('["<permissions_id>"]' AS jsonb), true, now(), now());

UPDATE users SET role_id = '<role_id>' WHERE email = 'admin@example.com';
```

The integration test suite does exactly this for the `admin_token` fixture in `tests/gatekeeper/conftest.py`.

## Examples

### User can only read their own profile

```json
{
  "service": "gatekeeper",
  "actions": ["getUser"],
  "resources": ["gatekeeper/users/alice-uuid"]
}
```

### Team member can read all users in an org

```json
{
  "service": "gatekeeper",
  "actions": ["getUser"],
  "resources": ["gatekeeper/users/*"]
}
```

### Org admin has full control over their org

```json
{
  "service": "gatekeeper",
  "actions": ["*"],
  "resources": ["gatekeeper/orgs/acme-uuid"]
}
```

### Service-to-service permission check

An external service `inventory` checking whether a user may read items:

```json
{
  "service": "inventory",
  "actions": ["readItem"],
  "resources": ["inventory/items/*"]
}
```

```bash
curl -X GET http://gatekeeper:8080/check_permissions \
  -H "Authorization: Bearer <token>" \
  -d '{"service":"inventory","action":"readItem","resource":"inventory/items/sku-456"}'
# → {"authorized": true}
```
