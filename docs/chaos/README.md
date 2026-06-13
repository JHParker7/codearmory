# Chaos

Chaos-engineering control plane — the first integration built on the [outpost framework](../outpost/README.md). It is a **thin control plane with no Kubernetes code**: it drives an outpost via the [outpost-gateway](../outpost-gateway/README.md) and records the verdicts the outpost reports back. The actual Litmus ChaosEngine/ChaosResult handling lives entirely in the outpost's chaos module.

## How it works

1. A user creates an experiment (`POST /experiments`), choosing a target outpost, an experiment type, and the target workload (namespace + label selector).
2. Chaos stores the experiment as `pending` and enqueues a `run-experiment` command to the gateway for that outpost. It returns `202` immediately.
3. The outpost's chaos module materialises a Litmus **ChaosEngine** against the target workload, then reports a `run-started` event and later a `verdict` event.
4. The gateway dispatcher delivers those events to `POST /internal/events`. Chaos matches them by `experiment_id`, advances the experiment's status, and emits hooks lifecycle events.

Chaos holds **no cluster credentials**. All authorisation for user requests is delegated to Gatekeeper.

## Status model

Experiment status strings are written verbatim from the outpost's verdict and **must** match the success/failure states declared for the `chaos/run-experiment` workflow action in the registry manifest (a unit test asserts this).

| Status | Meaning |
|--------|---------|
| `pending` | Command enqueued, awaiting the outpost |
| `running` | Outpost acknowledged and started the experiment |
| `Pass` | Litmus verdict `Pass` (terminal, success) |
| `Fail` | Litmus verdict `Fail` (terminal, failure) |
| `Error` | Errored, or completed with no decisive verdict (terminal, failure) |
| `stopped` | User stopped the experiment (terminal) |

## Experiment types

| Type | Default params |
|------|----------------|
| `pod-delete` | `TOTAL_CHAOS_DURATION=30`, `CHAOS_INTERVAL=10`, `PODS_AFFECTED_PERC=50` |
| `pod-network-latency` | `TOTAL_CHAOS_DURATION=60`, `NETWORK_LATENCY=2000` |

Caller-supplied `params` are merged over the type defaults.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/chaos` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | Gatekeeper base URL |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for key rotation with Gatekeeper |
| `OUTPOST_GATEWAY_URL` | — | outpost-gateway base URL (where commands are enqueued) |
| `OUTPOST_INTERNAL_KEY` | — | Shared HMAC secret for the internal command/event plane. Must match the gateway's value. |
| `HOOKS_URL` | — | Hooks service base URL. Set with `HOOKS_TRIGGER_KEY` to emit experiment events. |
| `HOOKS_TRIGGER_KEY` | — | Shared HMAC secret for emitting experiment events to hooks. |
| `PORT` | `8090` | Port the server listens on |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant.

## API

All user endpoints require `Authorization: Bearer <token>`, verified by Gatekeeper.

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/experiments` | `createExperiment` on `chaos/experiments` | Start an experiment (enqueues a command); returns `202` |
| `GET` | `/experiments` | `listExperiment` on `chaos/experiments` | List the caller's experiments |
| `GET` | `/experiments/{id}` | `getExperiment` on `chaos/experiments/{id}` | Get an experiment |
| `DELETE` | `/experiments/{id}` | `deleteExperiment` on `chaos/experiments/{id}` | Stop and remove an experiment |
| `GET` | `/experiment-types` | `listExperimentType` on `chaos/experiment-types` | List supported experiment types |

`POST /internal/events` consumes outpost events from the dispatcher (shared-key HMAC, not user-facing). Full schema: [openapi.yaml](openapi.yaml).

### Start an experiment

```bash
curl -X POST http://localhost:8090/experiments \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "outpost_id": "<outpost-id>",
    "experiment_type": "pod-delete",
    "target_app_ns": "demo",
    "target_app_label": "app.kubernetes.io/component=conductor",
    "target_app_kind": "deployment"
  }'
# → 202 {"experiment_id":"uuid","status":"pending", ...}
```

## Workflows integration

Chaos publishes a `chaos/run-experiment` async action, so a pipeline can **gate on a chaos experiment**: the step submits an experiment and polls until it reaches a terminal state, succeeding on `Pass` and failing on `Fail`/`Error`.

```yaml
steps:
  - name: chaos-gate
    action: chaos/run-experiment
    with:
      outpost_id: <outpost-id>
      experiment_type: pod-delete
      target_app_ns: demo
      target_app_label: app.kubernetes.io/component=conductor
```

## Hooks integration

When `HOOKS_URL` + `HOOKS_TRIGGER_KEY` are set, chaos emits lifecycle events to the [hooks](../hooks/README.md) service (source `chaos`):

| Event | When |
|-------|------|
| `experiment.started` | An experiment is created |
| `experiment.passed` | An experiment reaches `Pass` |
| `experiment.failed` | An experiment reaches `Fail`/`Error` |

## Access model

An experiment is visible to its creator and to any user in the same org; requests from outside receive `404`. Verdicts are correlated by `experiment_id`, so duplicate at-least-once events for a terminal experiment are no-ops.

## Metrics

| Metric | Description |
|--------|-------------|
| `chaos.experiments.created.total` | Experiments created, labelled by `experiment_type` |
| `chaos.experiments.resolved.total` | Experiments that reached a terminal verdict, labelled by `status` |

## Testing

```bash
cd src/systems/chaos && go test ./...
docker compose --profile test run --rm chaos-integration-tests
```
