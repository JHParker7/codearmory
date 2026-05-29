# Hooks

Webhook receiver and pipeline rule engine. Accepts generic webhook events from external systems, matches them against configurable pipeline rules, and triggers workflow runs on the Workflows service.

## How it works

```
External system (git host, CI, etc.)
  │
  └── POST /hooks ──────────────────────────► Hooks :8087
        │  1. Parse payload (repo, event, ref, commit, pusher, message)
        │  2. X-Hook-Event header overrides body.event if present
        │  3. Query active pipeline rules matching repo + event
        │  4. For each rule:
        │     a. Apply ref_filter (empty=any, exact, or prefix glob)
        │     b. Verify X-Hub-Signature-256 HMAC if rule has a secret
        │     c. Build workflow inputs from input_mapping + system defaults
        │     d. POST /internal/workflows/{id}/runs on Workflows service
        │        (signed with HMAC-SHA256 using HOOKS_TRIGGER_KEY)
        │  5. Record event + trigger outcomes in PostgreSQL
        └── Return 200 with HookEvent (always, even on partial failure)
```

Rules and event history are accessible via authenticated REST API.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/hooks` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Gatekeeper base URL for rule management auth |
| `WORKFLOWS_URL` | `http://localhost:8085` | Workflows service base URL for triggering runs |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for gatekeeper key rotation |
| `HOOKS_TRIGGER_KEY` | — | **Required in production.** Shared HMAC secret with the workflows service. Used to sign internal trigger requests. Must match `HOOKS_TRIGGER_KEY` on the workflows service. |
| `PORT` | `8087` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `hooks` | OTel service name |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `DATABASE_URL_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

## API

### Webhook receiver (unauthenticated)

`POST /hooks` — receives generic webhook events. No bearer token required; per-rule HMAC verification is used instead.

**Payload:**

```json
{
  "repo": "myorg/myrepo",
  "event": "push",
  "ref": "refs/heads/main",
  "commit": "abc123def456",
  "pusher": "alice",
  "message": "fix: update config"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `repo` | Yes | Repository identifier used to match pipeline rules |
| `event` | Yes | Event type (e.g. `push`, `merge`, `tag`). Can also be set via `X-Hook-Event` header, which takes precedence. |
| `ref` | No | Git ref (e.g. `refs/heads/main`) |
| `commit` | No | Commit SHA |
| `pusher` | No | Username who triggered the event |
| `message` | No | Commit or event message |

**Response (200):**

```json
{
  "event_id": "uuid",
  "repo": "myorg/myrepo",
  "event_type": "push",
  "ref": "refs/heads/main",
  "payload": {"repo": "...", "event": "push", ...},
  "rules_matched": 1,
  "status": "triggered",
  "triggers": [
    {
      "trigger_id": "uuid",
      "rule_id": "uuid",
      "workflow_id": "uuid",
      "run_id": "uuid",
      "status": "triggered"
    }
  ],
  "created_at": "..."
}
```

**Event status values:** `received` (no rules matched), `triggered` (all succeeded), `failed` (all failed), `partial` (mixed).

### Pipeline rules (authenticated)

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/rules` | `createRule` on `hooks/rules` | Create a pipeline rule |
| `GET` | `/rules` | `listRule` on `hooks/rules` | List accessible rules |
| `GET` | `/rules/{id}` | `getRule` on `hooks/rules/{id}` | Get a rule |
| `PUT` | `/rules/{id}` | `updateRule` on `hooks/rules/{id}` | Update a rule |
| `DELETE` | `/rules/{id}` | `deleteRule` on `hooks/rules/{id}` | Soft-delete a rule |

### Event log (authenticated)

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/events` | `listEvent` on `hooks/events` | List events visible via accessible rules. Supports `?repo=` filter. |
| `GET` | `/events/{id}` | `getEvent` on `hooks/events/{id}` | Get an event with trigger details |

### Create a pipeline rule

```bash
curl -X POST http://localhost:8087/rules \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deploy-on-main-push",
    "repo": "myorg/myrepo",
    "events": ["push"],
    "ref_filter": "refs/heads/main",
    "workflow_id": "wf-uuid",
    "secret": "my-webhook-secret",
    "input_mapping": {
      "COMMIT_SHA": "commit",
      "BRANCH":     "ref",
      "REPO":       "repo",
      "PUSHER":     "pusher"
    }
  }'
```

### Rule fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Human-readable label |
| `repo` | string | Yes | Repo identifier to match (exact string match) |
| `events` | []string | Yes | Event types that trigger this rule (e.g. `["push", "merge"]`) |
| `ref_filter` | string | No | Ref to match. Empty = any ref. Exact string or prefix glob ending in `/*` (e.g. `refs/heads/*`). |
| `workflow_id` | string | Yes | Workflow to trigger on match |
| `secret` | string | No | HMAC-SHA256 secret for verifying `X-Hub-Signature-256`. Never returned in API responses. |
| `input_mapping` | object | No | Maps workflow input keys to webhook payload fields. See below. |

The `secret` field is write-only — it is stored but never returned in API responses. On update: omit the field (or send JSON `null`) to leave the existing secret unchanged; send `""` to clear it; send a non-empty string to replace it.

### Input mapping

When a rule fires, the workflow run receives the following inputs by default:

| Key | Value |
|-----|-------|
| `HOOK_REPO` | `payload.repo` |
| `HOOK_EVENT` | `payload.event` |
| `HOOK_REF` | `payload.ref` |
| `HOOK_COMMIT` | `payload.commit` |

Additional inputs are added from `input_mapping`, where the key is the workflow input name and the value is the payload field name (`repo`, `event`, `ref`, `commit`, `pusher`, `message`). Input mapping values override the defaults if the keys collide.

### Ref filter

| Filter value | Behaviour |
|---|---|
| `""` (empty) | Matches any ref |
| `refs/heads/main` | Exact match only |
| `refs/heads/*` | Matches any ref starting with `refs/heads/` |
| `refs/tags/*` | Matches any ref starting with `refs/tags/` |

### HMAC verification

If a rule has a `secret`, incoming requests must include:

```
X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, raw_body))>
```

This is the same format used by GitHub webhooks. Rules without a secret accept any request that matches repo, event, and ref_filter.

## Access model

Pipeline rules are org-scoped: a user can see and manage rules they created, or rules owned by members of their org. Event history is visible via the same scoping — an event is accessible if any of its triggered rules are accessible to the caller.

## Metrics

| Metric | Description |
|--------|-------------|
| `hooks.received.total` | Webhook events received, labelled by `repo` |
| `hooks.rules_matched.total` | Rules matched per event, labelled by `repo` and `workflow.id` |
| `hooks.runs_triggered.total` | Successful workflow trigger calls, labelled by `workflow.id` |

## Testing

```bash
# Unit tests
cd src/systems/hooks
go test ./...

# Integration tests (requires running Hooks, Workflows, and Gatekeeper)
pip install -r tests/hooks/requirements.txt
HOOKS_URL=http://localhost:8087 WORKFLOWS_URL=http://localhost:8085 \
  GATEKEEPER_URL=http://localhost:8080 pytest tests/hooks/ -v
```
