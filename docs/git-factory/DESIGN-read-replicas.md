# Design: read replicas + pull-through mirror

Status: **built.** This started as a proposal; everything in §2 below has since landed.
Kept as the design rationale for that code rather than as a plan — read §2 for what is
actually true today. Companion to `ARCHITECTURE.md` (§5 scaling roadmap).
Goal owner: make `git clone` fast for the Workflows/Forge CI plane, and let git-factory
scale clone (read) traffic horizontally.

---

## 1. What we're actually building (two capabilities, one architecture)

Two distinct capabilities came out of the scoping discussion. They compose into one model.

**A. Pull-through mirror / cache.** Most repos the CI clones don't originate in git-factory —
they live on an upstream backend (Forgejo at `192.168.53.171:3000`, GitHub, GitLab) brokered by
the **git-connector** credential broker. git-factory can hold a **mirror** of such a repo and
keep it warm, so a workflow clones from git-factory's local volume (in-cluster, warm, offloaded)
instead of paying a full clone against the upstream every run. This is the piece that directly
answers *"make git clone faster for workflows"* and it does **not** require Step 3/4.

**B. Read replicas (roadmap Step 3 + 4).** git-factory scales itself: repo bytes live on
`git-node`s, each repo replicated to `R` nodes; **reads** (`git-upload-pack`) fan out to any
replica, **writes** (`git-receive-pack`) go to the primary, and a per-repo version marker stops a
clone right after a push from being served stale.

**How they compose.** A mirror is just a `Repo` in git-factory with `Kind=mirror` + an upstream
URL. Once it's a git-factory repo, it automatically inherits (B)'s replica fan-out. So the mirror
gives the *immediate* CI win on a single node; replicas later scale that win across nodes. Build A
first, B second — A ships value without B.

---

## 2. Current state (grounded in the code)

| Capability | Status | Evidence |
|---|---|---|
| Single node, in-process git | ✅ | `gitplane.go` shells out to `git-upload-pack`/`git-receive-pack` against `GIT_STORAGE_ROOT` |
| Routing table | ✅ | `shard.go`: `ShardNode{Shard,Address}`, `resolveNode()`, `shardOf()`; `Repo.Shard` |
| Remote git-node + reverse proxy | ✅ | `proxy.go`: `maybeProxyToNode()`/`proxyToNode()`, wired into the wire path at `api_git_http.go:270,303`; `GIT_NODE_FORWARD_KEY` trust header, and a trusted forward is served locally so there is no loop |
| Replication + read fan-out | ✅ | `replicate.go` (`bumpRepoVersion`, fetch-driven push to replicas, `recordApplied`); `Repo.Version` + `ReplicaState.Applied`; `shard.go: pickReadNode()` routes reads only to a replica with `Applied >= Version` |
| Pull-through mirror | ✅ | `Repo.Kind`/`UpstreamURL`/`MirrorAt`, `api_mirror.go`, `POST /internal/mirrors`; git-connector's `PreferMirror` backend flag hands back git-factory's URL (`api_credentials.go`) |

The dogfood CI now clones **from git-factory**, not Forgejo:
`infra/ci/codearmory-ci.yaml` sets
`GIT_CLONE_URL: git:http://ca-codearmory-git-factory:9002/admin/codearmory.git`,
reaching it directly over the pod network.

### Where this sits against `ARCHITECTURE.md`'s numbering — read this before comparing

The two documents number the roadmap **differently**, which is a live source of
confusion when someone reads them together.

`ARCHITECTURE.md` §5 is: Step 1 single node → **Step 2 shared storage (JuiceFS)** →
Step 3 stateless pods behind one Service → **Step 4 (contingent) sharded local disk**,
where Step 4 is the thing that needs a routing table, a primary, replicas and a version
marker. This document numbers its own phases and calls the routing table "Step 2".

