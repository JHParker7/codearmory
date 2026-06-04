# Gitea Integration

Forgejo/Gitea integration service. Provides an authenticated API for managing repositories, pull requests, and account linking on a Forgejo (or Gitea) instance. Also proxies Git HTTP smart protocol operations so `git push`, `git pull`, and `git clone` can be routed through the platform.

## How it works

Users first link their CodeArmory account to their Forgejo account by providing a Forgejo personal access token. Subsequent API calls are proxied to Forgejo using the admin token with a per-user `Sudo` header, so all operations are performed as the authenticated user's Forgejo identity.

```
Authenticated CA user
  │
  └── PUT /account ──────────────────────► Gitea Integration :8088
        │  1. Verify Bearer token via Gatekeeper
        │  2. Verify Forgejo token against GET /api/v1/user (no Sudo)
        │  3. Store username ↔ user_id mapping in PostgreSQL
        │
  └── POST /repos ────────────────────────► Gitea Integration :8088
        │  1. Verify Bearer token
        │  2. Resolve caller's linked Gitea username from DB
        │  3. Forward to Forgejo API with Sudo: <gitea_username>
        └──► Forgejo instance (GITEA_URL)
```

## Requirements

- Go 1.25+
- PostgreSQL
- A running Forgejo or Gitea instance with an admin API token

## Configuration

| Variable | Default | Description |
|---|---|---|
| `GITEA_URL` | — | **Required.** Base URL of the Forgejo/Gitea instance (e.g. `https://git.example.com`). |
| `GITEA_ADMIN_TOKEN` | — | **Required.** Admin API token. Used with `Sudo` to perform operations on behalf of linked users. |
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/gitea_integration` | PostgreSQL connection string for the account link table. |
| `GATEKEEPER_URL` | `http://localhost:8081` | Gatekeeper base URL for permission checks. |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for gatekeeper key rotation. |
| `PORT` | `8088` | Port the server listens on. |
| `OTEL_SERVICE_NAME` | `gitea_integration` | OTel service name. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | — | Enable HTTPS. |
| `TLS_CLIENT_AUTH` | — | Set to `require` for mTLS. |
| `CA_CERT_FILE` | — | CA certificate for verifying the Forgejo TLS certificate when `GITEA_URL` uses HTTPS. |

All variables support a `_FILE` suffix variant (e.g. `GITEA_ADMIN_TOKEN_FILE`).

## Running locally

```bash
cd src/systems/gitea_integration
DATABASE_URL=postgresql://postgres:pass@localhost:5432/gitea_integration \
  GITEA_URL=http://localhost:3000 \
  GITEA_ADMIN_TOKEN=your-admin-token \
  GATEKEEPER_URL=http://localhost:8081 \
  go run .
```

## Docker

```bash
cd src/systems/gitea_integration
docker build -t gitea-integration:latest .

docker run -p 8088:8088 \
  -e DATABASE_URL=postgresql://postgres:pass@db:5432/gitea_integration \
  -e GITEA_URL=https://git.example.com \
  -e GITEA_ADMIN_TOKEN_FILE=/run/secrets/gitea-token \
  -e GATEKEEPER_URL=http://gatekeeper:8081 \
  -e GATEKEEPER_SERVICE_KEY=gitea-integration-secret \
  gitea-integration:latest
```

## Account linking

Before calling any repository or pull request endpoint, a user must link their CodeArmory account to their Forgejo account. Linking requires a Forgejo personal access token — this proves the user controls the Forgejo account they claim. The token is used once for verification and is never stored.

```bash
# Link account
curl -X PUT http://localhost:8088/account \
  -H "Authorization: Bearer <ca-token>" \
  -H "Content-Type: application/json" \
  -d '{"gitea_username": "alice", "gitea_token": "<forgejo-pat>"}'

# Get linked account
curl http://localhost:8088/account \
  -H "Authorization: Bearer <ca-token>"

# Unlink account
curl -X DELETE http://localhost:8088/account \
  -H "Authorization: Bearer <ca-token>"
```

