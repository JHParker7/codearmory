# Phase 4 runbook — deploy read replicas + pull-through mirror

Phases 1–3 (code) are **merged**, and git_factory is now an in-repo core service built by
the monorepo CI. Phase 4 is the rollout: set config, optionally add git-node replicas,
then cut CI over. Do it in this order — each step is safe on its own and the feature stays
**off** until the final flip.

**Already done on the dogfood install:** the CI clones from git_factory directly
(`GIT_CLONE_URL: git:http://ca-codearmory-git-factory:9002/admin/codearmory.git`), and the
chart provisions `GIT_FACTORY_CLONE_TOKEN_KEY`, so clone tokens are live rather than inert.

**Before scaling past one replica, read `ARCHITECTURE.md` §5.** Multi-node here is the
*contingent* sharded-disk path (Step 4), which is only meant to be chosen if the shared
filesystem's write path fails a measurement that has not been taken. Shared storage
(Step 2) reaches the same goal without a routing table, primary election or version
marker, and its service and chart sides are ready.

## New configuration

### git-factory (control plane + every git-node run the same image)
| Var | Purpose | Notes |
|---|---|---|
| `GIT_HTTP_BASE_URL` | external clone base (already set) | must be reachable by runners |
| `GIT_FACTORY_INTERNAL_KEY` | auth for `/internal/mirrors` + `/internal/clone-token` | shared with git-connector; **secret** |
| `GIT_FACTORY_CLONE_TOKEN_KEY` | HMAC key for runner clone tokens | **secret**; rotating it invalidates outstanding clone URLs |
| `GIT_NODE_FORWARD_KEY` | node→node trust (proxy + replication) | **secret**; only needed once >1 node |
| `GIT_NODE_ADDRESS` | this node's own base URL | set per git-node so it recognizes itself; unset on a single node |
| `GIT_STORAGE_PREFLIGHT` | startup verification of the repo store | `enforce` (default) / `warn` / `off`. **Leave it enforcing.** It refuses to start on a filesystem lacking atomic `O_CREAT\|O_EXCL`, atomic `rename()` or enforced locking — the properties git's correctness rests on, and the ones object-storage FUSE mounts silently lack |
| `GIT_REF_LOCK_MAX_AGE` | stale ref-lock janitor threshold | default `1h`; `0` disables. **Must stay on with more than one pod** — a lock left by a killed pod blocks that ref from every pod, indefinitely, and nothing can distinguish it from a push in flight |

Feature is inert until `GIT_FACTORY_INTERNAL_KEY` (mirror) / `GIT_FACTORY_CLONE_TOKEN_KEY`
(clone tokens) are set. `GIT_NODE_FORWARD_KEY`/`GIT_NODE_ADDRESS` matter only in a
multi-node topology; a single node ignores placement entirely (empty table = all local).

### git-connector (`src/systems/git`, core, Helm-deployed)
| Var | Purpose |
|---|---|
| `GIT_FACTORY_URL` | git-factory base URL (e.g. `http://ca-codearmory-git-factory:9002`) |
| `GIT_FACTORY_INTERNAL_KEY` | same secret as git-factory's |

Both unset → `prefer_mirror` degrades to today's behaviour (upstream URL returned).

## Rollout order

1. **Publish images.** git-factory image to `ghcr.io/code-armory-app/git-factory` (its
   pipeline/operator — this session has no push access); git-connector via the monorepo
   CI/builder. Until git-factory's image carries Phases 1–3, none of the endpoints exist.
2. **Set secrets** (above) on both deployments. Generate three independent random keys.
3. **Roll git-factory and git-connector.** Single-node: done — mirror + clone-token now
   work, placement table empty so everything serves locally. No behaviour change yet.
4. **Enable the mirror for one backend (opt-in):**
   `PUT /git/backends/{id}` with `{"prefer_mirror": true}` on the Forgejo backend the CI
   clones through. Nothing mirrors until a clone-token is next requested for it.
5. **Verify:** trigger one workflow clone; confirm git-connector logs
   `mirror: serving clone from git-factory` and the runner clones from git-factory. Roll
   back instantly by setting `prefer_mirror:false` — clones return to upstream.

## Adding git-node replicas (optional, when one node isn't enough)

The single-node install needs **none** of this — skip unless clone load on the primary is
the bottleneck.

1. Deploy git-node(s) as a **StatefulSet** (each owns a PVC of bare repos), same image,
   with `GIT_NODE_ADDRESS` = its own service DNS and `GIT_NODE_FORWARD_KEY` set. The
   control-plane Deployment stays stateless.
