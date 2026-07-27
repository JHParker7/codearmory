# Projects — first-class resource grouping for access control

## Why

"Project" already exists in the platform, but only as a **free-text view filter**: a
`project` string column on pipelines (workflows), executions (forge), and tickets, used
to filter list views and explicitly commented *"not a security boundary"*. There is no
projects table, no membership, and it is never consulted in a permission check.

The original intent was always to let **permissions apply to a collection of
resources** — grant a user a role on a project and they get scoped access to that
project's repos (down to specific branches), CI/CD pipelines, and boards, without
handing out per-resource grants one at a time.

This document makes `project` a **first-class entity** and a **permission scope**,
built entirely on the namespace-role primitives already in gatekeeper.

## What already exists (the foundation we build on)

Gatekeeper (post the `dev` merge) already has everything a project scope needs:

- **Resource strings** are `{namespace}/{service}/{collection}/{id}`, where a namespace
  is a user's `username` or their org's `org/{name}`. `scopeResource` auto-prepends the
  caller's namespace unless the resource already carries one
  (`api_helpers.go`).
- **Wildcard matching** — `matchPermission` matches `*`, trailing-prefix `create*`,
  resource prefix `foo/*`, and per-segment `foo/*/bar`. So a single permission on
  `alice/workflows/projects/web/*` covers every pipeline in project `web`.
- **Namespace roles** (`POST /roles/namespace`) — a user mints a role holding
  permissions over resources *in a namespace they own*, guarded by **confinement**
  (resource must start with your namespace) and **attenuation** (you may only grant a
  `(service, action, resource)` you already hold). `attenuatedPermissions` enforces
  both.
- **Role membership** — `PUT/DELETE /roles/{id}/members/{user_id}`, `GET
  /roles/{id}/members`, backed by `RoleMembership{RoleID, UserID, GrantedBy}`. Only the
  role's owner may assign it.
- **Scoped personal tokens** (`POST /tokens`) — a user mints a token bound to an
  attenuated role; the same `attenuatedPermissions` path.

A project is therefore **not new access machinery** — it is a *named grouping* that
these primitives target.

## Model

### Project entity (gatekeeper-owned)

```
type Project struct {
    ProjectID string    // uuid
    Slug      string    // url/resource-safe handle, unique within the namespace
    Name      string    // display name
    Namespace string    // owning namespace: "<username>" or "org/<orgName>"
    OwnerID   string    // gatekeeper user_id of the creator
    CreatedAt time.Time
}
// unique (Namespace, Slug)
```

`Slug` is what appears in resource strings; `Namespace` is what scopes it and what
confinement checks against. Projects live under a namespace exactly like repos do, so
the org that owns a project owns its scope.

### The project resource segment

A project is its **own top-level namespace** — `project/{slug}/…` — not nested under any
user, exactly like `org/{name}/…`. `scopeResource` recognises the `project/` prefix and
leaves it untouched (never re-scoping it to the caller), which is what lets a member in a
different personal namespace match the grant. Every participating service builds:

```
project/{slug}/{service}/{collection}/{id}
```

