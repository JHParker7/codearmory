# Workflows

CI/CD pipeline orchestrator. Accepts workflow definitions composed of ordered HTTP steps, executes them in sequence against registered backend services, and records per-step results.

## How it works

```
Caller → POST /workflows/{id}/runs
         │
         ▼
   workflow_runs (status=pending) in PostgreSQL
         │
         ▼
   Worker pool (5 goroutines, FOR UPDATE SKIP LOCKED)
         │  1. Pick pending run
         │  2. Fetch workflow definition
         │  3. Execute each step:
         │     - Resolve service URL from SERVICES map
         │     - Substitute ${KEY} from run inputs into path/body/headers
         │     - POST to service with caller's Authorization: Bearer token
         │     - Evaluate response against expected_status or any 2xx
         │  4. Write step results; clear stored token on terminal state
```

Access is org-scoped: users can access workflows and runs they created, or those owned by members of their org.

All auth is delegated to Gatekeeper via `POST {GATEKEEPER_URL}/check_permissions`. Responses include `user_id` and `org_id`.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/workflows` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Gatekeeper base URL |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for key rotation with Gatekeeper |
| `HOOKS_TRIGGER_KEY` | — | Shared HMAC secret with the hooks service. Required to accept internal trigger requests from hooks. |
| `SERVICES` | — | Comma-separated `name=url` pairs of services steps can call (e.g. `forge=http://forge:8083,blueprints=http://blueprints:8081`). `gatekeeper` is always available. |
| `PORT` | `8085` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `workflows` | OTel service name |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `DATABASE_URL_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

## Running locally

```bash
cd src/systems/workflows
DATABASE_URL=postgresql://postgres:pass@localhost:5432/workflows \
  GATEKEEPER_URL=http://localhost:8080 \
  SERVICES=forge=http://localhost:8083,blueprints=http://localhost:8081 \
  go run .
```

## Docker

```bash
cd src/systems/workflows
docker build -t workflows:latest .

docker run -p 8085:8085 \
  -e DATABASE_URL=postgresql://postgres:pass@db:5432/workflows \
  -e GATEKEEPER_URL=http://gatekeeper:8080 \
  -e GATEKEEPER_SERVICE_KEY=your-service-key \
  -e HOOKS_TRIGGER_KEY=your-hmac-secret \
  -e SERVICES=forge=http://forge:8083,blueprints=http://blueprints:8081 \
  workflows:latest
```

## API

All endpoints require `Authorization: Bearer <token>` verified by Gatekeeper.

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/workflows` | `createWorkflow` on `workflows/workflows` | Create a workflow |
| `GET` | `/workflows` | `listWorkflow` on `workflows/workflows` | List accessible workflows |
| `GET` | `/workflows/{id}` | `getWorkflow` on `workflows/workflows/{id}` | Get a workflow |
| `PUT` | `/workflows/{id}` | `updateWorkflow` on `workflows/workflows/{id}` | Update a workflow |
| `DELETE` | `/workflows/{id}` | `deleteWorkflow` on `workflows/workflows/{id}` | Soft-delete a workflow |
| `POST` | `/workflows/{id}/runs` | `triggerRun` on `workflows/runs` | Trigger a run |
| `GET` | `/runs` | `listRun` on `workflows/runs` | List accessible runs |
| `GET` | `/runs/{id}` | `getRun` on `workflows/runs/{id}` | Get a run with step results |
| `DELETE` | `/runs/{id}` | `cancelRun` on `workflows/runs/{id}` | Cancel a pending or running run |

### Internal endpoint (service-to-service)

`POST /internal/workflows/{id}/runs` is used exclusively by the hooks service. Requires `X-Hooks-Token` (HMAC-SHA256 signed with `HOOKS_TRIGGER_KEY`) and `X-Hooks-Timestamp` headers.

```json
{"triggered_by": "user-id", "org_id": "...", "inputs": {...}}
```

Returns `202 Accepted` with the created run.

### Create a workflow

```bash
curl -X POST http://localhost:8085/workflows \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deploy-staging",
    "description": "Build and deploy to staging",
    "steps": [
      {
        "name": "build",
        "service": "forge",
        "method": "POST",
        "path": "/executions",
        "body": {"image": "alpine:3.19", "command": ["sh", "-c", "echo building ${VERSION}"]},
        "expected_status": 201,
        "timeout_secs": 300
      },
      {
        "name": "notify",
        "service": "gatekeeper",
        "method": "GET",
        "path": "/healthz",
        "timeout_secs": 5
      }
    ]
  }'
```

A maximum of 50 steps per workflow is enforced.

### Step fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | No | Human label. Defaults to `step-N`. |
| `service` | string | Yes | Registered service name from `SERVICES` env var. |
| `method` | string | No | HTTP method. Defaults to `POST`. |
| `path` | string | Yes | Path on the target service. Must start with `/`. Supports `${KEY}` substitution from run inputs. |
| `body` | JSON | No | Request body. Supports `${KEY}` substitution. |
| `headers` | object | No | Extra headers to include. Values support `${KEY}` substitution. |
| `expected_status` | int | No | If set, only this exact status is treated as success. Otherwise any 2xx succeeds. |
| `timeout_secs` | int | No | Per-step timeout in seconds. Defaults to 30, capped at 3600. |

### Trigger a run

```bash
curl -X POST http://localhost:8085/workflows/{id}/runs \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"inputs": {"VERSION": "v1.2.3", "ENV": "staging"}}'
```

Returns `202 Accepted` with the created run (status `pending`). Poll `GET /runs/{id}` for completion.

### Run object

```json
{
  "run_id": "uuid",
  "workflow_id": "uuid",
  "triggered_by": "user-id",
  "org_id": "org-id",
  "status": "completed",
  "current_step": 1,
  "inputs": {"VERSION": "v1.2.3"},
  "step_runs": [
    {
      "step_run_id": "uuid",
      "step_index": 0,
      "step_name": "build",
      "status": "completed",
      "response_status": 201,
      "response_body": "...",
      "started_at": "...",
      "ended_at": "..."
    }
  ],
  "created_at": "...",
  "started_at": "...",
  "ended_at": "..."
}
```

**Run status values:** `pending` → `running` → `completed` | `failed` | `cancelled`

**Step status values:** `running` → `completed` | `failed` | `cancelled`

### Input substitution

`${KEY}` placeholders in step `path`, `body`, and `headers` are replaced with values from the run's `inputs` map at execution time. Unrecognised keys are left as-is.

## Metrics

| Metric | Description |
|--------|-------------|
| `workflows.runs.triggered.total` | Total runs triggered, labelled by `workflow.id` |
| `workflows.runs.completed.total` | Runs reaching a terminal state, labelled by `workflow.id` and `status` |
| `workflows.steps.completed.total` | Steps completing, labelled by `workflow.id` and `status` |

## Testing

```bash
# Unit tests
cd src/systems/workflows
go test ./...

# Integration tests (requires running Workflows and Gatekeeper)
pip install -r tests/workflows/requirements.txt
WORKFLOWS_URL=http://localhost:8085 GATEKEEPER_URL=http://localhost:8080 \
  pytest tests/workflows/ -v
```
