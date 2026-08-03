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

### Owner namespaces — read this before writing a resource string

Before matching, gatekeeper **rewrites** the requested resource: an *unscoped* resource
gets the **caller's username** prefixed to it (`scopeResource`). `forge/executions`
asked for by `alice` is matched as `alice/forge/executions`. That is what makes
"my things" work without every service filtering by hand, and it is why default grants
are templated `{username}/…`.

A resource is left **alone** when it already names an owner:

| Form | Owner | Example |
|---|---|---|
| `<username>/<service>/…` | a user | `alice/forge/executions` |
| `org/<orgName>/<service>/…` | an org | `org/acme/tickets/boards` |
| `project/<slug>/<service>/…` | a project (its own top-level namespace, not bound to a user) | `project/core/workflows/pipelines/*` |
| `codearmory/<service>/…` | **the platform** | `codearmory/forge/runner-classes` |

`codearmory` is the platform namespace: instance-level configuration that belongs to no
user, org or project — runner classes, allowed images, runtime backends, concurrency
limits, OIDC settings, the audit log, registry config, the default-org baseline, and the
global ticket field definitions. It is a real, non-loginable account so those changes
attribute to a principal that resolves.

**Why `codearmory`, `org` and `project` are reserved usernames.** None of them is
special-cased in the matcher — there is no branch for `codearmory` in `scopeResource` or
`ownerQualified`. `codearmory/forge/runner-classes` is left alone purely because it has
the shape `<owner>/<service>/…`, which is indistinguishable from an ordinary
user-owned resource. So if someone could register the username `codearmory`, their own
default grants would be templated straight into the platform namespace
(`{username}/forge/runner-classes` → `codearmory/forge/runner-classes`) and they would
hold instance configuration by construction. `org` and `project` would collide with the
other two exempt prefixes the same way. The guard in `User.Add`/`User.Update` is the only
thing standing between a signup form and platform config — it is load-bearing, not
cosmetic.

Two consequences worth internalising:

- **Declaring a platform resource unscoped is a bug that hides.** Before this convention,
  `forge/runner-classes` was caller-prefixed on *both* sides — grant and check — so it
  matched by symmetry rather than by ownership: global data modelled as though everyone
  had a private copy. Admin-only resources merely looked fine because the admin wildcard
  matches anything.
- **A resource leading with the service name is always unscoped.** `tickets/tickets` would
  otherwise parse as owner `tickets`, and the caller's name would never be applied —
  silently denying every user their own tickets. Any service whose collection shares its
  name has this shape.

Ordinary users get read-only defaults on the platform catalogs they need
(`listRunnerClass`/`getRunnerClass` on `codearmory/forge/runner-classes`); writes are not
in any default grant, so only a wildcard — i.e. an admin — can make them.

### Naming an owner from a service

`POST /check_permissions` returns the caller's **`username`** alongside `user_id`:

```json
{ "authorized": true, "user_id": "3f2a…", "username": "alice", "org_id": null }
```

That field exists because resources are keyed by **username** while a service only ever
learned the **user id** — so a service could not build an owner-first resource naming its
own caller without a second round trip to `/oauth/userinfo`. In the SDK, use `Check`
(which returns a `Subject`) rather than `CheckPermissions` when the handler needs it.

`username` is **omitted when empty**, so its presence means "this subject has a namespace
you can name". It is absent for a client-credentials subject — an OAuth client is not a
user and owns no namespace. Treat an absent username as *cannot build an owner-first
resource*, never as an empty namespace: `"" + "/tickets/tickets/x"` yields
`/tickets/tickets/x`, which matches no grant and turns a missing namespace into an
unexplainable 403.

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
| `getState`, `updateState`, `deleteState`, `lockState`, `unlockState` | `blueprints` | `states/{username}/*` |
| `createWorkflow`, `listWorkflow`, `getWorkflow`, `updateWorkflow`, `deleteWorkflow`, `triggerRun`, `listRun`, `getRun`, `cancelRun` | `workflows` | `workflows/workflows`, `workflows/workflows/*`, `workflows/runs`, `workflows/runs/*` |
| `createTicket`, `listTicket`, `getTicket`, `updateTicket`, `deleteTicket`, `createComment`, `deleteComment` | `tickets` | `tickets/tickets`, `tickets/tickets/*` |
| `createTrigger`, `listTrigger`, `getTrigger`, `updateTrigger`, `deleteTrigger`, `listEvent`, `getEvent` | `events` | `events/triggers`, `events/triggers/*`, `events` |

All other permissions must be explicitly granted by a user who already holds them.

## Permitted Services

The `service` field in a `Permissions` record must be listed in the `PERMITTED_SERVICES` environment variable (comma-separated). The default allowlist is `gatekeeper,blueprints,forge,workflows,tickets,hooks`. Attempts to create or update a permission record with any other service name are rejected with `400 Bad Request`.

To register a new service:

```bash
PERMITTED_SERVICES=gatekeeper,blueprints,forge,workflows,tickets,events,my-service
```

## Org-Scoped Permissions

`Permissions` records may carry an `org_id` to scope them to a specific tenant. Cross-tenant injection is prevented at role-mutation time:

- A `Permissions` record with `org_id = A` cannot be added to a role by a caller whose `org_id` is `B`.
- A caller with no `org_id` cannot use org-scoped permissions at all — only nil-org (personal/system) permissions are accepted.
- A role's `org_id` can only be set to a value that matches the caller's own `org_id`; it cannot be cleared or reassigned to a foreign org.
- A caller may only delete a `Permissions` record whose `org_id` matches their own; attempts to delete another org's permission records return `403 Forbidden`.

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

## Scoped Roles (Run Tokens)

Workflow run tokens carry a `scoped_role_id` in their session record. When present, `checkPermissions` evaluates only the permissions in that role — the user's direct role and team role are bypassed entirely. This enforces a minimal permission set for workflow executions without granting the triggering user's full access to the workflow worker.

Scoped roles are created and deleted automatically by the Workflows service via the internal `POST /internal/workflow-roles` and `DELETE /internal/workflow-roles/{role_id}` endpoints. Only the `workflows` service account may call these endpoints.

## Permission Check Audit

Every call to `checkPermissions` — whether from an internal `requirePermission` guard or from a service calling `POST /check_permissions` — can optionally be recorded in the `permissions_checks` table. Each row captures:

| Column | Description |
|--------|-------------|
| `permissions_check_id` | Row ID |
| `service` | Service the permission was checked against |
| `action` | Action that was evaluated |
| `resource` | Resource path that was evaluated |
| `user_id` | Caller's user ID |
| `org_id` | Caller's org ID at the time of the check |
| `team_id` | Caller's team ID at the time of the check |
| `granted` | Whether access was granted |
| `created_at` | Timestamp of the evaluation |

Set `AUDIT_PERMISSION_CHECKS=true` to enable recording. When unset or set to any other value, no rows are written and there is no runtime overhead. The table is append-only; rows are never updated or deleted.

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