So in `ARCHITECTURE.md`'s terms, what §2 above reports as built is essentially the
**Step 4** machinery — the *contingent* branch — and it was built **before** Step 2.
That is the reverse of the roadmap order, and Step 4 was explicitly supposed to be gated
on a measurement (`ARCHITECTURE.md` §5: "it is a measurement, not a guess") that has not
been taken. It is not wasted work — the mirror and read fan-out answer a real CI latency
problem on their own, which is the §1 argument here — but it does mean the sharded-disk
complexity `ARCHITECTURE.md` hoped to avoid partly exists already.

Shared storage (Step 2) remains the unbuilt piece: `GIT_STORAGE_ROOT` still resolves to a
local path, so `localDirFor()` returns `errRemoteNodeUnsupported` for a repo placed on
another node — the *wire* path proxies, but the API endpoints that touch the disk
directly (commits, blobs, gc) cannot serve a remote repo.

---

## 3. Target architecture

```
   Workflows / Forge runner
      │  git clone  (authenticated URL from git-connector)
      ▼
   ┌──────────────────────────────────────────────┐
   │ git-factory CONTROL PLANE (stateless, N pods) │
   │  • gatekeeper auth, repo metadata (Postgres)  │
   │  • routing: repo → shard → {primary, replicas}│
   │  • read  → reverse-proxy to a healthy replica │
   │  • write → reverse-proxy to the primary       │
   │  • mirror control: fetch upstream on demand   │
   └───────────────┬───────────────┬──────────────┘
        upload-pack │ (read)        │ receive-pack (write)
      ┌─────────────▼──┐   ┌────────▼────────┐   replication (post-receive
      │ git-node A     │──►│ git-node B      │    hook / async pack push)
      │ (primary for   │   │ (replica for    │◄─────────────────────────
      │  shard 00..7f) │   │  shard 00..7f)  │
      │ owns bare repos│   │ synced copy     │
      └────────────────┘   └─────────────────┘

   Mirror flow (capability A):
     git-connector  ──"repo connected: <upstream-url>"──►  git-factory
        control plane creates/refreshes a Kind=mirror Repo, git-node
        runs `git fetch --prune <upstream>` into the bare repo.
```

---

## 4. Data model changes (`src/control_plane/types.go`)

