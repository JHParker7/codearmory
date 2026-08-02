# Builder — org-level service control plane

`builder` (`:8095`) lets org admins **enable, disable, configure, register and
remove** services for their org in a user-friendly way. It is the declarative
desired-state store, and its **reconciler** (`reconciler.go`, started from `main`)
turns that desired state into actual per-org workloads: enabling a non-core service
deploys it and registers it with the registry at runtime, with no chart edit.

## Model

A **null/`default` org** is the baseline: its rows form the defaults every org
inherits. A real org's row for the same service **overrides** the baseline. Only
an explicit `enabled: false` blocks a service — anything unconfigured is
**default-on**, so existing behaviour never breaks.

Each `(org, service)` row carries:

- `enabled` — on/off for that org;
- `kind` — `platform` (a toggle/override of a registered service) or `custom`
  (an org-declared service, deployed by the reconciler);
- `config` — free-form JSON the service (or the reconciler) consumes;
- `image` / `port` — for `custom` services.

The control-plane core (`gatekeeper`, `conductor`, `registry`, `builder`) is
always enabled and cannot be toggled off.

## API

All routes are reached through conductor (`/builder/...`) and are authorised by
gatekeeper against the single platform-owned resource
**`codearmory/builder/orgs/default`**.

That resource names the **platform** as owner rather than templating an org id, and
that is deliberate. Conductor can only template a resource from path parameters, so an
`{id}`-shaped resource would have been rewritten to the *caller's* namespace and
matched identically for every org — a per-org gate in appearance only. Builder
administers the instance, so the honest resource is one platform-owned string that only
an admin's grant matches. `builder/api_org_services.go` re-checks the same constant, and
a test guards the two against drifting apart (if they diverge, conductor lets a request
through and builder refuses it, or the manifest loosens and only builder's check still
holds).

| Method | Path | Action |
|---|---|---|
| GET | `/services` | `listOrgServices` |
| GET | `/services/{service}` | `getOrgService` |
| PUT | `/services/{service}` | `configureOrgService` |
| DELETE | `/services/{service}` | `deleteOrgService` |
| PATCH | `/services/{service}/image` | `setOrgServiceImage` |

`PATCH /services/{service}/image` retargets a service at a new image — the endpoint CI
uses to roll out a freshly built tag.

`GET /services` returns the live registry catalog overlaid with the
default baseline and the org's overrides, so the admin UI lists every available
service with its effective state and `source` (`catalog`/`default`/`override`/
`custom`). `DELETE` removes an override (reverting to the baseline) or deletes a
custom service.

Org owners receive the four actions automatically on org creation via the
`grant_on: "org"` default grant in the registry manifest. **Pre-existing orgs**
(created before this feature) need a one-off backfill grant, since org-scoped
default grants are applied at create time, not rebuilt on login.

### Internal

`GET /internal/org-services/effective?org_id=` (shared `BUILDER_INTERNAL_KEY`
bearer) returns the set of services explicitly disabled for an org. Gatekeeper's
disable-gate calls this, caches it briefly, and denies requests to disabled
services. The gate is **config-gated** (inert unless gatekeeper has both
`BUILDER_URL` and `BUILDER_INTERNAL_KEY`) and **fails open**, so a builder outage
never locks the platform.

## Configuration

| Env | Purpose |
|---|---|
| `DATABASE_URL` | Postgres DSN (`builder` database) |
| `GATEKEEPER_URL` / `GATEKEEPER_SERVICE_KEY` | permission checks + service registration |
| `REGISTRY_URL` / `REGISTRY_SERVICE_KEY` | fetch the service catalog (read account) |
| `BUILDER_INTERNAL_KEY` | shared bearer for the gatekeeper disable-gate |

`builder` must appear in gatekeeper's `GATEKEEPER_SERVICES` and registry's
`REGISTRY_SERVICE_ACCOUNTS` (read), and be registered in the registry manifest.

## Reconciler (deploy on enable)

