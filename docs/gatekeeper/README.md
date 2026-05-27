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
| `GET` | `/check_permissions` | ✓ | Check caller's permission |
| `GET` | `/users` | ✓ | List users |
| `GET` `PUT` `DELETE` | `/users/{id}` | ✓ | User management |
| `POST` | `/orgs` | ✓ | Create org |
| `GET` | `/orgs` | ✓ | List orgs |
| `GET` `PUT` `DELETE` | `/orgs/{id}` | ✓ | Org management |
| `POST` | `/teams` | ✓ | Create team |
| `GET` | `/teams` | ✓ | List teams |
| `GET` `PUT` `DELETE` | `/teams/{id}` | ✓ | Team management |
| `POST` | `/roles` | ✓ | Create role |
| `GET` `PUT` `DELETE` | `/roles/{id}` | ✓ | Role management |
| `POST` | `/permissions` | ✓ | Create permissions record |
| `GET` `PUT` `DELETE` | `/permissions/{id}` | ✓ | Permissions management |
| `GET` `DELETE` | `/sessions/{id}` | ✓ | Session management |
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

All protected endpoints require `Authorization: Bearer <token>` and enforce RBAC permission checks. On signup, every user automatically receives:

- `getUser`, `updateUser`, `deleteUser` on their own user resource
- `createOrg` on `gatekeeper/orgs`, `createTeam` on `gatekeeper/teams`
- `getState`, `updateState`, `deleteState`, `lockState`, `unlockState` on `blueprints/states/{username}/*`

All other permissions must be explicitly granted. When a user creates an org, they additionally receive full state access on `blueprints/{org}/states/*`.

Invite endpoints are accessible only to the inviter and the invitee. Accepting an org invite sets `org_id` on the invitee's user record; accepting a team invite sets `team_id`.

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

| Table                 | Primary Key            | Foreign Keys                                                  |
|-----------------------|------------------------|---------------------------------------------------------------|
| `orgs`                | `org_id`               | —                                                             |
| `roles`               | `role_id`              | `org_id` → `orgs`                                            |
| `teams`               | `team_id`              | `role_id` → `roles`                                          |
| `users`               | `user_id`              | `org_id` → `orgs`, `role_id` → `roles`, `team_id` → `teams` |
| `sessions`            | `session_id`           | `user_id` → `users`                                          |
| `permissions`         | `permissions_id`       | —                                                             |
| `invites`             | `invite_id`            | `inviter_id` → `users`                                       |
| `permissions_checks`  | `permissions_check_id` | `user_id` → `users`, `org_id` → `orgs`, `team_id` → `teams` |

`users.email` and `users.username` have unique indexes. All tables use a soft-delete `active` column. Schema is defined in Go via GORM struct tags in `src/systems/gatekeeper/types.go`.
