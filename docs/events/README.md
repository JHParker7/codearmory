# Events

The platform's event plane: every service emits JSON **events** here, and **triggers** whose
field-based filters match dispatch **actions** (run a pipeline, open a ticket, notify, call a
webhook, enqueue an outpost command). It also hosts the inbound webhook adapters, so a git
push from GitHub or Forgejo becomes an event like any other.

**Port:** 8093 · **Registered as:** `events`

This service supersedes the former `hooks` service, which it absorbed. The difference is the
data model: a hooks *rule* was a git-shaped tuple (`source` + `events` + `ref_filter`) that
could only ever trigger a workflow. An events *trigger* is a filter over any field of any
event, dispatching any action. See [design.md](design.md) for the reasoning and
[openapi.yaml](openapi.yaml) for the full API.

## How it works

```
   emitters                          adapters (public, HMAC-signed)
   git_factory ─┐              ┌── POST /hooks         (generic)
   tickets ─────┼─ POST ───┐   ├── POST /hooks/git     (normalized)
   workflows ───┘  /internal│   ├── POST /hooks/gitea   (Forgejo/Gitea)
                   /events  │   └── POST /hooks/github  (GitHub App)
                            ▼   ▼
                      ┌──────────────┐
                      │    events    │  1. validate + store (idempotent on event id)
                      │    :8093     │  2. load the tenant's enabled triggers
                      └──────┬───────┘  3. evaluate each filter
                             │          4. claim (trigger, event) — exactly once
                             ▼          5. run the actions, retry with backoff
              run_pipeline · create_ticket · notify · webhook_out
                       · enqueue_outpost_command
```

## The envelope

One shape for every event. `subject` is what makes triggers addressable per-resource — the
thing the old shared `source` constant could not express.

```json
{
  "id": "01J...",
  "spec_version": "1",
  "type": "repo.push",
  "source": "codearmory_git_factory",
  "subject": "acme/myapp",
  "actor": { "org_id": "org-1", "user_id": "u-42" },
  "occurred_at": "2026-08-02T10:04:11Z",
  "data": { "ref": "main", "commit": "abc123", "pusher": "alice" }
}
```

`type` is dotted, namespaced and stable. `data` is free-form — filters reach into it by dotted
path (`data.ref`).

## Triggers

A trigger is a tenant-scoped subscription. Its `match` is either a **group** (`all` / `any` /
`not`) or a **leaf** (`field` / `op` / `value`); the recursion lets it express arbitrary
boolean logic while the common case stays a flat `all`.

```json
{
  "name": "ci-main",
  "match": { "all": [
    { "field": "type",     "op": "eq", "value": "repo.push" },
    { "field": "subject",  "op": "eq", "value": "acme/myapp" },
    { "field": "data.ref", "op": "eq", "value": "main" }
  ]},
  "actions": [
    { "kind": "run_pipeline", "config": {
        "pipeline_id": "<uuid>",
        "inputs": { "IMAGE_TAG": "{{ data.commit }}" }
    }}
  ]
}
```

String values in an action's config are templated: `{{ data.commit }}` interpolates from the
event, and an unknown path renders empty rather than failing the action.

`POST /triggers/test-match` dry-runs a filter against an event you supply — "would this fire?"
— without persisting anything.

## Authentication

Three schemes, by endpoint class. Getting these confused is the usual cause of a silent 401.

| Endpoints | Authenticated by |
|---|---|
| `POST /internal/events` | Shared-key HMAC over the event's identity, tenant, subject and payload digest, keyed by `EVENTS_TRIGGER_KEY`. Sent as `X-Events-Token` + `X-Events-Timestamp`; the timestamp must be within 30s. Emitters use the SDK's `events.Emit`, which signs identically. |
| `POST /hooks`, `/hooks/git`, `/hooks/gitea` | Provider HMAC over the raw body, keyed by `EVENTS_WEBHOOK_SECRET`. Accepted as `X-Hub-Signature-256: sha256=<hex>` or `X-Gitea-Signature: <hex>`. **Unset secret closes these endpoints** — an unsigned webhook is never trusted. |
| `POST /hooks/github` | The App's own `GITHUB_APP_WEBHOOK_SECRET`. Registered only when `GITHUB_APP_ID` is set. |
| Everything else | Gatekeeper bearer token via conductor. |

Records belonging to another tenant are reported as `404 Not Found`, never `403`, so the API
never confirms that an id exists for someone else.

### Tenancy on public webhooks

A third-party provider's payload carries no codearmory identity, so the tenant comes from the
endpoint URL: `?org_id=<id>` or `?user_id=<id>`. The webhook secret attests that whoever
configured the webhook holds it — **not** that the repo belongs to that tenant. The two are
bound only by being configured together in the provider's webhook settings. Per-repo secrets
(or, for the App, an installation→tenant mapping) are what would make this attestation
tenant-specific.

`/hooks/git` is the exception: it is used by `git_factory`, our own service, so the tenant
comes from the signed body instead.

## GitHub App