### 4a. Repo — add mirror fields
```go
type Repo struct {
    // ... existing ...
    Kind        string `gorm:"default:native" json:"kind"`   // "native" | "mirror"
    UpstreamURL string `json:"upstream_url,omitempty"`        // set when Kind=mirror; the brokered source
    MirrorAt    *time.Time `json:"mirror_at,omitempty"`       // last successful fetch from upstream
    // Version is bumped on every accepted write (or mirror fetch that moved a ref); a
    // replica may serve a read only when its applied Version >= the repo's Version.
    Version     int64  `gorm:"default:0" json:"-"`
}
```
`native` = born in git-factory (today's behaviour, push-authoritative). `mirror` = a cached copy
of `UpstreamURL`; writes to a mirror are refused at the wire (read-only), it's refreshed by
fetching upstream.

### 4b. Replace `ShardNode` (one address) with a placement + node table
```go
// A git-node process that owns disk.
type GitNode struct {
    ID        string `gorm:"primaryKey" json:"id"`      // "git-node-a"
    Address   string `json:"address"`                   // "http://git-node-a:9002"; "" = this process
    Healthy   bool   `gorm:"default:true" json:"healthy"`
    LastSeen  time.Time `json:"last_seen"`
}

// One row per (shard, node) — the replica set for a shard.
type ShardPlacement struct {
    Shard   string `gorm:"primaryKey" json:"shard"`     // 2-char id prefix (shardOf)
    NodeID  string `gorm:"primaryKey" json:"node_id"`
    Role    string `json:"role"`                        // "primary" | "replica"
    Applied int64  `json:"applied"`                     // highest repo Version this node has for the shard
}
```
`resolveNode()` becomes `resolvePlacement(shard) → {primary GitNode, healthy replicas []GitNode}`.
Empty table ⇒ everything local (single-node install unchanged — the current invariant is kept).

---

## 5. Routing & read/write split

- `gitplane` already distinguishes `svcUploadPack` (read) from `svcReceivePack` (write). Lift that
  distinction up to the **request router**: `handleUploadPack`/`handleInfoRefs?service=upload-pack`
  are reads; `handleReceivePack`/`info/refs?service=receive-pack` are writes.
- **Read:** resolve placement → pick a healthy replica whose `Applied >= repo.Version` (fall back
  to primary if none is caught up) → if local, run in-process (today's path); if remote,
  **reverse-proxy** the wire request (Step 3).
- **Write:** always the primary. Reject writes to `Kind=mirror` repos with `403` +
  `WWW-Authenticate` semantics preserved.
- **Reverse proxy = `httputil.ReverseProxy`** (per `ARCHITECTURE.md §5 Step 3` rationale — Smart
  HTTP is already HTTP; stream it, don't re-frame in gRPC). One proxy for both directions; only the
  target node differs.

---

## 6. Replication & staleness (the hard part — Step 4)

- **Trigger:** a git-node `post-receive` hook (native repos) or a successful mirror `fetch` bumps
  `Repo.Version` and enqueues replication.
- **Mechanism (v1):** primary pushes packs to each replica node's internal endpoint
  (`git push --mirror` over the node's own git wire, or an internal `POST /replicate` that runs
  `git fetch` from the primary). Async, at-least-once, retried; `ShardPlacement.Applied` is advanced
  when a replica confirms it reached `Version`.
- **Consistency the CI needs:** a clone that must see commit `X` (the SHA the pipeline just pushed
  or asked to mirror) must not be served by a replica that hasn't got `X`. The `Applied >= Version`
  gate covers the general case; for the exact-SHA case, expose a `?min_version=` / ref-existence
  check and route to the primary when no replica qualifies. **Writes are always strongly consistent
  (primary only); reads are read-your-writes via the version gate.**
- Serialize writes per repo (ref-lock) — already noted in `ARCHITECTURE.md §6`.

---

## 7. Pull-through mirror flow (git-connector integration — capability A)

This is the piece that makes workflow clones fast **without** Step 3/4, so it ships first.

1. **On repo connection**, git-connector (which already knows the upstream URL + can mint an
   authenticated clone URL for it) calls a new git-factory internal endpoint
   `POST /internal/mirrors {upstream_url, namespace, name}` (shared-key auth like the existing
   `/internal/clone-token`).
2. git-factory creates a `Kind=mirror` `Repo` and runs `git clone --mirror <auth-upstream-url>`
   into `GIT_STORAGE_ROOT/<id[:2]>/<id>.git` (or `git fetch --prune` if it already exists).
   Credentials come from git-connector (never persisted on the repo row).
3. **Refresh policy:** (a) on connection; (b) on demand right before a CI run — a
   `POST /internal/mirrors/{id}/refresh?ref=<sha|branch>` that fetches upstream and only returns
   once `ref` is present (guarantees the CI clone sees the commit it wants); (c) optionally on an
   upstream webhook (Forgejo push → events → refresh). Start with (a)+(b); (b) is what makes it
   *correct* for CI, not just fast.
4. **Workflows clone from the mirror.** git-connector, when asked for a clone token for a repo it
   has mirrored, returns git-factory's URL (`{GIT_HTTP_BASE_URL}/{ns}/{name}.git` with a git-factory
   token) instead of the upstream — transparently, behind a per-connection option
   (`prefer_mirror`). Forge's `git:` secret_ref path is unchanged; only the URL git-connector hands
   back differs. **No forge change required.**

Why this is faster even single-node: git-factory is in-cluster (ClusterIP, warm object cache,
no host-network hop) and offloads clone load from the upstream; the mirror is a full local copy so
`upload-pack` is a local disk stream. Replicas (capability B) then fan this out.

---

## 8. Workflows / Forge integration

- No change to `forge/git-clone` or the `git:` secret_ref mechanics — the authenticated URL is
  still opaque to forge. The only change is **which host git-connector returns** (upstream vs
  git-factory mirror), gated by a per-repo `prefer_mirror` flag.
- CI correctness: the pipeline's `checkout.ref` (a SHA/branch) is passed to the mirror
  refresh-on-demand call so the clone is guaranteed fresh for that ref.
- Migration switch for the dogfood CI: once mirrors are proven, point
  `GIT_CLONE_URL: git:.../codearmory.git` at the mirror (or just flip `prefer_mirror` and leave the
  URL — git-connector rewrites it).

---

## 9. Deployment

- git-node as a **StatefulSet** (owns a PVC of bare repos); control plane stays a Deployment (N
  replicas, stateless).
- Helm/builder: `GIT_NODE_ADDRESS` per node; a bootstrap that seeds `GitNode` + `ShardPlacement`
  rows; `PROXY`/NetworkPolicy so control-plane pods can reach node pods.
- Single-node dev (minikube) keeps working with an empty placement table (everything local).

---

## 10. Phased delivery (each phase ships something usable)

| Phase | Delivers | Repos touched | Status |
|---|---|---|---|
| **0** | This design, agreed | — | ✅ |
| **1 — Mirror on one node** | *The CI clone-speed win.* `Kind=mirror` repos, `/internal/mirrors` + refresh-on-ref, git-connector `prefer_mirror` returns git-factory URL, dogfood CI clones from git-factory | git-factory, git-connector, (CI yaml) | ✅ |
| **2 — Remote git-node + proxy** | Control plane can serve a repo whose bytes are on another node (reverse proxy) | git-factory | ✅ on the WIRE path (`proxy.go`). `errRemoteNodeUnsupported` remains for the disk-touching API endpoints (commits, blobs, gc), which shared storage removes rather than the proxy |
| **3 — Replication + read fan-out** | primary/replica roles, version gate, async pack replication, read→replica / write→primary | git-factory | ✅ (`replicate.go`, `pickReadNode`) |
| **4 — Deploy at scale** | StatefulSet git-nodes, placement bootstrap, Helm/builder, CI cut over | infra, builder | ❌ — the code exists; nothing multi-node is deployed. **Fallback, not the target** (see §10): sharded shared storage was selected instead, keeping this document's routing table and retiring its replication |

**Where the work actually goes next — decided.** Phases 1–3 are built, so the remaining
question was never "build the read-replica machinery" but **which of two paths to deploy**.
It is now settled by measurement, and the answer is a **hybrid that keeps this document's
routing table and retires its replication**:

> **Sharded shared storage.** Per shard: one JuiceFS filesystem, its own Redis metadata
> engine (with Sentinel), one bucket or prefix, and stateless git_factory pods that can each
> serve any repo in that shard. The repo → shard routing table is **kept**. Replication,
> primary election and the `Applied >= Version` staleness gate are **retired — dormant, not
> deleted.**

The measurement is written up in `ARCHITECTURE.md` §5 ("Step 2 — the measurement, and the
architecture it selects") and reproducible from `infra/local/juicefs/`. In short: shared
storage is correct for git across pods, **free on the read path** — which is the majority of
git traffic — and costs a write latency floor of ~972 serialised metadata round-trips per
push (~67ms measured, ~290ms modelled cross-node, against sub-millisecond on local disk).
That floor is below the threshold a human notices on a push, and it cannot be cached away,
because the coherence that makes cross-pod locking correct forbids caching metadata.

**Why sharding rather than one big filesystem.** The two real objections to shared storage —
a ~134 pushes/sec ceiling and a blast radius covering every repo — are per-*filesystem*
properties, so one filesystem per shard fixes both, linearly. That also goes with JuiceFS's
grain: it pins all metadata for one filesystem to a single metadata instance by design, to
keep multi-key transactions off the wire. And it is nearly free here, because **the routing
table this document already built is the only thing sharding adds.**

**Why this retires Phases 2–3 rather than deploying them.** Within a shard there is one copy
of each repo, so two pods cannot diverge — there is nothing for replication to synchronise
and nothing for a version gate to protect against. The `errRemoteNodeUnsupported` gap this
document notes for disk-touching endpoints (commits, blobs, gc) closes for the same reason:
every pod in a shard can reach every repo's bytes directly, so the proxy is no longer load-
bearing for correctness. Cost is the other half of it — 3× replication couples capacity to
server count (50 TB of git needs ~43 NVMe boxes to hold 150 TB raw, against ~€312/mo of
object storage), an axis the first draft of this decision omitted entirely.

**Keep the code.** `replicate.go`, `pickReadNode` and the version gate stay in the tree,
unwired. They are the fallback if the write path ever becomes binding — CI-rate push
traffic, monorepos where repack dominates, or deployment on bundled NVMe below the cost
crossover, where 3× is effectively free and local disk simply wins. Phase 1's pull-through
mirror is unaffected and remains valuable on its own.

The original framing of the two paths, kept because it is still the clearest statement of
the trade — with the caveat that the answer turned out to be neither pole but the shard-wise
combination above:

- **Shared storage** (`ARCHITECTURE.md` §5 Step 2 — JuiceFS). One copy of each repo, git
  pods become stateless compute, and no routing table, primary election or version marker
  is needed *at all*. The service and chart sides are ready (storage preflight,
  `persistence.existingClaim`, RWX-aware replicas), and the cluster side — CSI driver,
  metadata engine, object store — is deployed and measured at local scale in
  `infra/local/juicefs/`. **Selected, with one amendment**: sharded, so the routing table
  *is* retained — the "at all" above holds only for a single filesystem. The metadata engine
  must be Redis (or TiKV), not CloudNativePG, which measured ~4x worse on this path.
- **Phase 4 here** — sharded local disk with StatefulSet git-nodes, which is
  `ARCHITECTURE.md`'s Step 4. **Fallback, not deployed.** It was gated on a measurement of
  the shared filesystem's write path; that measurement showed a floor that is real but not
  user-visible, and cheaper to absorb than 3× replication is to own and to buy.

The gate did its job: the choice rests on evidence rather than a guess, which is what §5
Step 4 asked for — and the evidence pointed between the two poles rather than at one of
them. What remains is deployment of the shard-wise shared-storage target: a shard key and
placement bootstrap, per-shard filesystem + Redis + Sentinel, Helm/builder wiring, and the
CI cutover. The read-replica machinery is not deleted, and not deployed.

---

## 11. Risks / open questions

- **Mirror freshness vs. cost.** Refresh-on-ref (7.3b) is the correctness lever; without it a mirror
  can serve a stale clone. Confirm CI always passes a concrete ref (it does today via `HOOK_REF`).
- **Auth for mirror fetch.** git-connector mints the upstream credential; git-factory must fetch
  with it and never persist it. Reuse the `/internal/clone-token` shape.
- **Private upstream → who may clone the mirror?** The mirror inherits git-factory's own RBAC
  (`readRepo` on `repos/{id}`), decoupled from upstream ACLs — must be set to match, or mirrors are
  restricted to CI's service identity only. **Decision needed.**
- **Replication consistency window.** The `Applied >= Version` gate trades a little read latency
  (fall back to primary) for never serving stale — acceptable for CI. Confirm.
- **Two-repo change.** Phase 1 spans git-factory + git-connector; they version independently. Land
  git-factory's `/internal/mirrors` first (additive), then git-connector's `prefer_mirror`.
