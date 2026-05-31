# Workflows

CI/CD pipeline orchestrator. Composes reusable **Steps** into **Workflows**, executes them against registered backend services, and records per-step output.

## Two-layer model

The service has two distinct concepts:

**Step** — a reusable, named action definition stored in the `steps` table. A step declares *what to do* (`action` + `with` config) and can be referenced by many workflows. Changing a step's definition affects every pipeline that references it.

**Workflow** — an ordered list of step references (`step_id` + optional `parallel_group`). The workflow declares *when and in what order* to run steps; it stores no action logic itself.

```
Steps table          Workflows table
──────────           ───────────────
step A  ──────────►  [ref A, ref B, ref C]  ◄── workflow "deploy-staging"
step B  ──┐
step C  ──┘──────►   [ref B, ref C]         ◄── workflow "smoke-test"
```

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
         │  2. Fetch workflow definition; enrich step refs from steps table
         │  3. Group steps by parallel_group; execute each group:
         │     - Steps with same non-nil parallel_group run concurrently
         │     - Steps with nil parallel_group run sequentially
         │     - For each step:
         │       a. Resolve action: "http" → raw HTTP; other → action catalog
         │       b. Substitute ${KEY} from run inputs into all With string values
         │       c. Execute action; store output in WorkflowStepRun.output
         │  4. Write step results; clear stored token on terminal state
```

Access is org-scoped: users can access steps, workflows, and runs they created, or those owned by members of their org.

All auth is delegated to Gatekeeper via `POST {GATEKEEPER_URL}/check_permissions`. Responses include `user_id` and `org_id`.

## Action types

Every step has an `action` field that determines how it runs.

### `http` — raw HTTP escape hatch

Calls any registered service directly. Use this when no catalog action exists for what you need.

```json
{
  "action": "http",
  "with": {
    "service": "forge",
    "path": "/executions",
    "method": "POST",
    "body": {"image": "alpine:3.19", "command": ["sh", "-c", "echo ${VERSION}"]},
    "headers": {"X-Custom": "value"}
  }
}
```

`with` fields for `http`:

| Field | Required | Description |
|-------|----------|-------------|
| `service` | Yes | Registered service name (from `SERVICES` env var or registry) |
| `path` | Yes | Path on the target service. Must start with `/`. |
| `method` | No | HTTP method. Defaults to `POST`. |
| `body` | No | Request body (any JSON value). |
| `headers` | No | Extra request headers (`map[string]string`). |

### Catalog actions (e.g. `forge/run`, `tickets/create`)

Named actions loaded from the registry every 5 minutes. The registry publishes each service's supported actions with their path, method, body transforms, and optional async polling config. The caller supplies only the action-specific `with` fields; the service URL and HTTP details are resolved from the catalog.

```json
{
  "action": "forge/run",
  "with": {
    "image": "alpine:3.19",
    "command": ["sh", "-c", "echo ${VERSION}"]
  }
}
```

Use `GET /actions` to list all currently available catalog actions and their definitions.

## Input substitution

`${KEY}` placeholders in any string value inside a step's `with` map are replaced with values from the run's `inputs` map at execution time. Substitution applies recursively to nested maps and arrays. Unrecognised keys are left as-is.

Example: a step with `"path": "/deploys/${ENV}"` and a run triggered with `{"inputs": {"ENV": "staging"}}` executes against `/deploys/staging`.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/workflows` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Gatekeeper base URL |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for key rotation with Gatekeeper |
| `HOOKS_TRIGGER_KEY` | — | Shared HMAC secret with the hooks service. Required to accept internal trigger requests from hooks. |
| `REGISTRY_URL` | — | Registry base URL. Used to poll the action catalog every 5 minutes. |
| `REGISTRY_SERVICE_KEY` | — | Service key for authenticating with the registry to fetch the action catalog. |
| `SERVICES` | — | Comma-separated `name=url` pairs seeded at startup (e.g. `forge=http://forge:8083`). The registry catalog poller adds/updates entries automatically. `gatekeeper` is always available. |
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
  REGISTRY_URL=http://localhost:8086 \
  REGISTRY_SERVICE_KEY=your-registry-key \
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
  -e REGISTRY_URL=http://registry:8086 \
  -e REGISTRY_SERVICE_KEY=your-registry-key \
  -e SERVICES=forge=http://forge:8083,blueprints=http://blueprints:8081 \
  workflows:latest
```

## API

All endpoints require `Authorization: Bearer <token>` verified by Gatekeeper, except `/healthz` and the internal endpoints.

### Steps

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/steps` | `createStep` on `workflows/steps` | Create a step |
| `GET` | `/steps` | `listStep` on `workflows/steps` | List accessible steps |
| `GET` | `/steps/{id}` | `getStep` on `workflows/steps/{id}` | Get a step |
| `PUT` | `/steps/{id}` | `updateStep` on `workflows/steps/{id}` | Update a step |
| `DELETE` | `/steps/{id}` | `deleteStep` on `workflows/steps/{id}` | Soft-delete a step |

