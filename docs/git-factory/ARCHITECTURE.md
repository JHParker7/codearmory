# codearmory_git_factory — Architecture & Design

The git plane of the codearmory platform. The platform already has CI/CD, runners,
and tickets; this service owns **repositories and the git wire protocol**. It began
as a `codearmory_bootstrap`-generated service and now lives in the monorepo as a
**core** service (`src/systems/git-factory`, built by the monorepo CI, deployed by
the Helm chart, registered through the registry manifest). Either way it inherits
the platform spine: gatekeeper RBAC (forward-auth), Postgres via GORM,
OpenTelemetry, service-key rotation. This doc describes how to grow that scaffold
into a *scalable* git server without throwing any of the spine away.

Its **service identity is `codearmory_git_factory`** even though the directory and
Go module are `git-factory` — that identity is the first segment of every RBAC
resource and the key every grant is written against, so it must never be renamed.

Scope of v1: **Smart HTTP only** (clone/fetch/push over HTTPS). SSH is a future
add-on (see [SSH, later](#ssh-later)).

---

## 1. The one decision that matters: control plane vs git plane

Git repositories are **stateful** — bare repos are directories of files on a
disk, mutated by `git-receive-pack`. Everything else the platform does is
stateless request/response. Conflating the two is exactly what makes a monolith
(Gitea/Forgejo) hard to scale and hard to integrate: the process that answers
`GET /api/repos` is the same process that owns the filesystem, so you can't scale
one without the other, and you can't put your own auth in front without fighting
the monolith's.

So the single architectural rule this service follows — the same rule GitLab
converged on with **Gitaly** after starting on NFS:

> **Application code never touches git-on-disk directly. All git access goes
> through a narrow, repo-scoped interface.**

In v1 that "interface" is a function boundary inside this one process
(`gitplane.UploadPack(repoID, …)`). The moment you need more than one storage
node, that same boundary becomes a call to a remote `git-node` — and *nothing in
the control plane changes*. Design to the boundary now; defer the network.

What *crosses* that boundary is a transport decision deferred to Step 3 (§5), not
a property of the boundary itself: git wire traffic is reverse-proxied HTTP, and
only non-streaming control operations use an RPC/REST API. Keeping the Go
interface intact is what keeps that choice cheap to revisit.

```
                       ┌─────────────────────────────────────────────┐
   CI/CD · runners  ─► │  codearmory_git_factory — CONTROL PLANE     │
   tickets · UI        │  (stateless, scales horizontally)           │
   git clients ──────► │                                             │
                       │  • gatekeeper auth (inherited)              │
                       │  • Repo metadata + routing  → Postgres      │
                       │  • REST management API (/repos)             │
                       │  • Smart-HTTP entry (/{ns}/{repo}.git/…)    │
                       │                                             │
                       │        gitplane boundary  ┐                 │
                       └───────────────────────────┼─────────────────┘
                                                   │ v1: in-process call
                                                   │ v2+: HTTP proxy to a git-node
                       ┌───────────────────────────▼─────────────────┐
                       │  GIT PLANE  (stateless pods, shared storage)│
                       │  • bare repos on a shared JuiceFS mount     │
                       │  • runs git-upload-pack / git-receive-pack  │
                       │  • scales horizontally — any pod, any repo  │
                       └─────────────────────────────────────────────┘
```

Why this beats the Forgejo integration you hit: the control plane speaks the
platform's *native* language (gatekeeper resources, OTel traces, service keys),
and the stateful part is quarantined behind one boundary you fully own.

---

## 2. Surfaces

Two HTTP surfaces on the same service, with **different auth mechanics** — this
distinction is the subtlest part of the design, so it's called out explicitly.

### 2a. Management API — JSON, Bearer auth

Ordinary REST, identical in shape to the `widgets` scaffold. Auth is the standard
`gatekeeperClient.CheckPermissions(ctx, w, r, action, resource)` call, which
forwards the request's `Authorization: Bearer <jwt>` to gatekeeper.

| Method | Path | Action | Resource | Success |
|---|---|---|---|---|
| `POST`   | `/repos`      | `createRepo` | `codearmory_git_factory/repos`      | `201` |
| `GET`    | `/repos`      | `listRepo`   | `codearmory_git_factory/repos`      | `200` (array) |
| `GET`    | `/repos/{id}` | `getRepo`    | `codearmory_git_factory/repos/{id}` | `200` |
| `PATCH`  | `/repos/{id}` | `updateRepo` | `codearmory_git_factory/repos/{id}` | `200` |
| `DELETE` | `/repos/{id}` | `deleteRepo` | `codearmory_git_factory/repos/{id}` | `204` |

`createRepo` does two things atomically-ish: insert the `Repo` row, then
`git init --bare` the on-disk repo. If the init fails, roll back the row (or mark
it `provisioning` and reconcile) — never leave a metadata row without a repo or
vice versa. Delete is the reverse: remove row, then `rm -rf` the dir.

### 2b. Git wire API — pkt-line, credential-in-request auth

The Smart-HTTP v1/v2 protocol. Clients are `git` itself, so we don't get to
choose the shape — it's fixed by the protocol:

| Method | Path | Meaning | Action |
|---|---|---|---|
| `GET`  | `/{ns}/{repo}.git/info/refs?service=git-upload-pack`  | fetch/clone advertise | `readRepo` |
| `POST` | `/{ns}/{repo}.git/git-upload-pack`                     | fetch/clone           | `readRepo` |
| `GET`  | `/{ns}/{repo}.git/info/refs?service=git-receive-pack` | push advertise        | `writeRepo` |
| `POST` | `/{ns}/{repo}.git/git-receive-pack`                    | push                  | `writeRepo` |

Notes that trip people up:
- **`.git` suffix is optional.** `git` clones `/{ns}/{repo}` and `/{ns}/{repo}.git`
  interchangeably; accept both (strip a trailing `.git` before lookup).
- **Content types are exact and mandatory.** The advertisement responds
  `Content-Type: application/x-git-upload-pack-advertisement` (resp. `-receive-pack-`),
  and the body's first pkt-line is `# service=git-upload-pack\n` followed by a
  flush-pkt `0000`. Get these wrong and `git` fails with an opaque error.
- **Auth is not Bearer-by-default.** `git` sends credentials as HTTP **Basic**
  (`username:password`), where real users put a Personal Access Token in the
  password field. The service must therefore accept **either**:
  - `Authorization: Basic base64(anyuser:<token>)` — the token is the password, or
  - `Authorization: Bearer <token>` — for programmatic clients that set
    `-c http.extraHeader`.

  In both cases: extract the token → resolve it to an identity → call
  `CheckPermissions(readRepo|writeRepo, repos/{id})`. In v1, wrap the incoming
  request so its `Authorization` header is normalized to `Bearer <token>` before
  handing it to the existing gatekeeper SDK, so the *authorization* logic is
  shared with the management API and only the *credential extraction* differs.
- **Challenge correctly.** Unauthenticated access to a private repo returns `401`
  **with `WWW-Authenticate: Basic realm="git"`**, or `git` won't prompt/retry
  with credentials.

---

## 3. Auth & ownership model (unchanged from the scaffold's philosophy)

Two layers, exactly like `widgets`:

1. **gatekeeper gates coarse API access.** Grants are templated
   `{username}/codearmory_git_factory/repos[...]`; gatekeeper auto-scopes the
   checked resource by the caller's username. Passing the check means "this user
   may perform repo reads/writes," not "this user owns *this* repo."
2. **Per-record ownership is enforced here, in the DB.** Every query filters on
   `owner` (the gatekeeper `user_id`). Reading someone else's private repo passes
   gatekeeper but the ownership-filtered lookup returns not-found → `403/404`.

So: `readRepo`/`writeRepo` on `repos/{id}` gets you past gatekeeper; the git
handler then loads the repo by `{ns}/{repo}`, and if `repo.owner != caller`
(and the repo isn't public), it's `404`. This is the same trust split the README
documents for widgets, extended to git.

> **Visibility (future):** add `Repo.Visibility ∈ {private, public}`. A `public`
> repo skips the ownership check on `readRepo` only (anonymous clone), never on
> `writeRepo`. v1 is private-only.

---

## 4. Data model

`src/types.go` already renamed `Widget → Repo` with a `(owner, name)` unique
index — good. Two additions the git plane needs:

```go
type Repo struct {
    ID            string    `gorm:"primaryKey" json:"id"`            // uuid, stable, used in RBAC resource
    Owner         string    `gorm:"uniqueIndex:ux_owner_name;index" json:"-"`        // gatekeeper user_id — ownership filter
    Namespace     string    `gorm:"index" json:"namespace"`         // username used in the clone URL path
    Name          string    `gorm:"uniqueIndex:ux_owner_name" json:"name"`
    Description   string    `json:"description"`
    DefaultBranch string    `gorm:"default:main" json:"default_branch"`
    // Shard        string `json:"-"`  // v2+: which git-node holds the bytes
    CreatedAt     time.Time `json:"created_at"`
    UpdatedAt     time.Time `json:"updated_at"`
}
```

Design notes:
- **`Owner` (user_id) vs `Namespace` (username).** `Owner` is the stable id used
  for the ownership filter and is *not* serialized. `Namespace` is the
  human-readable segment in `/{namespace}/{name}.git`. `createRepo` gets both
  from the token: `userID, username, _ := CheckPermissions(...)`. Storing the
  username denormalizes it for URL routing; a username-rename story (rewrite
  `Namespace`, keep `Owner`) is a later concern — note it, don't build it.
- **`http_url` is derived, not stored.** Return it in the JSON so the UI/tests can
  build clone commands: `{base}/{namespace}/{name}.git`, where `{base}` comes
  from a `GIT_HTTP_BASE_URL` env (or the request `Host`). Keep it out of the
  table so a config change doesn't require a migration.
- **On-disk path is derived from the stable id, not the name:**
  `${GIT_STORAGE_ROOT}/{id[:3]}/{id}.git`, where `GIT_STORAGE_ROOT` is the shared
  JuiceFS mount (§5-Step2). Deriving from `id` (not `namespace/name`) buys two
  things: a repo rename is metadata-only — no bytes move — and **no user-supplied
  string is ever joined into a filesystem path**, which is the structural half of
  the traversal defence in §6. The 3-char fan-out dir keeps any single directory
  from holding millions of entries. The id is validated as a *canonical* uuid
  before the path is built (`repoDiskPath` in git.go), so a crafted id from the
  URL cannot reach the filesystem and the fan-out slice cannot go out of range.

---

## 5. Scaling roadmap — build in this order

Each step ships something usable; stop whenever "big enough."

**Step 1 — Single node, in-process git (v1, what the tests target).**
Control plane shells out to `git`/`git-upload-pack`/`git-receive-pack` against
`GIT_STORAGE_ROOT` on local disk, gated by gatekeeper. One node, no sharding.
This is ~90% of the value for ~10% of the complexity, and it makes
`git clone`/`push` work end-to-end against your auth. The integration tests
(§7) exercise exactly this.

**Step 2 — Shared storage (JuiceFS).** Point `GIT_STORAGE_ROOT` at a **JuiceFS**
mount instead of a local directory. JuiceFS splits the two halves of a filesystem:
**metadata** into a transactional engine (CloudNativePG — its own database on the
platform cluster, with a connection cap so a `git gc` storm can't starve other
services) and **data blocks** into S3-compatible object storage (MinIO, or the
provider's), with a local NVMe **read** cache in front.

No application code changes — it is still a path on a filesystem. What changes is
who can reach it: every git process now sees the same bytes.

*Why this and not local disk plus replication.* Durability on local disk requires
**building** replication: primary election, replica sync, failover, staleness
markers. Delegating durability to the object store makes it configuration instead
of code. That one decision is what collapses Step 3 and defers Step 4 entirely.

*Status — the service and chart side are ready; the cluster side is not deployed.*
Because there are no application code changes, "ready" means the service can be
pointed at a shared mount safely rather than hopefully:

- **Storage preflight** (`storagecheck.go`) verifies the three non-negotiable
  properties below against `GIT_STORAGE_ROOT` on every startup, before anything
  can accept a push, and refuses to start by default when one is missing. This is
  the check the table's "unsafe — rules itself out" row needs in order to bite:
  nothing else in the service would notice that a path stopped being a filesystem.
  It verifies them **locally** — the cross-client half still needs two pods
  against one mount and belongs in the deployment checklist.
- **Chart** takes `persistence.existingClaim` (a JuiceFS PVC provisioned outside
  the release, since the CSI driver, metadata engine and object store outlive any
  one release), and `accessMode: ReadWriteMany` unpins `replicaCount` and switches
  the rollout from `Recreate` to surge-first. Two combinations are refused at
  render time: replicas > 1 without RWX, and replicas > 1 with the ref-lock
  janitor disabled.
- **`persistence.fsGroup: false`** exists for this path. The ownership pass is a
  recursive `chown` over the whole store, and on a metadata-engine filesystem every
  operation in it is a transaction — on a large store that turns pod startup into a
  long outage, repeated by every replica.

Still to do, and deliberately not written blind: deploying the JuiceFS CSI driver,
the CloudNativePG metadata cluster (with its connection cap) and the object-storage
backend. Those are cluster-specific and cannot be validated by rendering a template.

**Step 3 — Scale the git plane horizontally.** Because storage is shared, git
server pods are **stateless compute** — any pod can serve any repo. Run N of them
behind an ordinary Kubernetes Service with ordinary autoscaling, and have the
control plane **reverse-proxy** to that Service rather than to a particular node.

No routing table, no shard map, no primary election. One copy of each repo means
nothing can diverge, so git's own on-disk locking (`O_CREAT|O_EXCL` lock files
plus atomic `rename`) serializes concurrent writers, and two pushes racing the
same branch resolve exactly as they would on one machine: one wins, the other is
told to retry.

*Why a proxy and not gRPC.* `git-upload-pack`/`git-receive-pack` are long-lived,
chunked, binary byte streams, and Smart HTTP is **already HTTP** — so forwarding
is a pure pass-through. Wrapping those streams in gRPC means re-framing a byte
stream into protobuf messages carrying `bytes` chunks: reimplementing HTTP
streaming, with extra copies, and losing `curl` as a debugging tool. GitLab hit
exactly this with Gitaly and had to add a "sidechannel" to bypass gRPC for pack
transfer. `httputil.ReverseProxy` gives streaming, chunked encoding and flush
control for free. Note that *where* a repo lives is a **lookup** problem, never a
transport problem — under Step 2 the lookup is trivial (one storage namespace),
and under Step 4 it becomes a routing table. Neither argues for gRPC.

*Where RPC still earns its keep.* Anything that is **not** a byte stream: node
lifecycle (create/delete/move a repo, rebalance shards, Step 4 replication) and
semantic queries a UI would want (list branches, read a blob at a ref, diff).
Those are structured request/response — give them a small RPC or REST API on the
node. So: proxy the bytes, RPC the control plane. This is Gitaly in miniature,
minus the part Gitaly regrets.

*If SSH is ever added* (§SSH, later), it has no inbound HTTP request to forward —
`sshd` hands you a raw stdin/stdout pipe. At that point, either have the SSH front
door call the node's HTTP endpoint, or introduce a protocol-agnostic
`UploadPack`/`ReceivePack` RPC. That choice is deferred until SSH actually exists;
HTTP is the primary and recommended integration path (it traverses corporate
proxies, reuses gatekeeper's token auth, and needs no key management in CI).

**Step 4 — (contingent) Sharded local disk.** Only if the shared-filesystem write
path proves too slow. Git is metadata-heavy and `fsync`-heavy, and on JuiceFS a
durable flush is a round-trip to object storage — **this is the one risk the
design carries**, and it is a measurement, not a guess (§7).

If it fails the bar, fall back to local NVMe per shard — and only then do you need
everything Step 2 avoided: a `repo → shard → node` routing table (`Repo.Shard`
already exists for it), writes pinned to a primary, replicas for HA and read
fan-out, and a per-repo version marker so a read immediately after a push isn't
served stale. That is the DGit / Gitaly-Cluster (Praefect) shape. Keep the §1
boundary intact and this stays reachable without touching the control plane.

### Storage alternatives considered

| Option | Verdict |
|---|---|
| **CephFS** | Correct POSIX semantics — real `flock` and atomic rename via the MDS — but synchronous inter-OSD replication puts node-to-node latency on every write, and it assumes a fast, low-jitter interconnect. Rough on commodity cloud networking, with high resource overhead at small node counts. |
| **Local NVMe + async S3 backup** | Fast, but a node or disk loss between an acknowledged push and the next backup loses that push. Backup is DR, not availability; availability needs replication — the work Step 2 exists to avoid. |
| **Object storage via FUSE** (s3fs, gcsfuse, Mountpoint, rclone) | **Unsafe — rules itself out.** No atomic rename and no real locking, which is git's entire safety model. Corrupts repositories rather than merely being slow. |
| **NFS / EFS / Filestore / Azure Files** | Workable on NFSv4.x. NFSv3 locking (NLM) is the historic weak spot and SMB is wrong for git. Higher per-op latency than the metadata/data split, with no compensating benefit. |
| **Redis as the metadata engine** | Faster per op, but the speed comes partly from *weaker durability*: an `appendfsync everysec` window can orphan a just-written object and corrupt a repo, while `always` gives the speed back. No PITR, and the dataset must fit in RAM — and metadata only grows. |
| **Redis Cluster** | Scales capacity and aggregate throughput, not per-operation latency — which was the only reason to want it. Adds slot routing and a less-travelled path for multi-key transactions. |

Whatever backs the filesystem, three properties are non-negotiable and must be
**verified on the real deployment**, not assumed from documentation:
`O_CREAT|O_EXCL` is atomic across clients (git's ref-lock primitive), `rename()`
is atomic across clients (how every ref update commits), and `flock`/`fcntl`
work across clients.

The startup preflight (`storagecheck.go`, `GIT_STORAGE_PREFLIGHT`) checks all
three, and refuses to start by default when one fails — the asymmetry is that a
false negative is one loud error naming the property and the override, while
continuing on a filesystem that really lacks them corrupts repositories silently,
and for pushed source this store is the only copy. It runs in one pod, so it
proves the properties **locally**: enough to catch a backend that fails them
outright (s3fs does not become atomic just because there is one writer), not
enough to certify multi-writer use. **Confirming the across-clients half is a
deployment step**: run two pods against the one mount and re-check, before
raising `replicaCount`.

Orthogonal, add whenever the pain shows up:
- **Git LFS** → offload large blobs to S3-style object storage so pack ops stay
  cheap. Cleanly separable; the pointer files live in git, the bytes don't.
- **Caching** → ref-advertisement cache + pack cache; CDN for anonymous clones of
  public repos (read-only, highly cacheable).
- **Read/write path split** → already latent in the protocol
  (`upload-pack` = read, `receive-pack` = write); lean on it at every step.

---

## 6. Failure modes to design against (v1)

- **Partial create/delete.** Row exists, dir doesn't (or vice versa). Pick an
  order and a reconciler; never trust that both succeeded.
- **Concurrent push to the same repo.** `git-receive-pack` handles ref-lock
  contention itself, and with one shared copy (§5-Step2) that holds across pods
  too — the loser gets "failed to lock ref", which is the correct answer to send
  the client. Only if you fall back to replication (Step 4) must writes be
  serialized per repo.
- **Stale ref locks.** `refs/heads/main.lock` is an ordinary file, so if a git
  process dies mid-push it persists and **no other pod can tell whether the holder
  is alive** — git has no lease on it. Shared storage doesn't fix this; it widens
  it from one host to all of them. Needs a janitor that clears lock files past a
  threshold.
  **Built** (`reflock.go`), running inside the maintenance sweep and needing no
  coordination between pods — the loser of a race gets `ENOENT` from the unlink.
  Threshold is `GIT_REF_LOCK_MAX_AGE`, default 1h, and `0` disables rather than
  meaning "everything is stale". It errs long deliberately: clearing too late
  leaves a ref unpushable and says so, while clearing too early is silent —
  unlinking a live lock doesn't fail the holder, whose `rename()` lands anyway,
  so two pushers each believe they hold the ref and one update is lost.
- **The metadata engine is absolute.** Blocks in object storage are unreadable
  without the JuiceFS metadata that maps files to them. Losing it loses
  everything, intact bytes notwithstanding — which is why it is Postgres with
  PITR rather than a cache-shaped store, and why its backups matter more than the
  object store's.
- **Never enable JuiceFS `--writeback`.** It buffers writes locally and uploads
  asynchronously, reintroducing precisely the acknowledged-write loss window that
  ruled out local disk in §5. Read caching is safe; write-back is not.
- **Path traversal.** `{ns}` / `{repo}` come from the URL. Reject anything that
  isn't `[A-Za-z0-9._-]+` *before* touching the filesystem, and never build the
  on-disk path from user strings (build it from `repo.id`, per §4).
- **Large/slow clones.** `git-upload-pack` on a big repo streams for a long time.
  The scaffold's `WriteTimeout: 60s` (main.go) **will kill real clones** — the
  git wire routes need their own, much longer (or zero) write timeout. Flag this
  when wiring the mux.
- **Request body limits.** `limitBody` caps bodies at 1 MiB (`maxBodyBytes`).
  A push POST is the whole packfile — potentially gigabytes. The git wire routes
  must bypass `limitBody`, or pushes fail instantly.

> These last two (timeout + body cap) are the two scaffold defaults most likely
> to bite you first — they're correct for JSON CRUD and wrong for git.

## SSH, later

Add an SSH endpoint (`gliderlabs/ssh` or a real `sshd` + `AuthorizedKeysCommand`)
that authenticates by public key → resolves to a user → runs the *same*
`gitplane` `UploadPack`/`ReceivePack` boundary as Smart-HTTP. Because auth is the
only thing that differs, SSH becomes a second front door onto an unchanged git
plane — which is the whole point of the §1 boundary. Store user public keys as a
new resource on this or the gatekeeper service.

---

## 7. What the integration tests pin down

`tests/test_repos.py` — the management API (mirrors `test_widgets.py`): auth
(`401`), validation (`400`), create→`201`, duplicate name→`409`, ownership
isolation (a second user can't see/get/delete your repo), delete→`204`→`404`.

`tests/test_git_http.py` — the git wire protocol:
- unauthenticated `info/refs` on a private repo → `401` + `WWW-Authenticate`;
- authenticated `info/refs` → `200`, exact advertisement content-type, and a body
  whose first pkt-line is `# service=git-upload-pack`;
- a **full round-trip with the real `git` CLI**: init → push → fresh clone →
  assert the file is present;
- a second user cannot clone your private repo.

These fail until the code exists — that's the point; they're the executable
spec to build Step 1 against. See each file's module docstring for the exact
contract.