**The Helm chart deploys only the core services** — gatekeeper, conductor, registry,
builder and portal. Every other service is `enabled: true` but `deploy: false`, so the
chart still provisions its **Secret, gatekeeper identity and registry route** (the
"slot") but not its workload. Builder fills the slot on demand: an instance admin
enables a non-core service and the reconciler deploys it; disabling tears it down.
This keeps a fresh install minimal and lets each instance run only what it uses.

The reconciler turns the instance admin's desired state into running workloads:
enabling a non-core service in the **default/admin scope** makes builder deploy it
into the instance namespace; disabling tears it down; reconfiguring rolls it with
the new env. Single-namespace (one instance) — only a platform admin can write the
default scope, so only they drive deploys. **On by default** in the chart
(`builder.reconcile.enabled: true`); fails soft when the cluster is unreachable.

How a workload is built:

- **platform service** (e.g. forge): clone the service's existing base Deployment
  (the Helm-rendered spec — env, secrets, probes), re-namespace it, and apply the
  `config` JSON as env overrides;
- **custom service**: render a minimal Deployment from the row's `image`/`port`/
  `config`, with the standard gatekeeper/db wiring from the conventional
  `<prefix>-<service>` Secret;
- a matching `<prefix>-<service>` ClusterIP Service is created so the existing
  registry routing reaches it. Builder-managed workloads are labelled
  `app.kubernetes.io/managed-by: codearmory-builder`, so the reconciler reclaims
  exactly what it owns.

**Off by default** and **single-namespace**. Config:

| Env | Default | Purpose |
|---|---|---|
| `BUILDER_RECONCILE` | unset (off) | enable the reconcile loop |
| `BUILDER_RECONCILE_INTERVAL` | `30s` | reconcile cadence (also nudged on each admin write) |
| `BUILDER_TARGET_NAMESPACE` | pod namespace | namespace workloads deploy into |
| `BUILDER_RELEASE_PREFIX` | `codearmory` | workload name prefix (`<prefix>-<service>`) |
| `BUILDER_IMAGE_REGISTRY` / `BUILDER_IMAGE_TAG` | — | image source for the template fallback |

The reconciler needs the `serviceAccount.create` Role (Deployments/Services in the
namespace): set `builder.reconcile.enabled=true` and `builder.serviceAccount.create=true`.
It connects via in-cluster config, or `KUBECONFIG` out-of-cluster. Verified live against
a Talos cluster (`TestLiveReconcile`, opt-in via `BUILDER_LIVE_TEST=1`).

## Stateful services (persistent volumes)

Most builder-deployed services keep their state in Postgres and are freely
replaceable. A service that keeps state **on disk** declares it in its def:

```json
"infra": {
  "persistence": {
    "mountPath": "/var/lib/<service>",
    "size": "20Gi",
    "storageClass": "",
    "accessMode": "ReadWriteOnce"
  }
}
```

Builder then:

- **creates the PVC** `<prefix>-<k8sName>-data` if it is absent, and otherwise leaves
  it entirely alone. A PVC's spec is near-immutable (class and access mode can never
  change; capacity only grows, only on an expandable class), so a reconcile must never
  try to converge one — **resizing stays a deliberate admin action**. The claim is
  labelled `infra-of=<service>` so `ListManaged` doesn't mistake it for a stray service;
- **mounts it** at `mountPath`, plus an `emptyDir` at `/tmp`. The scratch mount is
  required, not incidental: workloads run with `readOnlyRootFilesystem: true`, and a
  process that keeps state on disk (git writes lock and temp files constantly) needs
  somewhere writable outside its data directory. The read-only rootfs is kept — mounted
  volumes stay writable regardless, so the hardening costs nothing;
- **never deletes it.** Disabling a service tears down the Deployment/Service/PDB but
  not the data — the same rule the db providers follow (teardown never drops a
  database). Builder is not even granted `delete` on PVCs, so the guarantee holds at the
  RBAC layer, not just in the code path.

### `accessMode` is the design decision

`ReadWriteOnce` means exactly one writer, and that ripples through the rollout:

| | RWO (default) | RWX |
|---|---|---|
| replicas | pinned to **1** (extra pods would never schedule) | as configured |
| strategy | **Recreate** | RollingUpdate (`maxSurge:1`/`maxUnavailable:0`) |
| periodic rotation | **off** | as configured |
| PDB | none (single replica) | `minAvailable: replicas-1` |

The default surge rollout brings a replacement Ready *before* retiring the old pod —
which necessarily overlaps two pods, and the new one cannot attach a volume the old one
still holds. Left alone it deadlocks until `progressDeadlineSeconds` on **every** deploy.
So RWO gets `Recreate`: old pod fully gone, then the new one starts. That is a real
trade — a short outage on each deploy — and the "always N healthy" guarantee is simply
not available for a single-writer volume. The periodic rolling restart is disabled for
the same reason.

`ReadWriteMany` keeps the scalable shape, but only if the backing store gives real
atomic `O_CREAT|O_EXCL` creates and working `flock`. Start RWO; treat RWX as a later
migration once the storage is chosen.

### Ingress stays in Helm

Builder creates **no Ingress**, for stateful services or any other. It owns the
workload (Deployment/Service/PDB/PVC) and the registry route, which is all that
in-cluster traffic through conductor needs. A service that must be reachable directly
from outside — one speaking git-over-HTTP, say, which needs `proxy-body-size: 0` and
long timeouts that conductor's own ingress annotations don't carry — gets its Ingress
from the chart, applied separately. The cost is that such a service is not externally
reachable until someone applies it; the benefit is that builder's scope stays "the
workload", with no annotation-passthrough surface to maintain.

No builder-deployed service needs that Ingress today. git_factory used to be the one
that did, but it is now an in-repo **core** service: the chart owns its Deployment,
Service and Ingress outright (`gitFactory.ingress.enabled=true`), so none of it routes
through builder any more. The escape hatch above still stands for a future service that
needs to be reachable from outside the cluster.

## Dynamic provisioning (no Helm change)

Builder can bring a non-core service online on a running instance with **no Helm
change** — it provisions the two things Helm would otherwise supply, then deploys:

1. **Gatekeeper identity** — builder generates a key and registers it at runtime via
   gatekeeper's `POST /internal/service-accounts` (auth: `BUILDER_INTERNAL_KEY`).
   Gatekeeper's permission check falls back to the live `ServiceAccount` table, so the
   service authenticates without being in `GATEKEEPER_SERVICES`. Idempotent — a
   reconcile reuses the existing key (no pod churn).
2. **Secret** — builder synthesizes `<prefix>-<service>` with the generated
   gatekeeper key, the shared `conductor-forward-key` (read from the conductor
   Secret), and the admin-supplied **DB URL**. It merges into a chart-provisioned
   Secret if one exists, rather than clobbering it.

Builder **never creates the database** — the admin supplies a per-service DB URL for
a role already scoped to an existing database (builder needs no superuser/`CREATEDB`).

### Per-service DB URL (write-only, encrypted)

The admin sets a service's DB URL through the masked field in the org-services TUI
(or `PUT .../services/{service}` with `db_url`). It is:

- **encrypted at rest** — AES-256-GCM under builder's own `BUILDER_SECRETS_KEY`
  (separate from gatekeeper's key), **bound to the service name as AAD** so a stored
  URL can't be swapped between services;
- **write-only** — never returned; `GET` shows only `db_configured` + a redacted
  `db_host`; never logged;
- validated as a `postgres://` URL on entry.

> Hardening planned: a **Vault Transit** backend (key never leaves Vault, built-in
> rotation + audit) behind the same encrypt/decrypt calls, adopted by gatekeeper and
> builder via one toggle. The local AES scheme above is the baseline.

On by default with the reconciler; `BUILDER_PROVISION=false` disables it (e.g. when
the chart pre-provisions every service). Needs `BUILDER_SECRETS_KEY` (auto-generated
by the chart) and the secrets-write RBAC.

### Per-org (future)

Per-org namespaces + per-org routing are intentionally **not** built yet: that needs
an `org_id` JWT claim (gatekeeper) and conductor per-org backend selection. The
desired-state model (`OrgService`, default-baseline inheritance) is forward-compatible
with it.
