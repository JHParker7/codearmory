# Workflows — Architecture

## Overview

Workflows orchestrates graphs of HTTP steps against registered backend services. A workflow is a named set of steps plus the routes between them; each step makes one HTTP request. A run walks that graph: a step becomes ready once every inbound route is resolved and at least one was taken, and routes may carry conditions, so a pipeline can branch, join, and handle failure. Runs are queued in PostgreSQL and executed asynchronously by a background worker pool.

A workflow with no explicit `routes` has them derived as a plain chain in array order — a bare step array is a sequence, so an ordered list remains a valid way to author a simple pipeline.

Routes are the only encoding for parallelism: two edges out of one step fork the run, two edges into one step join it. Fan-out *within* a step (`matrix`, `scatter`, or a map region) is that step's own concern and carries its own `max_concurrent`.

```
API caller (Bearer JWT)
  |
  +-- POST /workflows/{id}/runs
        |
        +-- checkGatekeeper() --> POST /check_permissions on Gatekeeper
        |
        +-- Store caller's Bearer token in workflow_runs.token
        |
        +-- INSERT workflow_runs (status=pending)

        WorkerPool (5 goroutines)
          |
          +-- SELECT ... FROM workflow_runs
          |   WHERE status='pending' FOR UPDATE SKIP LOCKED LIMIT 1
          |
          +-- UPDATE workflow_runs SET status='running'
          |
          +-- For each step (in order):
          |     +-- Resolve service URL from serviceURLs map
          |     +-- Substitute ${KEY} in path / body / headers (from run.inputs)
          |     +-- HTTP request with Authorization: Bearer {run.token}
          |     +-- Evaluate response (expected_status or any 2xx)
          |     +-- INSERT/UPDATE workflow_step_runs
          |     +-- On failure: mark run failed, stop
          |
          +-- UPDATE workflow_runs SET status, ended_at, token=NULL
                (token cleared on every terminal state)
```

## Worker pool

Five goroutines poll the `workflow_runs` table concurrently. Concurrent access is serialised at the database level with `SELECT ... FOR UPDATE SKIP LOCKED` — each pending run is claimed by exactly one goroutine, with no external queue needed.

Each goroutine runs a tight poll loop: `tryOne()` → short sleep → repeat. On shutdown (context cancellation), goroutines finish their current step and exit cleanly.

### Run cancellation

`DELETE /runs/{id}` is a two-step operation:

1. `UPDATE workflow_runs SET status='cancelled', ended_at=now(), token=NULL WHERE run_id=? AND status IN ('pending','running')` — the DB write is the authoritative cancellation.
2. `pool.Cancel(id)` — sends a best-effort signal to the goroutine handling that run via a stored `context.CancelFunc`.

The worker's final UPDATE guards on `WHERE status='running'` so a race between a completing worker and a cancellation request always produces a consistent terminal state.

### Run leases (surviving a rolling deploy)

A claimed run is owned by exactly one pod, and that ownership has to stay decidable when the pod disappears mid-run. The claiming worker stamps a **lease** as it dequeues the run — not from its own heartbeat loop, so a run can never sit `running` with a NULL lease for a peer to misread — then refreshes it every `runLeaseHeartbeat` (30s) for as long as it holds the run. A peer may reclaim a run only once its lease has gone unrefreshed for `runLeaseStaleAfter` (5m).

The order-of-magnitude gap between the two is deliberate: a worker blocked on a slow step, a stop-the-world GC pause or a brief database blip must not be mistaken for a dead one. The cost of erring long is only that a genuinely crashed run is reclaimed a few minutes later. The staleness cutoff is evaluated by the **database's** clock, not the pod's, so skew between replicas cannot make one pod steal another's live run.

### Cancelling abandoned remote jobs

A step that submits an **async job** owns a resource on the target service for as long as that job lives, and abandoning the step does not release it. Forge admits new work against the *reservation* of everything it still considers running, so a single stranded execution can hold enough CPU and memory to freeze a whole cluster's pipelines.

The normal path is per-step: `pollAction`'s exit cancels the job it was polling. What that misses is a run whose worker died — nothing is left in memory to run that exit. So when the **lease sweep** reaps a run whose worker stopped heartbeating, and when **startup recovery** reclaims runs abandoned by a worker that is definitely gone, workflows calls the action's cancel endpoint for every job those runs still have outstanding (`cancelAbandonedRunJobs` in `jobcancel.go`). A reaper has no in-memory job ids at all, only what the step runs recorded, and the credential it uses is the token stored on the run row — a reaped run's session is left to expire rather than revoked, precisely so it is still good for the cancel.

