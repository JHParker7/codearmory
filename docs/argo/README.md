# Argo

Argo CD sync control plane — the **second integration** on the [outpost framework](../outpost/README.md), built to prove the framework generalises. Like [chaos](../chaos/README.md), it is a thin control plane with **no Argo CD credentials**: it drives an outpost via the [outpost-gateway](../outpost-gateway/README.md) and records the application state the outpost reports back. The actual Argo CD Application handling lives in the outpost's argo module.

Adding argo touched neither the outpost core nor the gateway nor the event backbone — only a new outpost module, this consumer service, and manifest entries.

## How it works

1. The outpost's argo module watches Argo CD **Application** resources and emits `app-state` events. Argo upserts an `App` record from each, so applications become visible in the platform.
2. A user triggers a sync (`POST /apps/{name}/sync`). Argo stores a `pending` **Sync**, enqueues a `sync` command to the app's outpost, and returns `202`.
3. The outpost's argo module patches the Application's `operation` field, which Argo CD reconciles, then reports `sync-started` and subsequent `app-state` events.
4. The gateway dispatcher delivers those events to `POST /internal/events`. Argo correlates them to the in-flight sync **by `outpost_id` + `app_name`** and advances its status.

## Status model

Sync status strings must match the success/failure states declared for the `argo/sync` workflow action in the registry manifest (a unit test asserts this).

| Status | Meaning |
|--------|---------|
| `pending` | Command enqueued, awaiting the outpost |
| `running` | Sync operation in progress |
| `Synced` | App is `Synced` + `Healthy` (terminal, success) |
| `Failed` | Sync operation failed (terminal, failure) |

## Tenant scoping

Apps and syncs are scoped by `org_id` when present, else by the owning user — so a self-hosted single-user deployment sees its own outpost's apps, and every member of an org sees the org's apps. The gateway stamps each event with the outpost's org and user so the consumer scopes identically to user requests.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/argo` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Gatekeeper base URL |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for key rotation with Gatekeeper |
| `OUTPOST_GATEWAY_URL` | — | outpost-gateway base URL (where commands are enqueued) |
| `OUTPOST_INTERNAL_KEY` | — | Shared HMAC secret for the internal command/event plane. Must match the gateway's value. |
| `PORT` | `8091` | Port the server listens on |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant.

## API

All user endpoints require `Authorization: Bearer <token>`, verified by Gatekeeper.

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/apps` | `listApp` on `argo/apps` | List discovered applications |
| `GET` | `/apps/{name}` | `getApp` on `argo/apps/{name}` | Get an application's latest state |
| `POST` | `/apps/{name}/sync` | `syncApp` on `argo/apps/{name}` | Trigger a sync (enqueues a command); returns `202` |
| `GET` | `/syncs/{id}` | `getSync` on `argo/syncs/{id}` | Get a sync operation's status |

`POST /internal/events` consumes outpost events from the dispatcher (shared-key HMAC). Full schema: [openapi.yaml](openapi.yaml).

### Trigger a sync

```bash
curl -X POST http://localhost:8091/apps/guestbook/sync \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"revision": "HEAD"}'
# → 202 {"sync_id":"uuid","status":"pending", ...}
```

If the application has not reported state yet, pass an explicit `outpost_id` in the body.

## Workflows integration

Argo publishes an `argo/sync` async action, so a pipeline can **gate on a healthy sync**: the step submits a sync and polls until it reaches a terminal state, succeeding on `Synced` and failing on `Failed`.

```yaml
steps:
  - name: deploy-and-verify
    action: argo/sync
    with:
      name: guestbook
      outpost_id: <outpost-id>
```

## Metrics

| Metric | Description |
|--------|-------------|
| `argo.syncs.triggered.total` | Syncs triggered |
| `argo.syncs.resolved.total` | Syncs that reached a terminal state, labelled by `status` |

## Testing

```bash
cd src/systems/argo && go test ./...
docker compose --profile test run --rm argo-integration-tests
```
