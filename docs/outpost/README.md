# Outpost & the Integration Framework

The **outpost** is a single, customer-deployed agent that is the *only* thing in the platform that ever touches an in-cluster system (Litmus chaos CRDs, Argo CD, …). CodeArmory's control-plane services never reach into a customer cluster — they **drive the outpost with commands** and **consume its data as events**, then weave both into the rest of the platform (workflows, hooks, portal).

This page describes the framework. The pieces that build on it have their own docs:

- **[outpost-gateway](../outpost-gateway/README.md)** — the outpost-facing connection point + Postgres event backbone.
- **chaos** — chaos-engineering control plane (first integration; now a separate `codearmory-chaos` repo).
- **argo** — Argo CD sync control plane (second integration; now a separate `codearmory-argo` repo).

## Why an outpost

Reaching into a customer's cluster from the control plane would mean the control plane holding cluster credentials and needing inbound network access to every customer. Instead, exactly one component — the outpost — runs *inside* (or against) the target cluster and **dials out** to the control plane over HTTPS. Benefits:

- **Zero inbound access.** The outpost long-polls for commands and POSTs events outbound only. Nothing connects *into* the customer cluster.
- **Zero control-plane cluster credentials.** All Kubernetes/CRD/Argo code lives in the outpost's modules; control-plane services hold none.
- **Uniform self-hosted and SaaS.** One code path. Self-hosted simply runs the outpost in the same cluster as the control plane against the in-cluster gateway; SaaS runs it in the customer's cluster pointing at the gateway over a private link. The mechanism is identical.

## Architecture

```
 customer (or self-hosted) cluster            CodeArmory control plane
 ┌─ outpost (one deploy) ──────────┐   HTTPS   ┌─ outpost-gateway ───────────────┐
 │ modules (user-enabled):         │  outbound │  enroll · long-poll commands ·  │
 │  • chaos  → litmus CRDs         │ ◄────────►│  ingest events (outpost key)    │
 │  • argo   → Argo CD Application │ long-poll └──────────┬──────────────────────┘
 │ in-cluster RBAC per module      │   + POST    Postgres backbone
 └─────────────────────────────────┘            outpost_commands (queue, SKIP LOCKED)
                                                 outpost_events  (outbox + dead-letter retry)
                                                          │ dispatch by integration (HTTP, HMAC)
                              user ─conductor─►  chaos svc · argo svc · …  ─► workflows / hooks / portal
```

- **Commands** (control → outpost): a control service enqueues `{outpost_id, integration, type, payload}` via the gateway's internal API; the outpost long-polls, and the matching module handles it.
- **Events** (outpost → control): a module emits `{integration, type, payload}`; the outpost POSTs it to the gateway, which writes it to a transactional outbox and dispatches it to the integration's consumer service.

The command/event envelope is kept transport-agnostic: it is carried today by long-poll + outbound POST (stateless, replica-agnostic on the shared Postgres queue, no socket infrastructure), and a WebSocket carrier could be added later without touching any integration service.

## The outpost agent (`src/systems/outpost`)

A small Go binary. Its only outbound dependency is the outpost-gateway. Lifecycle:

1. **Enroll.** On first start it exchanges a single-use **enrollment token** (minted in the portal) for a long-lived **outpost key** via `POST /outpost/register`, and persists the key so a restart does not need a fresh token.
2. **Run.** It loops: `GET /outpost/commands` (long-poll, ≤30 s) → route each command to its module's `HandleCommand` → POST any resulting events and ack the command. A background goroutine sends periodic heartbeats; each module's `Start` runs watches/informers that emit events as cluster state changes.
3. **Reconnect.** All requests are outbound HTTPS with exponential backoff on failure.

### Module framework

Each integration is one pluggable **module**. Adding an integration touches neither the outpost core nor the gateway — you add a `Module` implementation and a control-plane consumer service.

```go
type Command struct { ID, Integration, Type string; Payload map[string]any }
type Event   struct { EventID, Integration, Type string; Payload map[string]any }

type Module interface {
    Name() string                                                  // e.g. "chaos"
    HandleCommand(ctx context.Context, c Command) ([]Event, error) // imperative actions
    Start(ctx context.Context, emit func(Event)) error             // watches/streams
}
```

- **chaos module** — `client-go` dynamic client + dynamic informer over `litmuschaos.io/v1alpha1` chaosengines/chaosresults. `run-experiment` builds a ChaosEngine and creates it; `stop` deletes it. A ChaosResult informer reads `status.experimentStatus.{verdict,phase,failStep,probeSuccessPercentage}` defensively and emits `verdict` events.
- **argo module** — drives `argoproj.io` Applications through the same dynamic client (no separate Argo API credentials). `sync` patches the Application's `operation` field (which Argo CD reconciles); an Application informer emits `app-state` events.

## Idempotency & delivery guarantees

- Commands and events both carry IDs; consumers dedupe (at-least-once, idempotent).
- The command queue is claimed with `FOR UPDATE SKIP LOCKED` (any gateway replica serves any outpost).
- The event outbox is delivered by a poller with dead-letter retry and exponential backoff (the hooks/workflows idiom — no message broker).
- Consumers correlate by a stable key: chaos by `experiment_id`, argo by `outpost_id` + `app_name`.

## Authentication

| Hop | Mechanism |
|-----|-----------|
| Outpost → gateway (register) | single-use enrollment token (`<outpost_id>.<secret>`), bcrypt-verified |
| Outpost → gateway (commands/events/heartbeat) | `X-Outpost-ID` + `Authorization: Bearer <outpost-key>`, bcrypt-verified |
| Control service → gateway (`/internal/commands`) | shared-key HMAC-SHA256 (`X-Internal-Token` + 30 s window) |
| Gateway dispatcher → consumer (`/internal/events`) | same shared-key HMAC |
| User → outpost management / chaos / argo (via conductor) | Gatekeeper bearer JWT |

The shared internal key (`OUTPOST_INTERNAL_KEY`) is held by the gateway and every integration consumer; the per-outpost keys are owned by the gateway.

## Deploying an outpost

Use the dedicated, customer-installable chart at `infra/helm/outpost/` (see its [README](../../infra/helm/outpost/README.md)):

```bash
helm install my-outpost infra/helm/outpost \
  --namespace codearmory-outpost --create-namespace \
  --set controlPlaneURL=https://gateway.example.com \
  --set modules="chaos\,argo" \
  --set enrollmentToken=<token-from-portal>
```

The chart grants least-privilege RBAC per enabled module (chaos: `litmuschaos.io` CRDs + a runner ServiceAccount per target namespace; argo: `argoproj.io` Applications) and persists the outpost identity across restarts.

## Adding a new integration

1. **Outpost module** — implement `Module` in `src/systems/outpost/module_<name>.go`.
2. **Consumer service** — copy the chaos/argo skeleton: enqueue commands to the gateway, consume `/internal/events`, expose a user API and (optionally) a workflows async action.
3. **Manifest entries** — register the service + its async action + default grants in `infra/local/registry-manifest.json` and the Helm copy.
4. **Wiring** — add the service-key, compose/Helm templates, a database, and a portal page.

The outpost core, the gateway, and the event backbone are untouched — that is the test the framework is designed to pass, and what the argo integration demonstrates.