It is best-effort and time-boxed (`asyncCancelTimeout`, 5s per job), and each run is handled independently with failures logged and skipped: the sweep must reclaim the rest of its batch regardless, and a failed cancel must never block reclaiming a run.

## Step execution

### Service URL resolution

Each step names a service (`step.service`). The URL is looked up in the in-memory `serviceURLs` map, populated from the `SERVICES` environment variable at startup. `gatekeeper` is always pre-populated from `GATEKEEPER_URL`. An unknown service name fails the step immediately.

### Input substitution

`${KEY}` placeholders in `step.path`, `step.body`, and `step.headers` are replaced with the corresponding value from `run.inputs` at execution time. Placeholders for unknown keys are left unchanged.

```
step.path  = "/state/${USER}/${ENV}"
run.inputs = { "USER": "alice", "ENV": "staging" }
--> actual path: "/state/alice/staging"
```

### Token forwarding

The Bearer token that triggered the run is stored in `workflow_runs.token` and forwarded as `Authorization: Bearer {token}` on every step request. Steps execute with the same permissions as the original caller. The token is removed from the database immediately when the run reaches a terminal state.

### Response evaluation

A step succeeds when:
- `expected_status` is non-zero: the response status code must match exactly.
- `expected_status` is 0: any `2xx` response succeeds.

Any other outcome fails the step (and the run). The response body (up to 1 MB) is stored in `workflow_step_runs.response_body`.

## Internal trigger endpoint

`POST /internal/workflows/{id}/runs` is used exclusively by the Events service. It bypasses the Gatekeeper JWT check and instead verifies an HMAC-SHA256 token:

```
X-Hooks-Token: hex(HMAC-SHA256(EVENTS_TRIGGER_KEY,
                   "hooks:{workflow_id}:{triggered_by}:{timestamp}"))
X-Hooks-Timestamp: {unix_seconds}
```

The timestamp must fall within a ±5 minute window to prevent replay attacks.

## Data model

Three tables, managed with GORM AutoMigrate:

```
workflows
  workflow_id  TEXT  PRIMARY KEY
  name         TEXT
  description  TEXT  DEFAULT ''
  steps        JSONB ([]WorkflowStep)
  created_by   TEXT
  org_id       TEXT  DEFAULT ''
  active       BOOL  DEFAULT true
  created_at   TIMESTAMPTZ
  updated_at   TIMESTAMPTZ

workflow_runs
  run_id        TEXT  PRIMARY KEY
  workflow_id   TEXT
  triggered_by  TEXT
  org_id        TEXT  DEFAULT ''
  status        TEXT  DEFAULT 'pending'
  current_step  INT   DEFAULT 0
  inputs        JSONB (map[string]string)
  token         TEXT  -- caller's Bearer JWT; NULL once terminal
  created_at    TIMESTAMPTZ
  started_at    TIMESTAMPTZ (nullable)
  ended_at      TIMESTAMPTZ (nullable)

workflow_step_runs
  step_run_id     TEXT  PRIMARY KEY
  run_id          TEXT
  step_index      INT
  step_name       TEXT
  status          TEXT
  response_status INT   (nullable)
  response_body   TEXT  (nullable, up to 1 MB)
  started_at      TIMESTAMPTZ (nullable)
  ended_at        TIMESTAMPTZ (nullable)
```

## Run and step lifecycle

```
Run:   pending --> running --> completed
                           --> failed
                           --> cancelled

Step:  running --> completed
               --> failed
               --> cancelled (run cancelled during this step)
```

A run fails as soon as any step fails; subsequent steps are not executed. Step run rows are only created when the worker reaches that step — there are no pre-created `pending` step rows.

## Access model

Workflows and runs are org-scoped. A user can access resources they created or those belonging to members of their org. Out-of-org requests receive `404 Not Found`.

## Metrics

| Metric | Labels |
|--------|--------|
| `workflows.runs.triggered.total` | `workflow.id` |
| `workflows.runs.completed.total` | `workflow.id`, `status` |
| `workflows.steps.completed.total` | `workflow.id`, `status` |