## API

All endpoints require `Authorization: Bearer <token>` verified by Gatekeeper. Repository and pull request endpoints additionally require the caller to have a linked Forgejo account (returns `422` otherwise).

### Account

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/account` | `getAccount` on `gitea_integration/account` | Get the caller's linked Forgejo account |
| `PUT` | `/account` | `linkAccount` on `gitea_integration/account` | Link (or re-link) the caller's Forgejo account |
| `DELETE` | `/account` | `unlinkAccount` on `gitea_integration/account` | Remove the account link |

### Repositories

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/repos` | `listRepo` on `gitea_integration/repos` | List the caller's repositories |
| `POST` | `/repos` | `createRepo` on `gitea_integration/repos` | Create a repository |
| `GET` | `/repos/{owner}/{name}` | `getRepo` on `gitea_integration/repos/{owner}/{name}` | Get a repository |
| `DELETE` | `/repos/{owner}/{name}` | `deleteRepo` on `gitea_integration/repos/{owner}/{name}` | Delete a repository |
| `GET` | `/repos/{owner}/{name}/branches` | `listBranch` on `gitea_integration/repos/{owner}/{name}` | List branches |
| `GET` | `/repos/{owner}/{name}/tags` | `listTag` on `gitea_integration/repos/{owner}/{name}` | List tags |
| `GET` | `/repos/{owner}/{name}/releases` | `listRelease` on `gitea_integration/repos/{owner}/{name}` | List releases |
| `GET` | `/repos/{owner}/{name}/commits` | `listCommit` on `gitea_integration/repos/{owner}/{name}` | List commits (supports `?page=` and `?limit=`) |

### Pull Requests

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/repos/{owner}/{name}/pulls` | `listPull` on `gitea_integration/repos/{owner}/{name}/pulls` | List pull requests. Supports `?state=open\|closed\|all` (default: `open`). |
| `POST` | `/repos/{owner}/{name}/pulls` | `createPull` on `gitea_integration/repos/{owner}/{name}/pulls` | Create a pull request |
| `GET` | `/repos/{owner}/{name}/pulls/{index}` | `getPull` on `gitea_integration/repos/{owner}/{name}/pulls/{index}` | Get a pull request |
| `POST` | `/repos/{owner}/{name}/pulls/{index}/merge` | `mergePull` on `gitea_integration/repos/{owner}/{name}/pulls/{index}` | Merge a pull request |

### Git HTTP smart protocol

| Path prefix | Description |
|---|---|
| `/{owner}/{name}.git/info/refs` | Git smart HTTP discovery (`git fetch`, `git clone`) |
| `/{owner}/{name}.git/git-upload-pack` | Git fetch/clone pack exchange |
| `/{owner}/{name}.git/git-receive-pack` | Git push pack exchange |

Git operations are proxied to the Forgejo instance. Authentication is handled by Forgejo directly — clients authenticate with their Forgejo credentials, not their CodeArmory token.

## Owner scoping

`{owner}` in repository paths can be:
- The caller's own Forgejo username — for personal repositories.
- The caller's CodeArmory org name — for org repositories, provided the org name matches the Forgejo organisation name.

Any other `{owner}` value returns `403 Forbidden`.

## Integration tests

Integration tests for this service require a live Forgejo instance and a Forgejo admin token. They are designed to run against a k8s or compose deployment:

```bash
pip install -r tests/gitea_integration/requirements.txt

GITEA_INTEGRATION_URL=http://gitea-integration:8088 \
GATEKEEPER_URL=http://gatekeeper:8081 \
GITEA_URL=http://forgejo:3000 \
GITEA_ADMIN_TOKEN=<admin-token> \
  pytest tests/gitea_integration/ -v
```

## Schema

| Table | Primary Key | Description |
|-------|-------------|-------------|
| `gitea_accounts` | `user_id` | Maps CodeArmory `user_id` to a Forgejo `gitea_username`. |

## Testing

```bash
# Unit tests
cd src/systems/gitea_integration
go test ./...
```
