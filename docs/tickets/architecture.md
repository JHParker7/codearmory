# Tickets — Architecture

## Overview

Tickets is a general-purpose task tracker. It is an org-scoped CRUD service with threaded comments and optional plain-text references to resources in other platform services (workflow runs, forge executions).

```
Client (Bearer JWT)
  |
  v
Tickets :8086
  |
  +-- checkGatekeeper() --> POST /check_permissions on Gatekeeper
  |   returns (userID, orgID)
  |
  +-- canAccessTicket():
  |     ticket.created_by == userID
  |     OR (orgID != "" AND ticket.org_id == orgID)
  |     --> 404 Not Found on denial (not 403)
  |
  +-- GORM --> PostgreSQL (tickets + ticket_comments tables)
```

## Data model

```
tickets
  ticket_id          TEXT  PRIMARY KEY
  title              TEXT
  description        TEXT  DEFAULT ''
  status             TEXT  DEFAULT 'open'
  priority           TEXT  DEFAULT 'medium'
  created_by         TEXT  (user ID)
  org_id             TEXT  DEFAULT ''
  assignee_id        TEXT  (nullable -- user ID, no FK enforcement)
  workflow_id        TEXT  (nullable -- cross-service reference, plain text)
  run_id             TEXT  (nullable -- cross-service reference, plain text)
  forge_execution_id TEXT  (nullable -- cross-service reference, plain text)
  active             BOOL  DEFAULT true
  created_at         TIMESTAMPTZ
  updated_at         TIMESTAMPTZ

ticket_comments
  comment_id  TEXT  PRIMARY KEY
  ticket_id   TEXT  (logical FK, not enforced at DB level)
  author_id   TEXT  (user ID)
  body        TEXT
  active      BOOL  DEFAULT true
  created_at  TIMESTAMPTZ
  updated_at  TIMESTAMPTZ
```

Cross-service references (`workflow_id`, `run_id`, `forge_execution_id`) are stored as plain text. Tickets never validates that the referenced resource exists.

## Access model

Access is checked in the application layer before every database mutation or read:

```go
func canAccessTicket(t Ticket, userID, orgID string) bool {
    return t.CreatedBy == userID || (orgID != "" && t.OrgID == orgID)
}
```

- A user can always access their own tickets.
- Users in the same org can access each other's tickets.
- Out-of-org attempts receive `404 Not Found` rather than `403 Forbidden` to avoid leaking whether a ticket ID exists.

List queries apply the same filter at the database layer (`WHERE created_by=? OR org_id=?`) so inaccessible tickets are never included in responses.

## Status and priority

Status values: `open`, `in_progress`, `resolved`, `closed`. Any transition in any direction is permitted.

Priority values: `low`, `medium` (default), `high`, `critical`.

The metrics layer fires `tickets.resolved.total` when a `PUT /tickets/{id}` transitions a ticket from a non-terminal status to `resolved` or `closed`. This comparison is made in the handler against the pre-update status fetched from the DB.

## Comments

Comments are returned inline on `GET /tickets/{id}` and `PUT /tickets/{id}` (loaded from DB in both cases). List responses (`GET /tickets`) always include an empty `comments: []` array to keep payloads small without a second query per ticket.

Comment deletion is soft (`active = false`). The caller must be either the comment author or a user with org-level access to the parent ticket. Ticket owners and org-members can moderate any comment.

## Soft deletes

Every row carries `active BOOL`. Delete operations set `active = false`. All queries filter `WHERE active = true`. Deleted records remain in the database for audit purposes.

## Database

Two tables, managed with GORM AutoMigrate:

| Table | Primary key |
|-------|------------|
| `tickets` | `ticket_id` |
| `ticket_comments` | `comment_id` |

## Metrics

| Metric | Labels |
|--------|--------|
| `tickets.created.total` | `priority` |
| `tickets.resolved.total` | `status` (resolved / closed) |
| `tickets.comments.total` | `ticket.id` |
