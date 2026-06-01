# Blueprints — Architecture

## Overview

Blueprints is a self-hosted Terraform HTTP backend. It implements the Terraform state storage protocol (GET, POST, DELETE, LOCK, UNLOCK), stores Terraform state encrypted at rest in PostgreSQL, and caches reads in Redis.

```
Terraform / OpenTofu client
  |
  | Bearer token OR HTTP Basic auth (email:password)
  v
Blueprints :8081
  |
  +-- extractToken()
  |     Bearer  --> use as-is
  |     Basic   --> POST /login on Gatekeeper --> exchange for Bearer token
  |
  +-- checkPermissions() --> POST /check_permissions on Gatekeeper
  |
  +-- State cache (Redis)
  |     GET hit  --> return cached plaintext; skip DB
  |     GET miss --> read from DB, decrypt, cache
  |     POST / DELETE --> write DB, evict cache key
  |
  +-- PostgreSQL (states + locks tables)
```

## Authentication

Blueprints accepts two credential formats on every request:

**Bearer token** — a JWT issued by Gatekeeper. Used by API clients and the CLI.

**HTTP Basic** — `Authorization: Basic base64(email:password)`. On each request Blueprints calls `POST /login` on Gatekeeper to exchange the credentials for a Bearer token, then uses that token for the permission check. This allows Terraform to authenticate without a pre-issued token.

## Workspace scoping

Workspaces are addressed as `/state/{username}/{workspace}`. The workspace key stored in the DB is `{username}/{workspace}` and the Gatekeeper resource is `states/{username}/{workspace}`.

Permission actions per operation: `getState`, `updateState`, `deleteState`, `lockState`, `unlockState`.

## Encryption

All state is encrypted at rest using AES-256-GCM before being written to PostgreSQL.

```
encrypt(plaintext):
  nonce <- 12 random bytes (crypto/rand)
  ciphertext <- AES-256-GCM seal(encKey, nonce, plaintext)
  stored as: [nonce || ciphertext]

decrypt(blob):
  nonce <- blob[:12]
  ciphertext <- blob[12:]
  return AES-256-GCM open(encKey, nonce, ciphertext)
```

`ENCRYPTION_KEY` must be a 64-character hex string (32 bytes). Blueprints refuses to start without it — there is no plaintext fallback.

## Locking

Workspace locking serialises concurrent Terraform operations. All lock, unlock, and state-update operations use `SELECT ... FOR UPDATE` inside a transaction to serialise concurrent requests on the same workspace row, preventing TOCTOU races between the lock check and the subsequent write.

```
LOCK:
  BEGIN
  SELECT lock_data FROM locks WHERE workspace=$1 FOR UPDATE
    row exists  --> 423 Locked (return existing lock info)
    no row      --> INSERT INTO locks ...; COMMIT --> 200 OK

UNLOCK:
  BEGIN
  SELECT lock_data FROM locks WHERE workspace=$1 FOR UPDATE
    no row      --> COMMIT (idempotent 200 -- Terraform expects 200 even if unlocked)
    row exists  --> verify request ID == stored ID --> 409 on mismatch
                    DELETE FROM locks ...; COMMIT --> 200 OK

POST (state update):
  BEGIN
  SELECT lock_data FROM locks WHERE workspace=$1 FOR UPDATE
    locked, no ID supplied  --> 409 Conflict (return lock info)
    locked, ID mismatch     --> 409 Conflict
    unlocked or ID matches  --> UPSERT states ...; COMMIT
```

## State cache

Blueprints maintains a Redis read-cache for GET state requests.

- **Population:** successful DB reads populate Redis with a short TTL.
- **Eviction:** every successful POST (state update) and DELETE evicts the workspace key.
- **Fallback:** if Redis is unavailable the cache is bypassed transparently; every GET goes to the DB.

## Database

Two tables, created with `CREATE TABLE IF NOT EXISTS` on every startup:

| Table | Key | Columns |
|-------|-----|---------|
| `states` | `workspace TEXT PRIMARY KEY` | `data BYTEA` (encrypted), `updated_at` |
| `locks` | `workspace TEXT PRIMARY KEY` | `lock_data TEXT` (JSON), `created_at`, `updated_at` |

## Custom HTTP methods

Terraform's locking protocol uses the non-standard HTTP methods `LOCK` and `UNLOCK`. Go's `ServeMux` does not accept these as route prefixes. Blueprints registers the workspace path without a method prefix and dispatches manually:

```go
mux.HandleFunc("/state/{username}/{workspace}", lockUnlock(userKey))
// lockUnlock switches on r.Method -> handleLockState / handleUnlockState
```

## TLS

Optional server TLS. Set `TLS_CERT_FILE` + `TLS_KEY_FILE` to enable. Set `CA_CERT_FILE` additionally to require client certificate authentication (mTLS).

## Metrics

| Metric | Labels |
|--------|--------|
| `blueprints.state.get.total` | `result` (found / not_found) |
| `blueprints.state.update.total` | — |
| `blueprints.state.delete.total` | — |
| `blueprints.state.lock.total` | `result` (ok / conflict) |
| `blueprints.state.unlock.total` | — |
| `blueprints.permissions.checked.total` | `authorized` (bool) |