Set `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY` and `GITHUB_APP_WEBHOOK_SECRET` to register
`POST /hooks/github`. Beyond normalising deliveries, the App reports **back**: for each
pipeline run a matched trigger starts, it opens a check run on the pushed commit and watches
it to completion (success / failure / cancelled, or `timed_out` after two hours), so the
result appears on the PR in GitHub's UI.

All three values are required together — the service refuses to start with an app id but no
webhook secret rather than accepting unverified deliveries. Set `GITHUB_API_URL` to
`https://<host>/api/v3` for GitHub Enterprise Server.

## Delivery guarantees

At-least-once, with idempotency rather than exactly-once:

- **Ingestion** is idempotent on the event id, so a redelivery is a no-op.
- **Dispatch** claims each `(trigger, event)` pair through a unique index, so a redelivered
  event never double-fires a trigger.
- **Failures** retry with exponential backoff (15s base, 6 attempts) and then dead-letter.
  Actions run in order and stop at the first failure, so the dispatch retries as a unit.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8093` | Listen port |
| `DATABASE_URL` | — | PostgreSQL DSN |
| `DATABASE_READ_URL` | `DATABASE_URL` | Optional read-replica DSN |
| `GATEKEEPER_URL` | `http://localhost:8081` | Permission checks + key rotation |
| `GATEKEEPER_SERVICE_KEY` | — | Must match the `events` entry in gatekeeper's `GATEKEEPER_SERVICES` |
| `WORKFLOWS_URL` | `http://localhost:8085` | Target of the `run_pipeline` action |
| `TICKETS_URL` | `http://localhost:8086` | Target of the `create_ticket` action |
| `NOTIFICATIONS_URL` | `http://localhost:8088` | Target of the `notify` action |
| `OUTPOST_GATEWAY_URL` | `http://localhost:8092` | Target of `enqueue_outpost_command` |
| `EVENTS_TRIGGER_KEY` | — | Shared HMAC: emitters sign with it, and workflows verifies pipeline dispatch with it. **Every holder must carry the identical value** or dispatch 401s. |
| `EVENTS_WEBHOOK_SECRET` | — | Keys the provider HMAC on the public webhook endpoints. Unset leaves them closed. |
| `GITHUB_APP_ID` / `GITHUB_APP_PRIVATE_KEY` / `GITHUB_APP_WEBHOOK_SECRET` | — | Optional GitHub App (all three, or none) |
| `GITHUB_API_URL` | `https://api.github.com` | GitHub Enterprise Server API root |

Every secret also supports the `_FILE` suffix for volume-mounted Kubernetes secrets.

> **On the wire scheme.** The `run_pipeline` action signs its dispatch to workflows with the
> `hooks:` MAC prefix and `X-Hooks-Token`/`X-Hooks-Timestamp` headers. That naming is
> historical — it is the protocol workflows has always spoken, and renaming it would break
> every deployment mid-upgrade for no functional gain. Only the service holding the key
> changed. The same applies to the chart's `bootstrapServiceKey "hooks-trigger"` seed: it must
> **not** be "renamed to match", since a different seed mints a different key on one side.

## Emitting events

Services use the shared SDK rather than reimplementing the envelope and its HMAC:

```go
import sdkevents "github.com/code-armory-app/codearmory_sdk/events"

emitter := sdkevents.New(eventsURL, eventsKey, "my-service", httpClient)

_ = emitter.Emit(ctx, sdkevents.Event{
    Type:    "thing.happened",
    Subject: thingID,
    Actor:   sdkevents.Actor{OrgID: orgID, UserID: userID},
    Data:    map[string]any{"detail": "..."},
})
```

Emission is best-effort by design: the thing that produced the event has already succeeded, so
failing it afterwards because a listener was unreachable would be a lie to the caller. `Emit`
returns an error for logging, and callers drop it. An emitter built with an empty URL or key is
disabled and `Emit` is a no-op — the correct behaviour for a deployment not running events.

## API

Full spec in [openapi.yaml](openapi.yaml), also served at `GET /openapi.yaml`.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/triggers` | Create a trigger |
| `GET` | `/triggers` | List the caller's triggers |
| `GET` | `/triggers/{id}` | Get a trigger |
| `PUT` | `/triggers/{id}` | Replace name / match / actions / enabled |
| `DELETE` | `/triggers/{id}` | Delete a trigger |
| `POST` | `/triggers/test-match` | Dry-run a filter against an event |
| `GET` | `/events` | List the tenant's recent events (`?type=` filter) |
| `GET` | `/events/{id}` | Get one event |
| `POST` | `/internal/events` | Ingest from a trusted emitter (HMAC) |
| `POST` | `/hooks` | Generic webhook |
| `POST` | `/hooks/git` | Normalized git webhook |
| `POST` | `/hooks/gitea` | Forgejo / Gitea webhook |
| `POST` | `/hooks/github` | GitHub App webhook (when configured) |

## Metrics

| Metric | Attributes |
|---|---|
| `events.received.total` | `source` |
| `events.triggers.matched.total` | `event.type` |
| `events.actions.run.total` | `kind`, `ok` |
