# Notifications

The Notifications service (`:8094`) delivers messages to external destinations
through **pluggable provider integrations**. Slack, email (SMTP), and a generic
webhook ship in-tree; new integrations are added as a single self-contained file
that implements one interface — nothing else in the service changes.

## Concepts

- **Provider** — a delivery backend plugin (`slack`, `email`, `webhook`, …). Each
  provider declares a config schema (which fields it needs and which are secret).
- **Channel** — a configured instance of a provider, e.g. one Slack webhook or one
  SMTP mailbox. Channels are owned by a user and optionally scoped to an org.
- **Notification** — a durable delivery record. Sending enqueues one notification
  per target channel; a background worker delivers them and retries failures.

## Request flow

Like every backend, requests enter through **Conductor** under the
`/notifications` prefix and are authorised by **Gatekeeper** before reaching the
service. The service never holds long-lived provider credentials beyond what is
stored in a channel's config (secret fields are redacted in every API response).

```
Client → Conductor → [Gatekeeper: CheckPermissions] → Notifications → provider (Slack/SMTP/webhook)
```

## API

| Method & path | Action | Description |
| --- | --- | --- |
| `GET /providers` | `listProvider` | List providers and their config schema |
| `GET /channels` | `listChannel` | List channels (`?enabled=true` to filter) |
| `POST /channels` | `createChannel` | Create a channel |
| `GET /channels/{id}` | `getChannel` | Get a channel |
| `PUT /channels/{id}` | `updateChannel` | Update a channel (type is immutable) |
| `DELETE /channels/{id}` | `deleteChannel` | Delete a channel |
| `POST /channels/{id}/test` | `testChannel` | Send a synchronous test message |
| `POST /notify` | `createNotification` | Enqueue a message to channels |
| `GET /notifications` | `listNotification` | List delivery records (`?status=`) |
| `GET /notifications/{id}` | `getNotification` | Get a delivery record |

See [`openapi.yaml`](./openapi.yaml) for full schemas.

### Sending

`POST /notify` with a `body` (and optional `subject`). Target specific channels
with `channel_ids`, or omit it to fan out to every enabled channel you can
access. Each target becomes a `pending` notification; the worker delivers it
promptly and retries up to 5 attempts on failure, marking it `sent` or `failed`.

`POST /channels/{id}/test` delivers synchronously and returns the outcome
immediately — useful for validating a channel's config.

## Delivery & retries

A single background worker drains the durable queue. Claims use
`SELECT ... FOR UPDATE SKIP LOCKED` so multiple replicas never deliver the same
notification twice, and notifications left `pending` by a crash are recovered on
startup. Secret config values are stored as written but masked (`***`) in API
responses; submitting a channel update with a field still set to `***` preserves
the stored secret.

## Triggering from platform events

Notifications is a normal registry-driven service, so a **workflow step** can
target its `notifications/notify` action. Combined with the hooks service
(event → workflow), this gives event-driven notifications without any
notifications-specific wiring: a hooks rule triggers a workflow, and a workflow
step calls `/notify`.

## Adding a provider

1. Add `provider_<name>.go` implementing `Notifier` (`Info`, `Validate`, `Send`).
2. Register it from an `init()` with `RegisterNotifier`.
3. Add the type constant to `types.go`.

The new provider automatically appears in `GET /providers`, is accepted by
channel CRUD, and is delivered by the worker. No changes to the API, worker, or
schema are required.

## Configuration

| Env var | Description |
| --- | --- |
| `PORT` | Listen port (default `8094`) |
| `DATABASE_URL` | PostgreSQL DSN |
| `GATEKEEPER_URL` | Gatekeeper base URL |
| `GATEKEEPER_SERVICE_KEY` | Shared key for service registration / key rotation |

Provider credentials (Slack webhook URLs, SMTP passwords, webhook auth headers)
are **per-channel** config, not service-wide env vars.