2. Seed placement rows (Postgres `shard_nodes`): one `role=primary` row per shard (the
   node holding the bytes) and one `role=replica` row per replica address. Empty address =
   the local node. 256 shards (2-hex `shardOf`); start by placing all on one primary plus
   N replicas, refine later.
3. NetworkPolicy: allow control-plane pods → node pods on 9002, and node → node (for
   replication fetches).
4. Behaviour becomes: writes → primary; after each push the primary calls each replica's
   `/internal/replicate`; a replica serves reads only once `ReplicaState.Applied >=
   Repo.Version` (never stale). A replica that is down simply stays behind and is skipped.

## CI cutover (the actual "faster clones for workflows")

Once step 4 above is verified, the dogfood CI clones the mirror automatically (it goes
through git-connector's internal clone-token, and `prefer_mirror` rewrites the URL). No CI
YAML change is required — the URL git-connector returns changes, not the pipeline. If you
prefer an explicit switch, point `GIT_CLONE_URL` at the git-factory repo path directly.

## Local image build (minikube, no ghcr push access)

Step 1's "publish images" does not have to go through ghcr. To run this code on a local
minikube, build and side-load it:

```bash
# git-factory
cd src/control_plane
docker build -t ghcr.io/code-armory-app/git-factory:phase4-local .
minikube image load ghcr.io/code-armory-app/git-factory:phase4-local

# git-connector (monorepo)
cd src/systems/git
docker build -t ghcr.io/code-armory-app/git:phase4-local .
minikube image load ghcr.io/code-armory-app/git:phase4-local
```

minikube here runs **containerd**, not docker — `minikube image load` is required;
`eval $(minikube docker-env)` does not make the image visible to the kubelet. The
workloads use `imagePullPolicy: IfNotPresent`, so a side-loaded tag is used as-is and
never pulled.

Roll git-factory onto it:

```bash
kubectl -n codearmory set image deployment/ca-codearmory-git-factory \
  git-factory=ghcr.io/code-armory-app/git-factory:phase4-local
```

## Field notes from the first local rollout

Four things bit on the way in. Read these before repeating the exercise.

1. **`shard_nodes` keeps a single-column primary key on upgrade.** GORM's AutoMigrate
   adds columns but never alters a PRIMARY KEY, so an install that ran a pre-replica
   git-factory keeps `PRIMARY KEY (shard)` — which physically forbids a shard holding a
   primary *and* its replicas, silently breaking replication while a fresh install works
   fine. `widenShardNodePK` (runs at boot, Postgres-only, idempotent) repairs it.

2. **git-factory's Deployment is builder-managed, so patch the source of truth.** The
   image/config live in builder's `org_services` row, not the Deployment; builder
   reconciles every 30s (`BUILDER_RECONCILE_INTERVAL`). A raw `kubectl set image` is
   normally reverted.

3. **…except builder currently never reaches the Deployment for this service.** The
   deployed builder image predates `files/services/git_factory.json`, so it finds no def,
   derives the Secret name from the registry name (`ca-codearmory-codearmory_git_factory`,
   underscores → invalid RFC-1123), and `provision` fails. `EnsureService` returns before
   `applyDeployment`, so git-factory's workload is effectively frozen and a manual patch
   *does* stick. That is a broken-reconciler side effect, not a supported mechanism: once
   builder ships a git_factory def, it will start overwriting manual patches — set the
   `org_services` row instead.

4. **Config cannot currently be changed through builder's API.** `PUT /builder/services/
   {service}` 500s on any config edit (even writing the identical value back):
   `upsertOrgService` passes a raw `map[string]any` to `Updates`, which bypasses the
   `serializer:json` binding on the `config` column, and pgx has no encode plan for a Go
   map → `text`. Only the `Create` path (first write of a row) serializes correctly. Until
   builder marshals config explicitly in the updates map, new config keys have to be set
   when the row is first created, or written directly to the `org_services` row.

Unrelated but visible in the logs: git-factory reports `key rotation: unexpected status
401` because gatekeeper's `PERMITTED_SERVICES` does not list `codearmory_git_factory`.
That predates this work and is not caused by the rollout.

## Rollback

- Mirror: `prefer_mirror:false` on the backend → upstream clones resume immediately.
- Clone tokens / replicas: unset the keys / delete placement rows → single-node local
  serving, byte-identical to pre-Phase-4.
