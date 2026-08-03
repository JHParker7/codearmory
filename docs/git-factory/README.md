# git_factory — the platform's own git host

git_factory **is the git plane**: it stores bare repositories and serves them over
Smart HTTP, so codearmory can host the repos it builds rather than only brokering
access to someone else's. It is a core, in-repo service — built by the monorepo CI,
deployed by the Helm chart, and registered through the registry manifest.

**Mind the naming.** The directory and Go module are `git-factory`, but the service
**identity is `codearmory_git_factory`** — the name it registers under, the first
segment of every RBAC resource (`codearmory_git_factory/repos`), and the key every
grant is written against. Never rename the identity: doing so invalidates existing
grants.

It is **not** the same thing as **git_connector** (directory `src/systems/git`), which
is the backend-agnostic credential broker that mints short-lived clone credentials for
GitHub, GitLab, Forgejo or generic backends. git_factory is one of git_connector's
backends.

## Documents

| Doc | What it covers |
|---|---|
| `ARCHITECTURE.md` | the design and the §5 scaling roadmap — read this first |
| `DESIGN-read-replicas.md` | rationale for the pull-through mirror and read fan-out (both built) |
| `RUNBOOK-phase4.md` | operating a multi-node deployment |

## Layout

Code lives in `src/systems/git-factory/` (module `git-factory`).

| File | Role |
|---|---|
| `main.go` | entrypoint: slog + telemetry, route table, graceful shutdown, `secret()`/`envOrDefault()` |
| `api_repo.go` | repo CRUD, branches, commits, tree/blob, archive, readme |
| `api_git_http.go` | the Smart HTTP wire: `/{ns}/{repo}/info/refs` and the pack endpoints |
| `gitplane.go` | shells out to `git-upload-pack`/`git-receive-pack`; `localDirFor()` resolves a repo's directory |
| `git.go` | on-disk layout — `repoDiskPath()` derives the path from the repo's **stable id**, never its name, so a rename stays metadata-only |
| `proxy.go` | reverse-proxies a wire request to the node holding the bytes |
| `shard.go` / `replicate.go` | shard placement, replica versions, read fan-out |
| `mirror.go` / `api_mirror.go` | pull-through mirrors of upstream repos |
| `maintain.go` | quota accounting and the periodic `git gc` sweep |
| `reflock.go` | the stale ref-lock janitor — clears `.lock` files whose holder is gone |
| `storagecheck.go` | startup preflight: verifies the filesystem properties git's correctness depends on |
| `collab.go` / `protect.go` / `visibility.go` | collaborators, branch protection, public/private reads |
| `clonetoken.go` | short-lived HMAC clone tokens for CI runners |
| `ui.go` | the embedded mini-portal at `/ui` |

## Develop

```bash
cd src/systems/git-factory
go build ./...
go vet ./...
go test .

# Module mode, which is what the Docker build uses — a plain `go build` will not
# catch a go.sum that only works in workspace mode.
GOWORK=off go build ./...
```

## Configuration (env)

All read via `secret()`, which prefers `${VAR}_FILE` (mounted secrets) over the plain
env var.

| Var | Default | Notes |
|---|---|---|
| `PORT` | `9002` | listen port |
| `DATABASE_URL` | `…/git_factory` | Postgres DSN (repo metadata; the bytes are on disk) |
| `DATABASE_READ_URL` | — | optional read replica |
| `GATEKEEPER_URL` | `http://localhost:8080` | gatekeeper base URL |
| `GATEKEEPER_SERVICE_KEY` | — | initial service key; must match this service's entry in gatekeeper's `GATEKEEPER_SERVICES` |
| `GIT_STORAGE_ROOT` | `temp/repos` | where the bare repos live. **This is the only copy of pushed source** |
| `GIT_HTTP_BASE_URL` | — | the external base URL reported as a repo's clone URL; must be reachable by a git client *outside* the cluster |
| `GIT_STORAGE_PREFLIGHT` | `enforce` | startup verification of the storage root — see below. `warn` / `off` to downgrade |
| `GIT_REF_LOCK_MAX_AGE` | `1h` | how long an abandoned `.lock` file may sit before the janitor clears it. `0` disables |
| `GIT_MAINTENANCE_INTERVAL` | `1h` | how often the in-process repack loop runs. `0` disables |
| `GIT_REPO_QUOTA_MB` | `0` (unlimited) | per-repo ceiling; an oversized push is refused by `git` itself via `receive.maxInputSize` |
| `GIT_FACTORY_INTERNAL_KEY` | — | **secret**; auth for `/internal/mirrors` + `/internal/clone-token`, shared with git_connector |
| `GIT_FACTORY_CLONE_TOKEN_KEY` | — | **secret**; HMAC key for runner clone tokens. Rotating it invalidates outstanding clone URLs |
| `GIT_NODE_FORWARD_KEY` | — | **secret**; node→node trust for the proxy and replication. Only needed with more than one node |
| `GIT_NODE_ADDRESS` | — | this node's own base URL, so it recognises itself. Unset on a single node |
| `EVENTS_URL` / `EVENTS_TRIGGER_KEY` | — | where push events are posted, and the key they are signed with. Unset simply turns the integration off |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP collector; telemetry off if unset |
| `LOG_LEVEL` | `info` | slog level |

### Storage preflight

git's correctness rests on three filesystem properties: `O_CREAT|O_EXCL` is atomic
(the ref-lock primitive), `rename()` is atomic (how every ref update commits), and
advisory locking is actually enforced. A filesystem missing them **corrupts
repositories rather than returning errors** — object storage mounted through FUSE
(s3fs, gcsfuse, Mountpoint, rclone) fails all three by design.

Because `GIT_STORAGE_ROOT` is "just a path", nothing else in the service would notice
that it stopped being a real filesystem. So `storagecheck.go` verifies all three at
startup, before anything can accept a push, and **refuses to start** by default when
one fails. The asymmetry is deliberate: a false negative is one loud error naming the
property and the override, while continuing is silent corruption of the only copy.

It runs in one pod, so it proves the properties **locally**. Confirming they hold
*across* clients needs two pods against one mount and is a deployment step — see
`ARCHITECTURE.md` §5 before raising `replicaCount`.

## Scaling

`replicaCount` is **1** by default and the volume is ReadWriteOnce: one pod owns the
bytes, and git has no cross-node write locking. The chart refuses, at render time, to
produce more than one replica without `accessMode: ReadWriteMany`, and refuses more
than one replica with the ref-lock janitor disabled — shared storage does not fix a
stale lock, it widens it from one host to every pod.

See `ARCHITECTURE.md` §5 for the roadmap and what is and is not deployed.

## RBAC convention

- action = camelCase verb + singular noun (`createRepo`, `listRepo`, `getRepo`, …)
- **Per-record resources are owner-first**: `<owner-namespace>/codearmory_git_factory/repos/<id>`,
  built by `resRepoOf` from the repo's own namespace rather than the caller's. Handlers
  load the repo *before* the permission check so the resource can name its owner, and a
  denial is rewritten to **404** so existence does not leak.
- Collection endpoints stay caller-scoped (`{username}/codearmory_git_factory/repos`) —
  "show me mine" is the caller-relative question.
- The manifests declare the honest **collection** resource on per-record endpoints,
  because conductor can only template a resource from path parameters and would
  otherwise gate every id with a string that is identical for a given caller. The
  per-record decision is made here, with the row in hand.
