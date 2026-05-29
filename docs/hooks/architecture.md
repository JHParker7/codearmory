# Hooks — Architecture

## Overview

Hooks receives generic webhook events from external systems (git hosts, CI pipelines), matches them against configurable pipeline rules, and triggers workflow runs via a signed internal API call.

```
External system (git host, CI, etc.)
  |
  +-- POST /hooks -----------------------------------------> Hooks :8087
        |
        +-- Parse payload (repo, event, ref, commit, pusher, message)
        |   X-Hook-Event header overrides body.event when present
        |
        +-- INSERT hook_events (status=received)
        |
        +-- Query matching rules:
        |     WHERE repo=? AND active=true AND ?=ANY(events)
        |
        +-- For each matching rule:
        |     +-- Apply ref_filter (empty=any, exact, prefix glob "/*")
        |     +-- Verify X-Hub-Signature-256 HMAC if rule.secret is set
        |     |     mismatch -> skip this rule (other rules still run)
        |     +-- Build inputs: HOOK_* defaults + input_mapping overrides
        |     +-- POST /internal/workflows/{id}/runs (HMAC-signed)
        |
        +-- fire-and-forget: INSERT hook_triggers (one per matched rule)
        +-- fire-and-forget: UPDATE hook_events SET rules_matched, status
        +-- Return 200 with HookEvent (always, even on partial failure)
```

## Pipeline rules

A pipeline rule maps a repository + event type combination to a workflow run.

```
pipeline_rules
  rule_id       TEXT  PRIMARY KEY
  repo          TEXT  -- exact match on payload.repo
  events        JSONB ([]string) -- event types that trigger this rule
  ref_filter    TEXT  -- see ref matching below
  workflow_id   TEXT  -- workflow to trigger
  secret        TEXT  -- HMAC-SHA256 secret (write-only, never returned)
  input_mapping JSONB -- { workflow_key: payload_field }
  created_by    TEXT
  org_id        TEXT
  active        BOOL
```

The `secret` field is stored but never returned by the API. Update semantics on `PUT /rules/{id}`: omit the field (JSON null) to leave the secret unchanged; send `""` to clear it; send a non-empty string to replace it.

### Ref filter matching

| Filter | Behaviour |
|--------|-----------|
| `""` (empty) | Matches any ref |
| `"refs/heads/main"` | Exact match |
| `"refs/heads/*"` | Any ref starting with `refs/heads/` |

The trailing `*` must be immediately preceded by `/` — patterns like `refs/heads*` are treated as exact strings, not globs.

### HMAC verification

When a rule has a `secret`, the request must carry:

```
X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, raw_body))>
```

This is the same scheme as GitHub webhooks. An HMAC mismatch skips that rule; it does not produce a 4xx response, and other rules for the same event still run. The raw body is read before JSON parsing so the HMAC covers exactly the bytes the sender signed.

## Workflow dispatch

Hooks calls Workflows through the internal endpoint, authenticating with an HMAC token:

```
token = hex(HMAC-SHA256(HOOKS_TRIGGER_KEY,
             "hooks:{workflow_id}:{triggered_by}:{timestamp}"))

POST /internal/workflows/{id}/runs
  X-Hooks-Token: {token}
  X-Hooks-Timestamp: {unix_seconds}
  { triggered_by, org_id, inputs }
```

This lets Workflows verify the trigger originated from a trusted Hooks instance without a user JWT.

## Input mapping

System defaults are always injected first:

| Key | Value |
|-----|-------|
| `HOOK_REPO` | `payload.repo` |
| `HOOK_EVENT` | `payload.event` |
| `HOOK_REF` | `payload.ref` |
| `HOOK_COMMIT` | `payload.commit` |

`input_mapping` then adds or overrides additional keys from the payload:

```json
{ "COMMIT_SHA": "commit", "BRANCH": "ref", "REPO": "repo" }
```

Workflow steps receive all of these as run inputs, available for `${KEY}` substitution in paths, bodies, and headers.

## Event recording

Every inbound webhook creates a `hook_events` row before rule matching, guaranteeing a record even when no rules match. The trigger row inserts and the `rules_matched` / `status` update happen in fire-and-forget goroutines after the response is sent, so slow DB writes do not delay the caller.

Event status values:

| Status | Meaning |
|--------|---------|
| `received` | No rules matched (or no active rules for this repo) |
| `triggered` | All matched rules dispatched successfully |
| `failed` | All matched rules failed to dispatch |
| `partial` | Some dispatched, some failed |

## Access model

Rules are org-scoped. A user can see and manage rules they created, or rules owned by members of their org. Event history follows the same scoping — an event is accessible if any of its triggered rules is accessible, or if the caller has an active rule for the event's repo (fallback for no-match events). Out-of-org requests receive `404 Not Found`, not `403 Forbidden`.

## Database

Three tables, managed with GORM AutoMigrate:

| Table | Purpose |
|-------|---------|
| `pipeline_rules` | Rule definitions |
| `hook_events` | Inbound webhook record |
| `hook_triggers` | Per-rule dispatch outcome (run_id if successful) |

On first startup, Hooks runs a safe migration that converts any pre-existing `TEXT[]` `events` column to `JSONB` (wrapped in `recover()` so it is a no-op on a fresh database).

## Metrics

| Metric | Labels |
|--------|--------|
| `hooks.received.total` | `repo` |
| `hooks.rules_matched.total` | `repo`, `workflow.id` |
| `hooks.runs_triggered.total` | `workflow.id` |
