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
git clone <repo>
cd gatekeeper/src
DATABASE_URL="postgres://user:pass@localhost:5000/gatekeeper" \
DATABASE_READ_URL="postgres://user:pass@localhost:5001/gatekeeper" \
go run ./...
```

`DATABASE_READ_URL` is optional — omitting it directs all queries to `DATABASE_URL`.

On first run, `main` creates all tables and applies foreign key constraints automatically. Re-running against an existing database is safe — migrations are idempotent.

## Development

All commands run from `src/`:

```bash
go build ./...          # Build
go test ./...           # Run tests (in-memory SQLite, no database required)
go test -run TestName   # Run a single test
go vet ./...            # Static analysis
```

Integration tests (Python) require a running server:

```bash
pip install -r tests/requirements.txt
API_URL=http://localhost:8080
DATABASE_URL="postgres://user:pass@localhost/gatekeeper"
# ---------- if testing local 
cd infra/local
docker compose up
cd ../..
# ----------
pytest tests/
```

## API

A complete OpenAPI 3.0 spec is at [`docs/openapi.yaml`](./docs/openapi.yaml). Import it into any OpenAPI-compatible tool (Swagger UI, Insomnia, Postman, etc.) for interactive docs.

Extended documentation is in [`docs/`](./docs/):

| File | Contents |
|------|----------|
| [`architecture.md`](./docs/architecture.md) | Entity model, auth flow, permission resolution, observability |
| [`permissions.md`](./docs/permissions.md) | RBAC model, matching rules, examples, bootstrapping |
| [`development.md`](./docs/development.md) | Local setup, unit tests, integration tests, adding new resources |
| [`deployment.md`](./docs/deployment.md) | Docker Compose, Helm chart, Kubernetes HPA, TLS, environment variables |

Quick reference:

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/signup` | — | Create account |
| `POST` | `/login` | — | Authenticate, get JWT |
| `GET` | `/check_permissions` | ✓ | Check caller's permission |
| `GET` `PUT` `DELETE` | `/users/{id}` | ✓ | User management |
| `POST` | `/orgs` | ✓ | Create org |
| `GET` `PUT` `DELETE` | `/orgs/{id}` | ✓ | Org management |
| `POST` | `/teams` | ✓ | Create team |
| `GET` `PUT` `DELETE` | `/teams/{id}` | ✓ | Team management |
| `POST` | `/roles` | ✓ | Create role |
| `GET` `PUT` `DELETE` | `/roles/{id}` | ✓ | Role management |
| `POST` | `/permissions` | ✓ | Create permissions record |
| `GET` `PUT` `DELETE` | `/permissions/{id}` | ✓ | Permissions management |
| `GET` `DELETE` | `/sessions/{id}` | ✓ | Session management |
| `POST` | `/orgs/{id}/invites` | ✓ | Invite a user to an org |
| `POST` | `/teams/{id}/invites` | ✓ | Invite a user to a team |
| `GET` | `/invites/{id}` | ✓ | Get invite (inviter or invitee only) |
| `POST` | `/invites/{id}/accept` | ✓ | Accept invite |
| `POST` | `/invites/{id}/decline` | ✓ | Decline invite |
| `DELETE` | `/invites/{id}` | ✓ | Revoke invite |

All protected endpoints require `Authorization: Bearer <token>` and enforce RBAC permission checks. On signup, every user automatically receives `getUser`, `updateUser`, and `deleteUser` on their own user resource. All other permissions must be explicitly granted.

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

| Table         | Primary Key      | Foreign Keys                                                  |
|---------------|------------------|---------------------------------------------------------------|
| `orgs`        | `org_id`         | —                                                             |
| `roles`       | `role_id`        | `org_id` → `orgs`                                            |
| `teams`       | `team_id`        | `role_id` → `roles`                                          |
| `users`       | `user_id`        | `org_id` → `orgs`, `role_id` → `roles`, `team_id` → `teams` |
| `sessions`    | `session_id`     | `user_id` → `users`                                          |
| `permissions` | `permissions_id` | —                                                             |
| `invites`     | `invite_id`      | `inviter_id` → `users`                                       |

`users.email` and `users.username` have unique indexes. All tables use a soft-delete `active` column. Schema is defined in Go via GORM struct tags in `src/types.go`.
