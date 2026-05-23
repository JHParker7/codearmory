# Development

## Prerequisites

- Go 1.22+
- Docker and Docker Compose (for the local stack)
- Python 3.10+ and pip (for integration tests)

## Local Setup

Start the full local stack (PostgreSQL, OTel Collector, Loki, Tempo, Prometheus, Grafana):

```bash
cd infra/local
docker compose up -d
cd ../..
```

Run the server against it:

```bash
cd src
DATABASE_URL="postgresql://postgres:test@127.0.0.1:5000/postgres" \
DATABASE_READ_URL="postgresql://postgres:test@127.0.0.1:5001/postgres" \
OTEL_SERVICE_NAME=gatekeeper \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
go run ./...
```

The server starts on `:8080`. On first run it creates all tables and foreign key constraints automatically — subsequent starts are safe to run against an existing database.

Grafana is available at http://localhost:3000 (anonymous admin). Tempo, Prometheus, and Loki datasources are pre-provisioned.

## Go Commands

All commands run from `src/`:

```bash
go build ./...          # Compile
go test ./...           # Run all unit tests (in-memory SQLite, no database needed)
go test -run TestName   # Run a single test by name
go test -v ./...        # Verbose output
go vet ./...            # Static analysis
```

## Unit Tests

Tests use an in-memory SQLite database wired up in `TestMain` (`src/db_test.go`). No running database or server is needed.

Key test helpers in `src/api_helpers_test.go`:

| Helper | Purpose |
|--------|---------|
| `createTestUser(t)` | Inserts a user with no role; cleans up on test completion |
| `createAuthorizedUser(t, action, resource)` | Inserts permission + role + user granting exactly one action/resource |
| `createAuthorizedUserViaTeam(t, action, resource)` | Same but permission comes through a team role (user has no direct `role_id`) |
| `makeSession(t, userID, expiresAt)` | Generates ECDSA key pair, signs JWT, inserts session |
| `withUserID(r, userID)` | Injects a user ID into request context (simulates `authMiddleware`) |

## Integration Tests

Integration tests run against a live server and require PostgreSQL. With the local stack running:

```bash
pip install -r tests/requirements.txt

API_URL=http://localhost:8080 pytest tests/ -v
```

Test fixtures (`tests/conftest.py`):

| Fixture | Scope | Description |
|---------|-------|-------------|
| `base_url` | session | API base URL from `API_URL` env var |
| `admin_token` | session | Admin user with wildcard permissions, seeded directly via DB |
| `new_user` | function | Fresh user created via `POST /signup` |
| `token` | function | JWT for `new_user` |
| `base_url_delete_fixture` | function | Dedicated user for deletion tests |

`pytest_sessionfinish` cleans up all test data (users, sessions, roles, permissions, teams, orgs, invites) identified by `%@example.com` email addresses.

## Adding a New Resource

Follow the existing pattern:

1. **Add the struct** to `src/types.go` with GORM tags and an `active bool` soft-delete column.
2. **Implement the `db` interface** in `src/db.go` (`Add`, `Update`, `Remove`, `Get`).
3. **Add `AutoMigrate` and FK** entries in `src/main.go`.
4. **Add `AutoMigrate`** in `TestMain` in `src/db_test.go`.
5. **Write handlers** in a new `src/api_{resource}.go` file following the `handleCreate{X}` / `handleGet{X}` / `handleUpdate{X}` / `handleDelete{X}` pattern.
6. **Register routes** in `src/main.go`.
7. **Write unit tests** in `src/api_{resource}_test.go`.
8. **Add to `openapi.yaml`**.

Each handler should:
- Start an OTel span.
- Call `requirePermission` with the appropriate action and resource path.
- Use `dbLog` (not `slog.Error` directly) when handling database errors so that not-found results are logged at Warn rather than Error.
