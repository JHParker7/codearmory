# JuiceFS shared storage for git_factory (local minikube)

This is `ARCHITECTURE.md` §5 **Step 2 — shared storage**, actually deployed. The doc
listed the cluster side as *"still to do, and deliberately not written blind: deploying
the JuiceFS CSI driver, the CloudNativePG metadata cluster and the object-storage
backend"*. These manifests are that, at minikube scale, plus the cross-pod checklist the
in-process preflight explicitly cannot cover.

> **Status: this is the selected architecture, at one-shard scale.** Step 2 was gated on a
> measurement of the shared write path. That measurement was taken here, and it selected
> **sharded shared storage** — one JuiceFS filesystem plus its own Redis per shard, the
> repo → shard routing table kept, and replication / primary election / staleness gate
> retired. See `ARCHITECTURE.md` §5 "Step 2 — the measurement, and the architecture it
> selects" and `DESIGN-read-replicas.md` §10.
>
> What this directory deploys is **one shard**. A production install runs several, each with
> its own filesystem, its own Redis, and its own bucket or prefix. The engine here is now
> **Redis with Sentinel** (`10-meta-redis.yaml`), not the CloudNativePG originally
> specified: Postgres measured ~4x worse on git's write path, and is kept as
> `alt-meta-postgres.yaml` only so that comparison can be re-run.
>
> Kept so the numbers can be reproduced and re-argued rather than taken on trust.

```bash
./setup.sh     # bring it up (idempotent), migrating any existing repo store
./verify.sh    # the cross-pod checks — run this before trusting >1 replica
```

## Why this changes anything

git_factory is the one core service holding user data on a filesystem rather than in
Postgres, and with a ReadWriteOnce volume that pins it to **one replica**: a second pod
either cannot schedule or becomes a second writer to a store with no cross-node locking.

JuiceFS splits the filesystem in two, and the split is the whole point:

