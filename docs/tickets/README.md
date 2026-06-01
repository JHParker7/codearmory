# Tickets

General-purpose task tracker. Org-scoped tickets link to workflow runs and forge executions, and support threaded comments.

## How it works

Tickets are scoped to an organisation. Any user can create a ticket; it is visible to its creator and to any other user in the same org. Tickets may reference external resources by storing their IDs — linked `workflow_id`, `run_id`, and `forge_execution_id` values are plain text references with no cross-service validation.

Comments are nested under tickets and follow the same visibility rules as the parent ticket.

All authorisation is delegated to Gatekeeper via `POST {GATEKEEPER_URL}/check_permissions`. Tickets never verifies JWTs directly.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/tickets` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Gatekeeper base URL |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for key rotation with Gatekeeper |
| `PORT` | `8086` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `tickets` | OTel service name |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `DATABASE_URL_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

## API

All endpoints require `Authorization: Bearer <token>`, verified by Gatekeeper.

### Tickets

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/tickets` | `createTicket` on `tickets/tickets` | Create a ticket |
| `GET` | `/tickets` | `listTicket` on `tickets/tickets` | List accessible tickets |
| `GET` | `/tickets/{id}` | `getTicket` on `tickets/tickets/{id}` | Get a ticket with its comments |
| `PUT` | `/tickets/{id}` | `updateTicket` on `tickets/tickets/{id}` | Update a ticket |
| `DELETE` | `/tickets/{id}` | `deleteTicket` on `tickets/tickets/{id}` | Soft-delete a ticket |

### Comments

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/tickets/{id}/comments` | `createComment` on `tickets/tickets/{id}` | Add a comment |
| `DELETE` | `/tickets/{id}/comments/{comment_id}` | `deleteComment` on `tickets/tickets/{id}/comments/{comment_id}` | Delete a comment |

### Create a ticket

```bash
curl -X POST http://localhost:8086/tickets \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Deploy v1.2 to production",
    "description": "Coordinate the production deployment",
    "priority": "high",
    "assignee_id": "user-uuid",
    "workflow_id": "wf-uuid",
    "run_id": "run-uuid"
  }'
```

### Create request fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `title` | string | Yes | Short summary |
| `description` | string | No | Full description |
| `priority` | string | No | `low`, `medium` (default), `high`, `critical` |
| `assignee_id` | string | No | User ID of the assignee |
| `workflow_id` | string | No | ID of a linked workflow |
| `run_id` | string | No | ID of a linked workflow run |
| `forge_execution_id` | string | No | ID of a linked forge execution |

### Update request fields

Same as create, plus:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `status` | string | No | `open` (default), `in_progress`, `resolved`, `closed`. Omitting preserves the current value. |

### Ticket object

```json
{
  "ticket_id": "uuid",
  "title": "Deploy v1.2 to production",
  "description": "...",
  "status": "open",
  "priority": "high",
  "created_by": "user-id",
  "org_id": "org-id",
  "assignee_id": "user-id",
  "workflow_id": "wf-uuid",
  "run_id": "run-uuid",
  "forge_execution_id": null,
  "comments": [],
  "created_at": "...",
  "updated_at": "..."
}
```

### List filters

`GET /tickets` supports optional query parameters:

| Parameter | Description |
|-----------|-------------|
| `status` | Filter by status (`open`, `in_progress`, `resolved`, `closed`) |
| `priority` | Filter by priority (`low`, `medium`, `high`, `critical`) |
| `assignee_id` | Filter by assignee user ID |

List queries are filtered at the database layer — users only see tickets within their org.

### Status flow

```
open → in_progress → resolved → closed
```

Any status transition is permitted in any direction via `PUT /tickets/{id}`.

### Comments

```bash
# Add a comment
curl -X POST http://localhost:8086/tickets/{id}/comments \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"body": "Deployment complete, closing."}'

# Delete a comment
curl -X DELETE http://localhost:8086/tickets/{id}/comments/{comment_id} \
  -H "Authorization: Bearer <token>"
```

Comments are returned inline on `GET /tickets/{id}`, ordered by `created_at`. Only the comment author or a user with org-level access to the ticket can delete a comment.

## Access model

A ticket is visible to its creator and to any user in the same org. This scoping applies to get, update, delete, and comment endpoints — requests from users outside the org receive `404 Not Found`. List queries are filtered at the database layer rather than returning a filtered view of all tickets.

Linked resource IDs (`workflow_id`, `run_id`, `forge_execution_id`) are stored as plain text references. Tickets performs no cross-service validation on these values.

## Metrics

| Metric | Description |
|--------|-------------|
| `tickets.created.total` | Tickets created, labelled by `priority` |
| `tickets.resolved.total` | Tickets moved to `resolved` or `closed` status |
| `tickets.comments.total` | Comments added, labelled by `ticket.id` |

## Testing

```bash
# Unit tests
cd src/systems/tickets
go test ./...

# Integration tests
pip install -r tests/tickets/requirements.txt
TICKETS_URL=http://localhost:8086 GATEKEEPER_URL=http://localhost:8080 \
  pytest tests/tickets/ -v
```
