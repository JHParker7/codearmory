# Conductor — Architecture

## Overview

Conductor is the public-facing API gateway. Every REST API request from external clients enters the platform through it. It verifies caller identity with Gatekeeper, rewrites authentication headers, validates request shape, and reverse-proxies the request to the appropriate backend service.

```
Client (Bearer JWT)
  |
  v
Conductor :8082
  |
  +-- 1. Block list check (per IP+userID pair)
  |
  +-- 2. Routing (hybrid -- see below)
  |
  +-- 3. Path param validation (UUID / slug)
  |
  +-- 4. Body validation (JSON, 64 KB cap, signup field validation)
  |
  +-- 5. Identity check: GET /users/{id} on Gatekeeper
  |        (public endpoints skip this step)
  |
  +-- 6. Header rewrite:
  |        Strip: Authorization (unless forward_auth), X-User-ID,
  |               X-Conductor-Token, X-Conductor-Timestamp,
  |               X-Forwarded-Host, X-Forwarded-Proto, X-Real-IP, X-Service-Key
  |        forward_auth=false --> inject X-User-ID + HMAC token
  |        forward_auth=true  --> keep Authorization header unchanged
  |
  +-- 7. httputil.ReverseProxy --> backend service
```

## Service registry

Conductor fetches its routing table from Registry every 30 seconds. At startup it retries until the registry is reachable. The table is held in two in-memory structures protected by a single `sync.RWMutex`:

- **`servicesMap`** — maps service name to `{ url, proxy, forwardAuth }`
- **`endpointsList`** — flat list of compiled `endpointEntry` structs

Both are swapped atomically on every refresh so readers always see a consistent pair.

### endpointEntry

Each registered endpoint is compiled into:

```
endpointEntry {
  method      string          // HTTP method
  pattern     *regexp.Regexp  // "/users/{id}" compiled to ^/users/([^/]+)$
  paramNames  []string        // ordered: ["id"]
  action      string          // Gatekeeper action, e.g. "getUser"
  resource    string          // may contain {param} placeholders
  public      bool            // skip auth + RBAC when true
  serviceName string
}
```

Path parameters `{id}` are compiled to `([^/]+)` capturing groups. Captured values are substituted into the `resource` string at request time to produce the per-resource permission string (e.g. `gatekeeper/users/abc-123`).

## Routing

Conductor uses hybrid routing to support two URL forms:

```
1. Service-prefixed:  /forge/executions/abc-123
   --> strip "forge/", match "/executions/abc-123" within forge's endpoints
   --> 404 if forge is registered but the endpoint is not

2. Full path:         /users/abc-123
   --> match against all registered endpoints across all services
   --> 404 if no match found
```

Service-prefixed routing is tried first. If the first path segment is a registered service name, the full-path fallback is never attempted.

## Authentication

Conductor performs identity verification only — it does not check RBAC. Permission checking is delegated to each backend service, which calls `POST /check_permissions` on Gatekeeper directly.

**`checkUserAuth` flow:**

1. Decode the JWT payload locally (no signature verification) to extract the `sub` claim (user ID).
2. Call `GET /users/{id}` on Gatekeeper, forwarding the original `Authorization` header. Gatekeeper performs full JWT signature verification.
3. `200 OK` from Gatekeeper means the user exists and the token is valid.

Public endpoints (declared with `public: true` in the registry manifest) skip this check entirely.

## Header injection

After a successful identity check, Conductor rewrites headers before forwarding:

| `forward_auth` | Injected | Stripped |
|----------------|---------|---------|
| `true` | `Authorization` unchanged | — |
| `false` | `X-User-ID`, `X-Conductor-Token`, `X-Conductor-Timestamp` | `Authorization` |

`X-Conductor-Token` is `hex(HMAC-SHA256(CONDUCTOR_FORWARD_KEY, "conductor:{userID}:{timestamp}"))`. Backend services with `forward_auth=false` can verify this token to confirm that `X-User-ID` was injected by a trusted Conductor instance, not forged by a client.

Conductor strips `X-User-ID`, `X-Conductor-Token`, `X-Conductor-Timestamp`, `X-Service-Key`, and standard proxy headers from every incoming request before conditionally re-setting them.

## Suspicious-activity blocking

Conductor tracks authentication failures per `(IP, userID)` pair. A failure is recorded when a well-formed JWT (identifiable user ID) passes the identity check but Gatekeeper subsequently reports an error or user-not-found. Missing or malformed tokens do not count.

After 10 failures the pair is blocked for one hour — all requests from that pair receive `403 Forbidden` without any backend call. A successful authentication resets the counter for that pair.

The block list is in-memory and does not survive restarts.

## Path and body validation

Validation runs before auth so bad input gets `400 Bad Request`, not `401 Unauthorized`:

- **UUID params** (`{id}`) — normalised to lowercase hyphenated form. The 32-char hex form is accepted and converted.
- **Slug params** (`username`, `workspace`, `org`, `team`) — validated against `[a-zA-Z0-9_-]{1,64}`.
- **Request body** — capped at 64 KB; JSON bodies checked for syntax validity.
- **Signup body** — additionally validated: `email` (format), `username` (slug pattern), `password` (min 8 chars).

## Metrics

| Metric | Labels |
|--------|--------|
| `conductor.requests.allowed.total` | — |
| `conductor.requests.rejected.total` | `reason` (no_token, malformed_token, unauthorized, user_not_found, gatekeeper_error) |
| `conductor.ips.blocked.total` | `source_ip`, `user_id` |
