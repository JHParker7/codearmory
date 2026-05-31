# Gatekeeper

Authentication and user management service with role-based access control (RBAC) and multi-tenant organisation support.

## Overview

Gatekeeper manages users, organisations, teams, roles, and sessions. Users belong to an organisation and a team, and are assigned a role that carries a set of permission records. Sessions store a signed JWT alongside the public key needed to verify it.

```
Org
 └── Role ──► Permissions (service + actions + resources)
 └── Team ──► Role
      └── User ──► Org, Role, Team
           └── Session (JWT)
```

## Requirements

- Go 1.25+
- PostgreSQL

## Getting Started

```bash
cd src/systems/gatekeeper
DATABASE_URL="postgres://user:pass@localhost:5000/gatekeeper" \
DATABASE_READ_URL="postgres://user:pass@localhost:5001/gatekeeper" \
go run ./...
```

`DATABASE_READ_URL` is optional — omitting it directs all queries to `DATABASE_URL`.

On first run, `main` creates all tables and applies foreign key constraints automatically. Re-running against an existing database is safe — migrations are idempotent.

## Development

All commands run from `src/systems/gatekeeper/`:

```bash
go build ./...          # Build
go test ./...           # Run tests (in-memory SQLite, no database required)
go test -run TestName   # Run a single test
go vet ./...            # Static analysis
```

Integration tests (Python) require a running server:

