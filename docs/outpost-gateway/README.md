# Outpost Gateway

The outpost-facing connection point for cluster integrations, and the Postgres **event backbone** behind them. It is distinct from [Conductor](../conductor/README.md): outposts talk *directly* to the gateway, not through the user API gateway. See the [framework overview](../outpost/README.md) for the big picture.

## How it works

The gateway has three surfaces:

1. **User-facing** (`/outposts*`, reached through Conductor with a Gatekeeper bearer token) — register an outpost, list/get/delete it, and receive its single-use enrollment token.
2. **Outpost-facing** (`/outpost/*`, hit directly by an outpost) — enroll, long-poll for commands, acknowledge them, ingest events, and heartbeat. Authenticated by a per-outpost key (bcrypt-verified), not Gatekeeper JWTs.
3. **Internal** (`/internal/commands`, called by control-plane services) — enqueue a command for an outpost, authenticated by a shared-key HMAC.

Two Postgres tables form the backbone:

- **`outpost_commands`** — a queue. Control services enqueue rows; the command long-poll claims them with `FOR UPDATE SKIP LOCKED`, so any gateway replica can serve any outpost.
- **`outpost_events`** — a transactional outbox. The outpost POSTs events into it; a background **dispatcher** delivers each to the integration's consumer service (`/internal/events`, HMAC-signed), with dead-letter retry and exponential backoff. Consumers dedupe by event ID (at-least-once).

The gateway holds **no cluster credentials**. It is pure HTTP + Postgres.

## Enrollment

When a user adds an outpost in the portal, the gateway creates an `outposts` row (status `pending`) and returns a **single-use enrollment token** of the form `<outpost_id>.<secret>` (the secret is bcrypt-hashed at rest). The outpost calls `POST /outpost/register` with the token; the gateway verifies it, mints a long-lived **outpost key**, clears the enrollment token (single-use), flips the outpost to `connected`, and returns the key. All subsequent outpost requests carry `X-Outpost-ID` + `Authorization: Bearer <outpost-key>`.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/outpost_gateway` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Gatekeeper base URL (for the user-facing `/outposts*` routes) |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for key rotation with Gatekeeper |
| `OUTPOST_INTERNAL_KEY` | — | Shared HMAC secret for the internal command/event plane. Must match the chaos/argo consumers' value. |
| `EVENT_CONSUMERS` | — | Map of integration → consumer base URL, e.g. `chaos=http://chaos:8090,argo=http://argo:8091`. The dispatcher appends `/internal/events`. |
| `PORT` | `8092` | Port the server listens on |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant that reads the value from a file path (Docker/Kubernetes secret mounts).

## API

### User-facing (via Conductor, Gatekeeper bearer)

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/outposts` | `createOutpost` on `outpost-gateway/outposts` | Register an outpost; returns its enrollment token (once) |
| `GET` | `/outposts` | `listOutpost` on `outpost-gateway/outposts` | List the caller's outposts |
| `GET` | `/outposts/{id}` | `getOutpost` on `outpost-gateway/outposts/{id}` | Get an outpost |
| `DELETE` | `/outposts/{id}` | `deleteOutpost` on `outpost-gateway/outposts/{id}` | Delete an outpost |

### Outpost-facing (direct, outpost-key auth)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/outpost/register` | Exchange an enrollment token for an outpost key |
| `GET` | `/outpost/commands` | Long-poll (≤30 s); claims and returns pending commands |
| `POST` | `/outpost/commands/{id}/ack` | Acknowledge a delivered command |
| `POST` | `/outpost/events` | Ingest an event into the outbox |
| `POST` | `/outpost/heartbeat` | Record liveness |

### Internal (shared-key HMAC)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/internal/commands` | Enqueue a command `{outpost_id, integration, type, payload}` for an outpost |

The full schema is in [openapi.yaml](openapi.yaml).

## Outpost lifecycle states

| Status | Meaning |
|--------|---------|
| `pending` | Registered in the portal, not yet enrolled |
| `connected` | Enrolled and seen recently via heartbeat/long-poll |
| `stale` | Enrolled but not seen within the liveness window (display-only) |

## Delivery semantics

- **Commands**: claimed via `SKIP LOCKED`, marked `claimed`, then `done` on ack. The internal enqueue validates that the target outpost has the requested module enabled (`409` otherwise).
- **Events**: written to the outbox, then dispatched. Transient failures retry with backoff (30 s → 900 s cap, up to 8 attempts) before dead-lettering. A re-posted event ID (outpost retry) is treated as success.

## Metrics

| Metric | Description |
|--------|-------------|
| `outpost.enrolled.total` | Outposts enrolled |
| `outpost.commands.enqueued.total` | Commands enqueued for outposts |
| `outpost.events.ingested.total` | Events ingested from outposts |
| `outpost.events.delivered.total` | Events delivered to consumer services |

## Testing

```bash
# Unit tests
cd src/systems/outpost-gateway
go test ./...

# Integration tests (the test plays the outpost role end-to-end)
docker compose --profile test run --rm outpost-gateway-integration-tests
```