### Actions

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/actions` | `listAction` on `workflows/actions` | List the in-memory action catalog |

### Workflows

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/workflows` | `createWorkflow` on `workflows/workflows` | Create a workflow |
| `GET` | `/workflows` | `listWorkflow` on `workflows/workflows` | List accessible workflows |
| `GET` | `/workflows/{id}` | `getWorkflow` on `workflows/workflows/{id}` | Get a workflow with enriched step definitions |
| `PUT` | `/workflows/{id}` | `updateWorkflow` on `workflows/workflows/{id}` | Replace a workflow's name, description, and step list |
| `DELETE` | `/workflows/{id}` | `deleteWorkflow` on `workflows/workflows/{id}` | Soft-delete a workflow |

### Runs

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/workflows/{id}/runs` | `triggerRun` on `workflows/runs` | Trigger a run |
| `GET` | `/runs` | `listRun` on `workflows/runs` | List accessible runs |
| `GET` | `/runs/{id}` | `getRun` on `workflows/runs/{id}` | Get a run with step results |
| `DELETE` | `/runs/{id}` | `cancelRun` on `workflows/runs/{id}` | Cancel a pending or running run |

### Internal endpoints (service-to-service)

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/internal/workflows/{id}/runs` | HMAC | Trigger a run (hooks service only) |
| `GET` | `/internal/workflows/{id}` | HMAC | Get `org_id` for a workflow (hooks ownership check) |
| `GET` | `/internal/runs/{id}` | HMAC | Get current run status (GitHub App polling) |

Internal endpoints use `X-Hooks-Token` (HMAC-SHA256 signed with `HOOKS_TRIGGER_KEY`) and `X-Hooks-Timestamp` headers instead of Bearer auth.

---

### Create a step

```bash
# Catalog action step
curl -X POST http://localhost:8085/steps \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "run-forge",
    "description": "Execute a forge run",
    "action": "forge/run",
    "with": {
      "image": "alpine:3.19",
      "command": ["sh", "-c", "echo ${VERSION}"]
    },
    "timeout": 300
  }'

# HTTP escape-hatch step
curl -X POST http://localhost:8085/steps \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "notify-slack",
    "action": "http",
    "with": {
      "service": "gatekeeper",
      "path": "/healthz",
      "method": "GET"
    },
    "timeout": 5
  }'
```

Step name must be unique per user/org. `timeout` defaults to 30, maximum 3600.

### Step object

```json
{
  "step_id": "uuid",
  "name": "run-forge",
  "description": "Execute a forge run",
  "action": "forge/run",
  "with": {"image": "alpine:3.19", "command": ["sh", "-c", "echo ${VERSION}"]},
  "timeout": 300,
  "created_by": "user-id",
  "org_id": "org-id",
  "active": true,
  "created_at": "...",
  "updated_at": "..."
}
```

### List steps

```bash
# All accessible steps
curl http://localhost:8085/steps -H "Authorization: Bearer <token>"

# Filter by name (substring match)
curl "http://localhost:8085/steps?name=forge" -H "Authorization: Bearer <token>"
```

### Create a workflow

```bash
curl -X POST http://localhost:8085/workflows \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deploy-staging",
    "description": "Build and deploy to staging",
    "steps": [
      {"step_id": "uuid-of-build-step"},
      {"step_id": "uuid-of-test-a", "parallel_group": 1},
      {"step_id": "uuid-of-test-b", "parallel_group": 1},
      {"step_id": "uuid-of-deploy-step"}
    ]
  }'
```

A maximum of 50 step references per workflow is enforced. All referenced steps must exist and be accessible to the caller.

### Parallel execution

Steps sharing the same non-nil `parallel_group` integer execute concurrently. The run waits for every step in a group to reach a terminal state before advancing to the next sequential step or group.

Steps without a `parallel_group` (or with a `null` value) execute sequentially in the order they appear.

```
Step 0 (no group)          → runs first
Steps 1 and 2 (group=1)    → run concurrently after step 0 finishes
Step 3 (no group)          → runs after both group-1 steps finish
```

### Workflow object

When reading a single workflow (`GET /workflows/{id}`), the response enriches each step ref with the full step definition:

```json
{
  "workflow_id": "uuid",
  "name": "deploy-staging",
  "description": "Build and deploy to staging",
  "created_by": "user-id",
  "org_id": "org-id",
  "active": true,
  "steps": [
    {
      "step_id": "uuid",
      "name": "run-forge",
      "action": "forge/run",
      "with": {"image": "alpine:3.19"},
      "timeout": 300,
      "parallel_group": null
    }
  ],
  "created_at": "...",
  "updated_at": "..."
}
```

List responses (`GET /workflows`) return an empty `steps` array to keep payloads small.

### Action catalog

```bash
curl http://localhost:8085/actions -H "Authorization: Bearer <token>"
```

Returns an array of action definitions loaded from the registry:

```json
[
  {
    "name": "forge/run",
    "service_name": "forge",
    "service_url": "http://forge:8083",
    "method": "POST",
    "path": "/executions"
  }
]
```

The catalog refreshes every 5 minutes. Requires `REGISTRY_URL` and `REGISTRY_SERVICE_KEY` to be set.

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
      "run_id": "uuid",
      "step_index": 0,
      "step_name": "run-forge",
      "status": "completed",
      "output": "...",
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

The `output` field on each step run contains the action result: the response body for `http` steps, or the action output for catalog actions. It is `null` while the step is pending or running. Up to 1 MB is stored per step.

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
