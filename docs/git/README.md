# git_connector

The `git_connector` service (in-repo directory `src/systems/git`) is a backend-agnostic **git credential broker**. Users link one or more git backends (GitHub, GitLab, Forgejo, or a generic git server); the broker mints — or just-in-time brokers — clone credentials on demand for whatever backend a repository belongs to. Credential material is encrypted at rest and is never returned by backend reads.

This is the **core git integration**. It is *not* a repository-management service: it does not create repos, branches, or pull requests. (Repo management lives in the optional, non-core [`gitea_integration`](#relationship-to-gitea_integration) service.) Git's only job is to answer the question *"give me an authenticated clone URL for this repo"* — for a user, or for Forge/Workflows running a job on the user's behalf.

## What it does

A user links a backend once (a GitHub App, a GitLab OAuth client, a Forgejo admin token, a PAT, etc.). From then on, any caller can ask for a credential by **repository URL** and the broker:

1. Derives the host from the repo URL (e.g. `github.com`, `gitlab.example.com`).
2. Finds the caller's linked backend for that host (at most one backend per host, so the match is unambiguous).
3. Mints or brokers a credential appropriate to that backend's auth mode.
4. Returns the username/secret plus an authenticated HTTPS `clone_url` with the secret injected as basic-auth userinfo.

```
Client (Bearer JWT)
  │
  └── POST /credentials {repo_url} ───────────────► Git :8096
        │  1. Verify token via Gatekeeper /check_permissions (forward_auth)
        │  2. deriveHost(repo_url) → look up the caller's backend for that host
        │  3. Mint (app/oauth/admin) or broker (pat/token/basic) a credential
        │  4. Persist any rotated material (e.g. GitLab refresh token)
        │
        ▼
      { username, secret, clone_url, backend, backend_type, expires_at }
```

Like Forge, Git authenticates each request **directly with Gatekeeper** (it is registered `forward_auth: true`): the raw Bearer token is forwarded to `POST /check_permissions` and the resolved `user_id` is the credential owner. The service never issues credentials on the basis of a Conductor-injected `X-User-ID` header alone.

## Backends and auth modes

A backend has a `type` and an `auth.mode`. The broker mints **short-lived** credentials where the backend supports it, and otherwise brokers a stored secret just-in-time:

| Type | Auth mode | What it does | Lifetime |
|------|-----------|--------------|----------|
| `github` | `app` | GitHub App (`app_id` + `installation_id` + `private_key`) → an installation token | **Short-lived** (~1h, genuinely time-boxed by GitHub) |
| `github` | `pat` | Brokers a stored personal access token as-is | Lifetime of the PAT |
| `gitlab` | `oauth` | Exchanges a refresh token for an access token; the rotated refresh token is persisted | **Short-lived** (~2h) |
| `gitlab` | `token` | Brokers a stored personal/group/project access token as-is | Lifetime of the token |
| `forgejo` | `admin` | Uses an admin token to mint a **per-user, repo-scoped** token, revoking the broker's prior tokens for that user first (revoke-on-reuse) | Until the next mint replaces it |
| `forgejo` | `token` | Brokers a stored personal access token as-is | Lifetime of the token |
| `generic` | `basic` | Brokers a stored username + password/token over HTTPS basic auth | Lifetime of the stored secret |

**Honest framing.** Three modes (`github` `app`, `gitlab` `oauth`, `forgejo` `admin`) produce credentials that are short-lived or revoke-on-reuse. The remaining modes (`pat`, `token`, `basic`) broker a long-lived secret you supplied — the value is held encrypted and only released at mint time, but the broker cannot make a static PAT expire on its own. Choose the minting modes when you want a real reduction in credential blast radius.

Where a mint produces a new value the backend expects to persist (notably GitLab's rotated refresh token), the broker re-seals and stores it before returning, so the next mint continues to work.

## Encryption at rest

All credential material — App private keys, PATs, OAuth refresh tokens, admin tokens, basic-auth passwords — is marshalled to JSON and sealed with **AES-256-GCM** (`nonce‖ciphertext`) before it touches the database. The key is derived (SHA-256) from `GIT_ENCRYPTION_KEY`, so any passphrase length works. The service **refuses to start** without `GIT_ENCRYPTION_KEY` — credentials are never stored in plaintext.

Backend reads (`GET /backends`, `GET /backends/{id}`) return a secret-free projection (`id`, `name`, `type`, `base_url`, `host`, `auth_mode`, timestamps). The sealed credential is never returned by the API and never logged.

## Requirements

- Go 1.25+
- PostgreSQL

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgresql://postgres:postgres@localhost:5432/git` | PostgreSQL connection string |
| `GATEKEEPER_URL` | `http://localhost:8080` | URL of Gatekeeper, used for permission checks and service-key rotation |
| `GATEKEEPER_SERVICE_KEY` | — | Shared service key registered with Gatekeeper. Required for service-to-service authentication in production. |
| `GIT_ENCRYPTION_KEY` | — | **Required.** Passphrase used to derive the AES-256 key that encrypts credential material at rest. The service exits on startup if unset. |
| `GIT_INTERNAL_KEY` | — | Shared key (`X-Internal-Key`) that authenticates Forge/Workflows calls to `POST /internal/clone-token`. When unset, the internal endpoint rejects all requests. |
| `GIT_FACTORY_URL` | — | git-factory's in-cluster base URL, set by the Helm chart. On its own it is what the **platform backend** row is seeded from at startup, so repos hosted on git-factory are clonable for every user with no per-user link and without waiting on builder. Combined with `GIT_FACTORY_INTERNAL_KEY` it also reaches git-factory's internal mirror surface. |
| `GIT_FACTORY_INTERNAL_KEY` | — | Shared secret for git-factory's `/internal/mirrors` surface — the same value git-factory itself holds. **Both** this and `GIT_FACTORY_URL` must be set for a `prefer_mirror` backend to be served from the cache; otherwise the broker silently returns the upstream URL. |
| `PORT` | `8096` | Port the server listens on |
| `OTEL_SERVICE_NAME` | `git` | Service name reported in traces and metrics |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `GIT_ENCRYPTION_KEY_FILE`, `DATABASE_URL_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

### TLS variables

Git supports the same `TLS_*` variables as the other Go services (`TLS_CERT_FILE`, `TLS_KEY_FILE`, `TLS_CLIENT_AUTH`, `TLS_CLIENT_CA_FILE`, `TLS_CLIENT_CERT_FILE`, `TLS_CLIENT_KEY_FILE`, `TLS_CA_FILE`) for server TLS, mutual TLS, and outbound client certificates.

## API

All user-facing endpoints require a Gatekeeper-issued Bearer token (`Authorization: Bearer <token>`) and are reached through Conductor under the `/git` prefix. Git verifies permissions directly with Gatekeeper on every request.

### Backends

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/git/backends` | `listBackend` on `git/backends` | List the caller's linked backends (no secrets) |
| `POST` | `/git/backends` | `createBackend` on `git/backends` | Link a new backend |
| `GET` | `/git/backends/{id}` | `getBackend` on `git/backends/{id}` | Get one backend (no secrets) |
| `PUT` | `/git/backends/{id}` | `updateBackend` on `git/backends/{id}` | Update a backend's `base_url` and/or `auth` |
| `DELETE` | `/git/backends/{id}` | `deleteBackend` on `git/backends/{id}` | Unlink a backend |
| `POST` | `/git/backends/{id}/test` | `testBackend` on `git/backends/{id}` | Verify a backend can actually mint (no repo needed) |

### Repos (selector)

Convenience read endpoints that back the repo/branch pickers in the portal and CLI (e.g. the Forge run form and the Workflows step editor). They enumerate what the caller can clone; they do **not** manage repositories.

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/git/repos` | `listRepo` on `git/repos` | List clone targets — enumerated across linked backends plus repos pinned manually |
| `POST` | `/git/repos` | `createRepo` on `git/repos` | Pin a repo to the selector (for generic/un-enumerable backends, or to surface extras) |
| `DELETE` | `/git/repos/{id}` | `deleteRepo` on `git/repos/{id}` | Remove a pinned repo |
| `GET` | `/git/repos/branches?url=<clone-url>` | `listRepo` on `git/repos` | List a repo's branches (for the checkout branch selector); `{ name, default }` per branch, `[]` for generic/un-enumerable backends |

The branch list feeds a `checkout.ref` (see [Auto-checkout](#auto-checkout-checkout--actionscheckout-equivalent)); like repo enumeration it fetches a single page and degrades to a free-text ref when a backend can't be enumerated.

### Credentials

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `POST` | `/git/credentials` | `mintCredential` on `git/credentials` | Mint a credential for a repository URL |

### Internal (Forge / Workflows)

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/git/internal/clone-token` | `X-Internal-Key: $GIT_INTERNAL_KEY` | Mint an authenticated clone URL for a named user + repo. Not routed through Conductor's RBAC — authenticated by the shared internal key. |
| `POST` | `/git/internal/repos/sync-config` | `X-Internal-Key: $GIT_INTERNAL_KEY` | Asks whether a repo's `.armory/workflows` should sync, and from which branches. Called by Workflows. |
| `POST` | `/git/internal/backends/platform` | `X-Internal-Key: $GIT_INTERNAL_KEY` | Register an in-cluster git host as a **platform backend**, so clones of its repos resolve for every user with no per-user link. The platform's own git-factory is seeded from `GIT_FACTORY_URL` at startup instead; this remains for a not-yet-upgraded builder and any other platform-deployed host. |

### Link a backend

```bash
# GitHub App (short-lived installation tokens)
curl -X POST http://conductor:8080/git/backends \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "github-acme",
    "type": "github",
    "base_url": "https://github.com",
    "auth": {
      "mode": "app",
      "app_id": 123456,
      "installation_id": 7891011,
      "private_key": "-----BEGIN RSA PRIVATE KEY-----\n..."
    }
  }'
# → 201 { "id": "...", "name": "github-acme", "type": "github", "auth_mode": "app", ... }
```

At most one backend per `(owner, host)`, and backend names are unique per owner. Linking a second backend for a host already linked returns `409 Conflict`.

### Mint a credential

```bash
curl -X POST http://conductor:8080/git/credentials \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"repo_url": "https://github.com/acme/widgets.git"}'
# → 200
# {
#   "type": "basic",
#   "username": "x-access-token",
#   "secret": "<short-lived token>",
#   "clone_url": "https://x-access-token:<token>@github.com/acme/widgets.git",
#   "backend": "github-acme",
#   "backend_type": "github",
#   "expires_at": "2026-06-29T13:00:00Z"
# }
```

`404` is returned when the caller has no backend linked for the repository's host.

## Forge integration (`git:` secret_ref)

Forge jobs reference credentials by **secret_ref** rather than embedding them. A `secret_ref` value of the form `git:<https-repo-url>` makes Forge call the broker's `POST /internal/clone-token` at **dispatch time** and inject the resulting authenticated clone URL into the named job env var. The credential is resolved per-run and is **never persisted** on the execution record.

```jsonc
// Forge execution / Workflows step
{
  "image": "alpine/git",
  "command": ["git", "clone", "$REPO_URL", "/work"],
  "secret_refs": {
    "REPO_URL": "git:https://gitlab.example.com/acme/widgets.git"
  }
}
```

Forge needs `GIT_INTERNAL_URL` (the broker's base URL) and `GIT_INTERNAL_KEY` (matching the broker's `GIT_INTERNAL_KEY`) set for `git:` references to resolve; otherwise the reference fails the execution.

The older `gitea:<owner>/<repo>` secret_ref scheme still works, but it targets the optional [`gitea_integration`](#relationship-to-gitea_integration) service (Forge's `GITEA_INTERNAL_URL` / `GITEA_INTERNAL_KEY`), not this broker. New jobs should prefer `git:` with a full repo URL, which works across all backend types.

### Auto-checkout (`checkout`) — actions/checkout equivalent

A `git:`/`gitea:` secret_ref only puts an **authenticated clone URL in an env var** — the job still has to run `git clone` itself. To have Forge check the repo out **for** you, add a `checkout` block. Before running `command`, Forge prepends a `git clone … && cd …` prologue so the command starts **inside** the checked-out repo — the [`actions/checkout`](https://github.com/actions/checkout) equivalent:

```jsonc
// Forge execution / Workflows step — clone into ./widgets, then run there
{
  "image": "alpine/git",
  "command": ["sh", "-c", "git rev-parse HEAD && make build"],
  "secret_refs": { "GIT_CLONE_URL": "git:https://gitlab.example.com/acme/widgets.git" },
  "checkout": {}          // env: GIT_CLONE_URL, path: <repo name>, shallow depth-1
}
```

`checkout` fields (all optional):

| Field | Default | Meaning |
|-------|---------|---------|
| `env` | `GIT_CLONE_URL` | Env var holding the clone URL. Must also be set by `secret_refs` or `env`. |
| `path` | the shared-volume root (`.`) when the working dir is a `workdir` volume, else the repo name from the ref (else `repo`) | Directory to clone into and `cd` into (relative, no `..`). When the checkout runs into a shared workspace volume made the working dir (as `forge/git-clone` does), it clones into the volume root so the volume itself becomes the working tree — downstream steps mount it at their root. |
| `ref` | remote's default branch | Branch or tag to check out (`git clone --branch`). Commit SHAs are not supported here. |
| `depth` | `1` (shallow) | `git clone --depth`; `0` requests a full clone. |

Requirements and behaviour:

- **`git` must be in the image** — or **omit `image` entirely** and Forge runs the checkout on its built-in minimal git image (`FORGE_GIT_IMAGE`, forge-controlled, bypasses the image allowlist). This is what the `forge/git-clone` workflow step does, so users never pick or maintain a git-capable image. The **command must be a shell form** (`["sh","-c", …]`) so the prologue can be woven in — both are enforced at submit (`400` otherwise).
- A **clone failure fails the whole execution** — the command never runs against an empty dir.
- Works on **every runtime backend** (docker, k8s, kata, gvisor) because it is a command transform, not a runtime feature.
- The env var can also be a plain public URL set via `env` (no creds) — checkout doesn't require a `secret_ref`.

## RBAC

Git registers the following actions and resources. The resources are namespaced with the owner's `{username}/` prefix when stored as grants, matching the platform's resource-scoping convention.

| Action | Resource | Default grant |
|--------|----------|---------------|
| `listBackend` | `{username}/git/backends` | granted to every user (`grant_on: user`) |
| `createBackend` | `{username}/git/backends` | granted to every user |
| `getBackend` | `{username}/git/backends/*` | granted to every user |
| `updateBackend` | `{username}/git/backends/*` | granted to every user |
| `deleteBackend` | `{username}/git/backends/*` | granted to every user |
| `testBackend` | `{username}/git/backends/*` | granted to every user |
| `mintCredential` | `{username}/git/credentials` | granted to every user |

Each user manages and mints against their own backends out of the box. The internal `clone-token` endpoint is **not** part of the RBAC surface — it is gated solely by `GIT_INTERNAL_KEY`.

## Relationship to `gitea_integration`

Git (this service) is the **core** credential broker — it is in the Go workspace, deployed by the Helm chart, and registered via the registry manifest.

[`gitea_integration`](../platform-guide.md) is a separate, **optional non-core** service for **Forgejo/Gitea repository management** (creating repos, listing branches/tags/commits, opening and merging pull requests). Its source lives in its own `codearmory-gitea_integration` repo, and **Builder** deploys and registers it on demand from `src/systems/builder/files/services/gitea_integration.json`. Most deployments do not run Forgejo, so it is off until an admin enables it.

In short: **Git brokers clone credentials for any backend; `gitea_integration` manages repositories on Forgejo.** They are independent — you can use either, both, or neither.

## Deployment

Git is a **core** service:

- in-repo at `src/systems/git`, part of the `src/systems/go.work` workspace;
- deployed by the Helm chart (`infra/helm/codearmory`);
- registered via the registry manifest (`infra/local/registry-manifest.json` and the Helm equivalent), so Conductor routes `/git/*` to it.

## Running locally

```bash
cd src/systems/git
DATABASE_URL=postgresql://postgres:pass@localhost:5432/git \
  GATEKEEPER_URL=http://localhost:8081 \
  GIT_ENCRYPTION_KEY=local-dev-passphrase \
  GIT_INTERNAL_KEY=local-internal-key \
  go run .
```

## Testing

```bash
cd src/systems/git
go test ./...
```