```bash
pip install -r tests/gatekeeper/requirements.txt
API_URL=http://localhost:8080 pytest tests/gatekeeper/ -v
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `DATABASE_URL` | Yes | PostgreSQL write connection string (`postgres://user:pass@host/db`) |
| `DATABASE_READ_URL` | No | PostgreSQL read-replica connection string. Falls back to `DATABASE_URL` if unset. |
| `REDIS_URL` | No | Redis connection string (`redis://host:6379/0`). Omit to disable caching. |
| `TLS_CERT_FILE` | No | Path to the PEM-encoded TLS certificate. Required together with `TLS_KEY_FILE` to enable HTTPS. |
| `TLS_KEY_FILE` | No | Path to the PEM-encoded TLS private key. Required together with `TLS_CERT_FILE` to enable HTTPS. |
| `TLS_CLIENT_AUTH` | No | Set to `require` to enable mTLS. When set, clients must present a certificate; service accounts can bind to specific fingerprints via `ClientCertFingerprints`. |
| `PERMITTED_SERVICES` | No | Comma-separated allowlist of service names that may appear in `Permissions` records. Defaults to `gatekeeper,blueprints,forge`. |
| `TRUSTED_PROXY_CIDRS` | No | Comma-separated CIDRs of trusted reverse proxies. When set, `X-Forwarded-For` is used to extract the real client IP for service-auth IP allowlist checks. |
| `PORT` | No | Port the server listens on (default: `8080`). |
| `OTEL_SERVICE_NAME` | No | Service name reported in traces and metrics (default: `gatekeeper`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | No | Set to `debug` for verbose output. |
| `GATEKEEPER_SECRETS_KEY` | No | 64 hex chars (32 bytes) AES-256-GCM key for encrypting secrets at rest. Secrets endpoints return 503 if unset. |
| `REGISTRY_URL` | No | Registry service base URL. Required to load default permission grants (org/team owner permissions). |
| `REGISTRY_SERVICE_KEY` | No | Service key for authenticating with the registry to fetch default grants. |

## API

A complete OpenAPI 3.0 spec is at [`openapi.yaml`](./openapi.yaml). Import it into any OpenAPI-compatible tool (Swagger UI, Insomnia, Postman, etc.) for interactive docs.

Extended documentation is in this directory:

| File | Contents |
|------|----------|
| [`architecture.md`](./architecture.md) | Entity model, auth flow, permission resolution, observability |
| [`permissions.md`](./permissions.md) | RBAC model, matching rules, examples, bootstrapping |
| [`development.md`](./development.md) | Local setup, unit tests, integration tests, adding new resources |
| [`deployment.md`](./deployment.md) | Docker Compose, Helm chart, Kubernetes HPA, TLS, environment variables |

Quick reference:

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/signup` | — | Create account |
| `POST` | `/login` | — | Authenticate, get JWT |
| `POST` | `/check_permissions` | ✓ (service-to-service) | Check caller's permission — called directly by backends (e.g. Blueprints, Forge); not routed through Conductor |
| `GET` | `/users` | ✓ | List users (scoped to caller's org) |
| `GET` `PUT` `DELETE` | `/users/{id}` | ✓ | User management |
| `POST` | `/orgs` | ✓ | Create org |
| `GET` | `/orgs` | ✓ | List orgs (scoped to caller's org) |
| `GET` `PUT` `DELETE` | `/orgs/{id}` | ✓ | Org management |
| `GET` `PUT` `DELETE` | `/orgs/{id}/secret-provider` | ✓ | Get, configure, or remove the org's secret provider |
| `POST` | `/teams` | ✓ | Create team |
| `GET` | `/teams` | ✓ | List teams |
| `GET` `PUT` `DELETE` | `/teams/{id}` | ✓ | Team management |
| `POST` | `/roles` | ✓ | Create role |
| `GET` `PUT` `DELETE` | `/roles/{id}` | ✓ | Role management |
| `POST` | `/permissions` | ✓ | Create permissions record |
| `GET` `PUT` `DELETE` | `/permissions/{id}` | ✓ | Permissions management |
| `GET` `DELETE` | `/sessions/{id}` | ✓ | Session management (session owner only) |
| `POST` | `/secrets` | ✓ | Create a secret for the caller's org |
| `GET` | `/secrets` | ✓ | List secrets for the caller's org (values never returned) |
| `PUT` `DELETE` | `/secrets/{id}` | ✓ | Update or soft-delete a secret |
| `POST` | `/orgs/{id}/invites` | ✓ | Invite a user to an org |
| `POST` | `/teams/{id}/invites` | ✓ | Invite a user to a team |
| `GET` | `/invites` | ✓ | List invites (own invites only) |
| `GET` | `/invites/{id}` | ✓ | Get invite (inviter or invitee only) |
| `POST` | `/invites/{id}/accept` | ✓ | Accept invite |
| `POST` | `/invites/{id}/decline` | ✓ | Decline invite |
| `DELETE` | `/invites/{id}` | ✓ | Revoke invite |
| `GET` | `/audit-logs` | ✓ | List audit log entries (paginated, filterable by `actor_id`, `action`, `resource_id`) |
| `POST` | `/service-permission-requests` | Service key | Submit a permission request from a service (authenticated via `X-Service-Key`) |
| `GET` | `/service-permission-requests` | ✓ | List service permission requests (filterable by `service_name`, `status`) |
| `GET` | `/service-permission-requests/{id}` | ✓ | Get a single service permission request |
| `POST` | `/service-permission-requests/{id}/approve` | ✓ | Approve a pending service permission request |
| `POST` | `/service-permission-requests/{id}/decline` | ✓ | Decline a pending service permission request |
| `POST` | `/internal/secrets/resolve` | Service key | Resolve (decrypt) named secrets for an org — called by the workflow worker |

All protected endpoints require `Authorization: Bearer <token>` and enforce RBAC permission checks. On signup, every user automatically receives:

- `getUser`, `updateUser`, `deleteUser` on their own user resource
- `createOrg` on `gatekeeper/orgs`, `createTeam` on `gatekeeper/teams`
- `getState`, `updateState`, `deleteState`, `lockState`, `unlockState` on `states/{username}/*`

All other permissions must be explicitly granted.

Invite endpoints are accessible only to the inviter and the invitee. Accepting an org invite sets `org_id` on the invitee's user record; accepting a team invite sets `team_id`.

`GET /sessions/{id}` and `DELETE /sessions/{id}` enforce session ownership: only the user whose session it is may retrieve or invalidate it. Cross-user access returns 403 even when the caller holds the required permission.

## Secrets

Gatekeeper provides a secrets management layer used by the workflow worker to inject credentials into pipeline runs. All secret values are encrypted at rest using AES-256-GCM. The feature requires `GATEKEEPER_SECRETS_KEY` to be set; all secrets endpoints return `503 Service Unavailable` if it is not.

### Builtin storage (default)

By default, secret values are stored in the `secrets` table, encrypted with the `GATEKEEPER_SECRETS_KEY`. The plaintext value is never returned by any API endpoint — responses include only `secret_id`, `org_id`, `name`, `created_by`, `created_at`, and `updated_at`.

### Provider adapters

Organisations can delegate secret storage to an external provider by configuring one via `PUT /orgs/{id}/secret-provider`. When a provider is configured, `POST /internal/secrets/resolve` fetches secrets from that provider instead of the `secrets` table.

| Provider | Config fields | Notes |
|----------|---------------|-------|
| `builtin` | — | Secrets stored encrypted in the `secrets` table. Default if no provider is configured. |
| `doppler` | `service_token`, `project`, `config` | Fetches each secret from the Doppler API per-request. |
| `vault` | `address`, `token`, `namespace` (optional), `mount` (default: `secret`) | Uses HashiCorp Vault KV v2. |
| `aws_sm` | `region` (optional) | Uses AWS Secrets Manager. Omit `region` to use the IAM role's default region. |

Provider config is stored encrypted in the `org_secret_providers` table. The `GET /orgs/{id}/secret-provider` response returns only the provider name and timestamps — never the config.

### Default permission grants

When `REGISTRY_URL` and `REGISTRY_SERVICE_KEY` are set, gatekeeper polls the registry's `GET /default-grants` endpoint every 5 minutes. These grants are applied automatically when orgs and teams are created, so users receive owner-level permissions without manual bootstrapping.

## Deployment

The Helm chart lives at `infra/helm/gatekeeper`. It deploys gatekeeper with an nginx Ingress load balancer and a HorizontalPodAutoscaler (min 3, max 10 replicas) driven by CPU and memory utilisation.

**Prerequisites:** `ingress-nginx` and `metrics-server` must be running in the cluster.

```bash
helm install gatekeeper ./infra/helm/gatekeeper \
  --set database.url="postgres://user:pass@host/gatekeeper" \
  --set ingress.host="gatekeeper.example.com" \
  --set env.OTEL_EXPORTER_OTLP_ENDPOINT="http://otelcol:4318"
```

Key values:

| Value | Default | Description |
|-------|---------|-------------|
| `image.repository` / `image.tag` | `gatekeeper:0.0.4` | Container image |
| `ingress.host` | `""` | Hostname the Ingress routes |
| `ingress.className` | `nginx` | Ingress controller class |
| `ingress.tls.enabled` | `false` | Enable HTTPS |
| `ingress.tls.secretName` | `""` | TLS Secret name (e.g. from cert-manager) |
| `ingress.annotations` | `{}` | Extra Ingress annotations |
| `autoscaling.minReplicas` | `3` | Minimum pod count |
| `autoscaling.maxReplicas` | `10` | Maximum pod count |
| `autoscaling.targetCPUUtilizationPercentage` | `70` | CPU scale target |
| `autoscaling.targetMemoryUtilizationPercentage` | `80` | Memory scale target |
| `database.url` | `""` | Creates a Secret with this value |
| `database.existingSecret` | `""` | Use a pre-existing Secret instead |

To enable TLS with cert-manager:

```yaml
ingress:
  host: gatekeeper.example.com
  tls:
    enabled: true
    secretName: gatekeeper-tls
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
```

## Schema

| Table                 | Primary Key            | Foreign Keys                                                  | Description |
|-----------------------|------------------------|---------------------------------------------------------------|-------------|
| `orgs`                | `org_id`               | —                                                             | |
| `roles`               | `role_id`              | `org_id` → `orgs`                                            | |
| `teams`               | `team_id`              | `role_id` → `roles`                                          | |
| `users`               | `user_id`              | `org_id` → `orgs`, `role_id` → `roles`, `team_id` → `teams` | |
| `sessions`            | `session_id`           | `user_id` → `users`                                          | |
| `permissions`         | `permissions_id`       | —                                                             | |
| `invites`             | `invite_id`            | `inviter_id` → `users`                                       | |
| `permissions_checks`  | `permissions_check_id` | `user_id` → `users`, `org_id` → `orgs`, `team_id` → `teams` | |
| `secrets`             | `secret_id`            | `org_id` → `orgs`                                            | AES-256-GCM encrypted org secrets. Value never returned by API. |
| `org_secret_providers`| `org_id`               | `org_id` → `orgs`                                            | Per-org secret provider config (builtin/doppler/vault/aws_sm). Config stored encrypted. |

`users.email` and `users.username` have unique indexes. All tables use a soft-delete `active` column. Schema is defined in Go via GORM struct tags in `src/systems/gatekeeper/types.go`.
