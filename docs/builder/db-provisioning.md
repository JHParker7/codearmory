# Builder database provisioning

Builder deploys non-core services at runtime. Each of those services needs a
PostgreSQL database. Historically builder never created one: the admin
pre-created a database + a role scoped to it and PUT a per-service `db_url`,
which builder encrypted (AES-256-GCM, `BUILDER_SECRETS_KEY`, AAD = service name)
and wrote into the service's k8s Secret as `database-url`. The service then
auto-migrates its own tables (`CREATE TABLE IF NOT EXISTS`) — so the *database*
must already exist, only the schema is self-creating.

This document describes the pluggable **db backend** that lets builder create
databases automatically, while letting security-strict environments remove
builder from the credential-custody chain entirely. The default is unchanged
(`manual`): nothing here alters existing installs until an admin opts in.

## The custody problem

A working credential has to exist *somewhere* — builder can't create a database
or connect a pod without one passing through *something*. The goal is therefore
not to make the secret vanish but to choose **who is the durable custodian** and
**for how long**. The backends below sit on a spectrum from "builder holds the
credential" to "no long-lived credential exists anywhere".

## The one architectural lever

The pod's `DATABASE_URL` is wired as an **optional `secretKeyRef`** (see
`templatePod` in `cluster.go`). That already decouples *what Secret the pod
reads* from *who fills it*. So every hardening backend only changes **which
Secret the `DATABASE_URL` ref points at** and **who owns it** — the Deployment
spec is otherwise untouched. Adding a backend never ripples into the deploy path.

## Backends

Selected globally with `BUILDER_DB_BACKEND` (default `manual`), overridable
per service with the `dbBackend` config key. The two delivery models:

- **builder-owned Secret** (`manual`, `sql`): builder writes `database-url` into
  the `codearmory-<svc>` Secret it already manages. Existing wiring.
- **foreign Secret reference** (`cnpg`, `external`): `DATABASE_URL` points at a
  *different* Secret that an operator / external-secrets controller owns and
  fills. Builder creates the resource that makes that Secret exist, but stores
  no credential.

| backend    | who creates the DB                         | who holds the credential        | builder stores at rest |
| ---------- | ------------------------------------------ | ------------------------------- | ---------------------- |
| `manual`   | admin, by hand                             | builder (encrypted db_url)      | encrypted db_url       |
| `sql`      | builder, `CREATE DATABASE` (once, at enable) | k8s Secret (etcd)             | encrypted derived url  |
| `cnpg`     | CloudNativePG operator (declarative CR)    | operator-generated Secret       | nothing — a CR + a Secret name |
| `external` | admin / Vault DB engine                    | Vault / external store          | nothing — a reference  |

### `manual` (default, unchanged)

Passthrough. The admin supplies `db_url`; builder behaves exactly as before.

### `sql` — any Postgres / YugabyteDB

Builder holds **one** maintenance connection (a role with `CREATEDB`, plus
`CREATEROLE` unless `BUILDER_DB_SQL_CREATE_ROLE=false`). On enable, builder:

1. Pre-flight checks `current_user` has the needed privileges and is **not** a
   superuser (refuses superuser — never store one).
2. Idempotently ensures a per-service role `svc_<service>` with a freshly
   generated base64url password (`SELECT 1 FROM pg_roles …` then `CREATE ROLE`).
3. Idempotently ensures the database owned by that role
   (`SELECT 1 FROM pg_database …` then `CREATE DATABASE … OWNER …`).
4. Derives the per-service URL and stores it **exactly where `db_url` lives
   today** (encrypted `db_url_ct`). From then on the service behaves like
   `manual` — the reconciler never re-runs DDL.

Portability: `CREATE ROLE` / `CREATE DATABASE … OWNER` / privilege checks are
plain SQL that work on RDS, Cloud SQL, Azure, self-hosted Postgres, and
YugabyteDB (wire-compatible). Making the service role the **owner** of its
database sidesteps the Postgres 15 `public`-schema change cleanly.

Maintenance credential lifetime:

- **ephemeral** (recommended): supplied in the enable request, used once, never
  persisted. Steady-state blast radius equals today's. Cost: re-provisioning
  needs the admin to re-supply it.
- **stored** (`BUILDER_DB_SQL_MAINTENANCE_URL`): convenient for unattended
  re-provisioning; builder becomes a durable custodian of a `CREATEDB`/`CREATEROLE`
  credential — treat it as the crown jewel (KMS/Vault-wrap `BUILDER_SECRETS_KEY`).

Builder **never** issues `DROP DATABASE`/`DROP ROLE`. Disabling a service leaves
its data intact; destructive cleanup is a deliberate, out-of-band admin action.

### `cnpg` — CloudNativePG operator

Builder creates a CNPG `Database` CR (and, when configured, relies on a managed
role) via the dynamic client, then points `DATABASE_URL` at the
operator-generated Secret. Builder's privilege drops from "a Postgres
credential" to "**k8s RBAC on CNPG CRs in one namespace**" — a much smaller
blast radius — and self-heal works because the operator owns the Secret durably.
YugabyteDB has its own operator and is **not** CNPG; use the `sql` backend for it.

Config: `BUILDER_DB_CNPG_CLUSTER`, `BUILDER_DB_CNPG_NAMESPACE`,
`BUILDER_DB_CNPG_CRED_SECRET_TEMPLATE` (e.g. `{cluster}-{service}`),
`BUILDER_DB_CNPG_CRED_KEY` (default `uri`).

### `external` — External Secrets Operator / Vault

Builder creates an `ExternalSecret` (ESO) referencing a store + path the admin
populated (a static db_url, or a Vault DB-engine dynamic role for short-lived,
auto-revoked credentials). ESO materializes the k8s Secret; builder holds only a
reference. Nothing long-lived lives in builder.

Config: `BUILDER_DB_EXTERNAL_STORE_REF`, `BUILDER_DB_EXTERNAL_STORE_KIND`
(`SecretStore` | `ClusterSecretStore`), `BUILDER_DB_EXTERNAL_PATH_TEMPLATE`
(e.g. `codearmory/{service}/db`), `BUILDER_DB_EXTERNAL_PROPERTY` (key within the
remote secret holding the URL), `BUILDER_DB_EXTERNAL_REFRESH` (e.g. `1h`).

## Blast radius summary

- `manual`/`sql` keep builder a custodian of per-service URLs (it already is
  today). `sql`'s only marginal exposure is the maintenance credential — removed
  entirely in ephemeral mode except during the provisioning call.
- `cnpg`/`external` remove builder from custody: it holds a k8s RBAC grant or a
  reference, never a database credential.
- `CREATEROLE` is the sharp privilege: on Postgres ≤ 15 it is close to
  superuser-dangerous; on 16+ it is scoped to roles it created. Prefer the
  `CREATEDB`-only profile (`BUILDER_DB_SQL_CREATE_ROLE=false` + a pre-created
  owner role) where per-service role isolation is not required.
- Builder never auto-drops, so a reconciler bug cannot delete data.

## Verification

Unit tests cover identifier validation, URL derivation, password generation, the
SQL provisioning orchestration (against a fake connection), CR/ExternalSecret
rendering (fake dynamic client), config parsing, and foreign-ref pod wiring.
Live verification — `CREATE DATABASE` against a real Postgres/Yugabyte, CNPG CR
acceptance, ESO reconciliation — must be done against the target cluster.