| half | where it goes | why |
| --- | --- | --- |
| **metadata** — inodes, dentries, locks, renames | a transactional engine (Redis + Sentinel) | this is what makes `rename` atomic and `flock` real, which is git's entire safety model |
| **data** — 4MiB chunks | object storage (MinIO here; the provider's S3 in production) | durability becomes configuration instead of replication code |

Mounting a bucket directly (s3fs, gcsfuse, Mountpoint, rclone) gives you the second half
without the first, which is why `storagecheck.go` refuses to start on one: it does not
fail your pushes, it corrupts repositories.

There is **no application change** — `GIT_STORAGE_ROOT` is still a path. What changes is
who can reach it.

## What gets deployed

| file | what |
| --- | --- |
| `00-namespace.yaml` | the `juicefs` namespace |
| `10-meta-redis.yaml` | metadata engine — Redis (primary + replica) and three Sentinels |
| `alt-meta-postgres.yaml` | the Postgres engine it replaced. **Not applied** — kept to re-run the comparison |
| `20-minio.yaml` | data backend — MinIO plus the bucket, created as a directory by an init container |
| `csi-driver-values.yaml` | Helm values for the JuiceFS CSI driver |
| `30-juicefs-fs.yaml` | the filesystem secret, the StorageClass, and the **ReadWriteMany** claim |
| `40-migrate-job.yaml` | one-shot copy of an existing repo store onto the new volume |
| `50-git-factory.yaml` | git_factory on the shared claim, **2 replicas** |

In production the equivalent is chart values, not manifests — the claim is provisioned
outside the release and named to the chart:

```yaml
gitFactory:
  replicaCount: 2
  persistence:
    existingClaim: git-factory-repos
    accessMode: ReadWriteMany
    fsGroup: false          # the ownership pass is a RECURSIVE chown; on a
                            # metadata-engine filesystem every chown is a transaction
  env:
    refLockMaxAge: 1h       # required once there is more than one replica
```

## minikube-specific accommodations

Not JuiceFS advice — local constraints, called out so they are not copied into a real
cluster:

- **Nothing is pulled in-cluster.** The node has IP connectivity but cannot resolve
  registry hosts, so `setup.sh` pulls on the host and `minikube image load`s, and every
  image is pinned to `IfNotPresent`/`Never`. An unpinned image is the failure mode where
  a mount pod sits in `ImagePullBackOff` while git-factory hangs in `ContainerCreating`.
- **One Redis replica and three Sentinels on one node.** The shape is production's; the
  redundancy is not, since a node loss still takes all of it. Sentinel is deployed anyway
  because the metaurl format is the part that gets miswritten, and it is worth exercising
  for real rather than discovering in production.
- **No Redis authentication.** A real deployment sets `requirepass`, puts that password in
  the metaurl, and *additionally* sets `SENTINEL_PASSWORD` in the mount pod's environment —
  they are separate credentials, and the URL's password does not authenticate to Sentinel.
- **Single node**, so the migration Job can mount the old ReadWriteOnce claim and the new
  one at once. On a multi-node cluster, pin it to the node holding the old volume.
- **fsGroup replaced by an init container** that `chown`s exactly two directories rather
  than recursing the store.

## Verifying it

`storagecheck.go` runs on every startup and is honest about its limit: it proves
`O_CREAT|O_EXCL`, atomic `rename` and `flock` **locally**, in one process on one node,
and says cross-client behaviour "belongs in the deployment checklist, not here".
`verify.sh` is that checklist:

0. both replicas passed the preflight and have **not restarted**
1. the two pods see one filesystem, and a write in one is visible in the other
2. `O_CREAT|O_EXCL` refuses the second pod while the file exists — git's ref-lock primitive
3. `flock` is *enforced* across pods, not merely accepted (with an uncontended control)
4. a **real concurrent push** to one branch from both pods: exactly one wins, both pods
   then read the same ref, and `fsck` reports no damage
5. every repo in the store is `fsck` clean

Check 3's negative result is the one worth understanding: a FUSE layer that accepts every
lock request and enforces nothing passes a naive test and loses ref updates in production.

### Results on this cluster

All checks pass. The loser of the push race is rejected with
`! [remote rejected] main -> main (incorrect old value provided)` — git's own
compare-and-swap on the ref, exactly the outcome two pushes racing on a single machine
get. Storage after migrating 15 repos: ~14 MB of chunks in MinIO, ~700 inodes in the
metadata engine.

## The measurement (what selected this architecture)

Numbers behind the decision recorded in `ARCHITECTURE.md` §5. Method matters here: the
client must be on **local disk** and only the bare target on the shared volume, or you
measure the client writing a working tree onto FUSE rather than the git server's own work.
An earlier pass got this wrong and made shared storage look ~10x worse than it is.

**Server-side, 30 incremental pushes** (client local, bare repo on the volume):

| target | 30 pushes | per push |
|---|---|---|
| local disk | ~0s | <33ms |
| JuiceFS + **Redis** metadata | 2s | ~67ms |
| JuiceFS + **Postgres** metadata | 13s | ~430ms |

Reads are free: `git bundle create` and a clone to a local destination run at local-disk
speed on JuiceFS.

**Why the write path has a floor.** Counting metadata operations against the engine:

| git operation | metadata ops |
|---|---|
| one small push | ~972 |
| one clone | ~575 |
| one bare-repo create | ~814 |

They are effectively serialised, so latency ≈ ops × RTT: 972 × 0.071ms = **69ms predicted
vs 67ms measured**. That model extrapolates, which is the point — this cluster has the
metadata engine on the same node, so 0.071ms is a best case. At a realistic cross-node
0.3ms it is ~290ms per push, and these are trivial one-file pushes on a 200-file repo.

**Scope of one metadata engine.** One Redis serves one *filesystem* — not one per pod, not
one per repo. Here: 4 app pods → 2 mount pods (one per node per volume) → 1 Redis holding
36,815 keys (inodes, chunk maps, dentries) and the POSIX lock table that makes cross-pod
`flock` work. At ~130k ops/s that is a global ceiling near **134 pushes/sec**. Per-shard
engines raise the ceiling linearly but leave the round-trip count untouched.

**Availability, which is the genuine win:** forced pod kill dropped **0** requests; rolling
deploy was clean, against **7s** for the ReadWriteOnce `Recreate` rollout. Node loss is
worse for RWO than either: the default `tolerationSeconds: 300` plus block-volume
detach/reattach puts it at **6–10 minutes**.

Caveats: one node, so no cross-node mount and no network RTT in the raw figures; small
repos only; no large-monorepo or concurrent-push load.

**Operational notes if you use this anyway.** `TrashDays` defaults to 1, and `git
gc`/repack deletes packfiles constantly, so object-storage usage and metadata counts run
persistently above the live repo size; consider `--trash-days 0` for a git store.
`startMaintenance` no longer sweeps from every replica — it takes a lease first (lease.go),
so one pod sweeps per interval however many are running.

## The metadata engine, and what was checked

Redis, with a replica and three Sentinels (`10-meta-redis.yaml`). Verified on this cluster
rather than assumed:

- **Sentinel discovery works through the CSI driver.** The mount pod logs
  `discovered new sentinel` twice and then `new master=... addr=juicefs-meta-1...`, with a
  ping latency of ~21µs. Note it found the *promoted* instance, not the one Sentinel was
  originally configured to monitor.
- **Failover.** Deleting the primary promoted the replica in ~12s (`down-after` 5s +
  `failover-timeout` 10s), a canary key written before the kill was present afterwards, and
  the restarted ex-primary rejoined **as a replica** — it asks Sentinel who the master is
  rather than assuming its ordinal, which is what stops a restart from becoming a second
  primary.
- **`maxmemory-policy noeviction` on both instances**, which is the one setting that must
  not be wrong. Eviction does not shed load here, it deletes inodes and chunk maps — the
  blocks stay perfectly intact in the bucket and become unreachable. JuiceFS tries to set
  this itself and only *warns* if it cannot, so a misconfigured instance looks healthy right
  until it starts destroying the filesystem. The corollary is worth planning for: at
  `maxmemory` with `noeviction`, Redis rejects every write **including deletes**, so a full
  metadata engine cannot be freed by deleting files. Monitor with headroom.
- **AOF + RDB together**, per JuiceFS's own recommendation — the AOF is the more complete
  record on recovery, the RDB is the compact thing to copy off-box.

**The engine and the bucket are a matched pair.** Pointing `metaurl` at a fresh engine while
keeping the old bucket does not migrate anything; it asks for a new filesystem, and JuiceFS
refuses with `Storage minio://... is not empty; please clean it up or pick another volume
name`. That refusal is correct — those blocks belong to a filesystem whose metadata knew
what they were. Swapping an engine means a clean prefix (or an emptied one) *and* re-running
the migration.

### A bug this found

Bringing two replicas up against one mount crash-looped both of them, on a filesystem
that was completely correct. The preflight wrote its probes under **fixed** names
(`excl.probe`, `rename.probe`, `flock.probe`) in the storage root — which on shared
storage is the same directory every other replica probes. Started together, they read
each other's files: the rename source vanished under the other pod's cleanup, the
destination read back as half-written bytes, and `flock` was *refused* because the other
pod held it — the property working, reported as broken. In the default `enforce` mode
that is a refusal to start.

Fixed in `storagecheck.go` by scoping probe names to the pod (hostname+pid), with an
age-gated sweep for probes abandoned by pods that no longer exist. `verify.sh` check 0
watches for the regression, since its signature is restart count rather than a log line.

## Teardown

```bash
kubectl delete -f 50-git-factory.yaml            # or re-apply the RWO deployment
kubectl delete -f 30-juicefs-fs.yaml             # PV is Retain: repos survive
helm uninstall juicefs-csi-driver -n juicefs
kubectl delete namespace juicefs
```

The StorageClass is `reclaimPolicy: Retain` on purpose — for pushed source this store is
the only copy.