Because there is no middle wildcard, a **single** permission `project/{slug}/*` covers the
whole project across every service (matchPermission's trailing `/*` is a literal prefix).
Examples (project `core`):

| resource                                              | grants                          |
|-------------------------------------------------------|---------------------------------|
| `project/core/workflows/pipelines/42`                 | one pipeline                    |
| `project/core/workflows/pipelines/*`                  | all pipelines in `core`         |
| `project/core/tickets/boards/*`                       | all boards in `core`            |
| `project/core/codearmory_git_factory/repos/*`         | all repos in `core`             |
| `project/core/codearmory_git_factory/repos/{id}/branches/dev` | one branch of one repo |
| `project/core/*`                                      | the entire project, all services (one grant) |

A resource **without** a project falls back to today's
`{namespace}/{service}/{collection}/{id}` — projects are additive, nothing breaks. A
project is **not bound to a user**: the creator becomes its first admin member, and an
optional org owns/manages it, but the resource path never contains a username.

### Branch-level repo access

Repos extend the segment with the ref:

```
{git_service}/projects/{slug}/repos/{repo}/branches/{branch}
```

The git smart-HTTP handler already knows the ref being pushed/fetched (it is in
`info/refs` / the pack negotiation), so it checks `writeRepo` on
`.../repos/{id}/branches/{ref}`. A project **developer** role holds
`.../repos/*/branches/*`; a **release-only** role holds `.../repos/*/branches/main`.
Read (`readRepo`) is checked at `.../repos/{id}` granularity (clone is all-or-nothing
per repo); write is per-branch.

### Project roles

A project ships three built-in roles, each a namespace role whose permissions are the
project-scoped wildcards for that tier. On `POST /projects` gatekeeper provisions them
through the **same attenuated path** as `handleCreateNamespaceRole` (the creator owns
`{ns}/*` so attenuation passes):

- **viewer** — `read*` / `list*` / `get*` on `{ns}/*/projects/{slug}/*`.
- **developer** — viewer + `write`/`create`/`update` on pipelines, boards, tickets,
  and `writeRepo` on `.../repos/*/branches/*`.
- **admin** — developer + `delete*` and project membership management.

Members are attached with the existing `PUT /roles/{id}/members/{user_id}`. "Give a
user a project role" = assign them the project's viewer/developer/admin role. No new
membership machinery.

### How a resource joins a project

Resources keep the existing free-text `project` column, now interpreted as a **project
slug within the resource's namespace** rather than a free label:

- workflows pipelines, forge executions, tickets/boards already have the column.
- git_factory repos gain a `project` column (nullable — repos may be unfiled).

A resource with `project = "core"` is in project `core`; its RBAC resource string is
built with the `projects/core` segment. Setting/clearing the field moves a resource
in/out of a project (guarded by `updateProject` on both the old and new project).

## API (gatekeeper)

```
POST   /projects                       create a project (+ provision viewer/developer/admin roles)
GET    /projects                       list projects in the caller's namespaces
GET    /projects/{id}                  get a project (incl. its role ids + members)
PUT    /projects/{id}                  rename
DELETE /projects/{id}                  delete (cascades its roles)
GET    /projects/{id}/roles            the three project roles + their members
POST   /projects/{id}/members          { user_id, tier } → assigns the tier's role
DELETE /projects/{id}/members/{user_id} revoke every project role from the user
```

`/projects/{id}/members` is sugar over `/roles/{id}/members/{user_id}` that resolves
`tier` → role id, so callers think in "add Bob as a developer on project core", not in
role uuids.

RBAC for the project API itself reuses the namespace gate: `createProject` /
`updateProject` / `deleteProject` on `{username}/gatekeeper/projects`, mirroring how
`createNamespaceRole` gates on `{username}/gatekeeper/roles`.

## Service wiring

Each service changes only *how it builds the resource string* for its existing
`CheckPermissions` calls — the check, the action names, and the fallback are unchanged:

```
res := svc + "/" + collection + "/" + id                    // today
if project != "" {
    res = svc + "/projects/" + project + "/" + collection + "/" + id   // project-scoped
}
```

- **workflows** — pipelines/runs use `pipeline.Project`.
- **tickets** — boards/tickets/comments use `board.Project`.
- **forge** — executions use the `project` label.
- **git_factory** — repos use `repo.Project`; the git-http handler adds the
  `branches/{ref}` suffix for `writeRepo`.

Registry manifest / default grants gain the project-scoped resource patterns so the
built-in project roles validate against declared actions.

## Migration & compatibility

1. `project` values that already exist become slugs. A one-off backfill can create a
   `Project` row per distinct `(namespace, project)` already in use so they show up in
   the API; unfiled resources stay unfiled.
2. Every resource-string change is *additive with fallback* — existing grants on
   `{ns}/{service}/{collection}/{id}` keep working for unfiled resources; only
   project-tagged resources move under the project segment, and their project roles
   carry the matching wildcard.
3. git_factory repos gaining `org_id`/`project` is the one schema addition on a service
   that is currently owner-scoped only; it stays nullable and owner-scoping remains the
   default when no project is set.

## The concrete first use

Grant `jhparker7` access to the admin's repos + pipelines + boards:

1. `POST /projects { slug: "core", namespace: "admin" }` → provisions `core` +
   viewer/developer/admin roles.
2. Tag the admin's `codearmory` / `codearmory-git-factory` repos, the CI pipelines, and
   the boards with `project = "core"`.
3. `POST /projects/{id}/members { user_id: "89d9085a-…", tier: "developer" }`.

`jhparker7` can now clone/push the admin's repos and run/see the project's pipelines and
boards — with one grant, scoped to `core`, revocable in one call.
