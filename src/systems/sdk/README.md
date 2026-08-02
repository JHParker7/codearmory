# codearmory_sdk

Shared Go packages used by codearmory services. Import path: `github.com/code-armory-app/codearmory_sdk`.

## Packages

### `registry` — service key rotation

Provides `StartKeyRotation`, which launches a background goroutine that rotates the service's gatekeeper key on a configurable interval. Services that register with gatekeeper receive an initial key at startup; this package keeps that key current without a restart.

```go
import "github.com/code-armory-app/codearmory_sdk/registry"

getKey := registry.StartKeyRotation(ctx, gatekeeperURL, "myservice", initialKey, 25*time.Minute)
// getKey() always returns the current key; safe to call from multiple goroutines.
```

If `gatekeeperURL`, `serviceName`, or `initialKey` is empty, the goroutine is not started and the getter always returns `initialKey`.

### `telemetry` — OpenTelemetry setup

Initialises OTLP exporters for logs, traces, and metrics, wires up the global `TracerProvider` and `MeterProvider`, and returns an `slog.Handler` that forwards records to the collector.

```go
import "github.com/code-armory-app/codearmory_sdk/telemetry"

otelHandler, shutdown, err := telemetry.Setup(ctx, "myservice")
if err != nil {
    // OTEL_EXPORTER_OTLP_ENDPOINT not set — log to stderr only
} else {
    slog.SetDefault(slog.New(telemetry.NewFanoutHandler(jsonHandler, otelHandler)))
    defer shutdown(context.Background())
}
```

`Setup` returns a non-nil error when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset; callers should fall back to stderr-only logging in that case.

`NewFanoutHandler` fans slog records out to multiple handlers simultaneously — useful for writing to both stderr and the OTel log exporter.

### `gatekeeper` — permission-check client

`Client` delegates per-request authorisation to gatekeeper's `/check_permissions` endpoint. Services that are not gatekeeper itself use this instead of verifying JWTs directly.

```go
import gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"

client := &gk.Client{
    URL:        gatekeeperURL, // e.g. "http://localhost:8080"
    Service:    "myservice",   // this service's registered name
    HTTPClient: httpClient,    // optional; defaults to http.DefaultClient
}

func handleSomething(w http.ResponseWriter, r *http.Request) {
    userID, orgID, ok := client.CheckPermissions(ctx, w, r, "createThing", "myservice/things")
    if !ok {
        return // CheckPermissions already wrote the HTTP error
    }
    // proceed
}
```

`CheckPermissions` extracts the caller's `Authorization: Bearer` token, forwards it to gatekeeper with the `(service, action, resource)` triple, and returns `(userID, orgID, true)` when authorised. On any failure it writes the appropriate HTTP error (`401`, `403`, `500`, or `503`) and returns `("", "", false)`.

The `HTTPClient` field accepts any `*http.Client` — pass an OTel-instrumented client to propagate trace context to gatekeeper.

#### `Check` — when you need to name an owner

`Check` is the same call returning the full `Subject`, which adds the caller's
**username**:

```go
sub, ok := client.Check(ctx, w, r, "getThing", "myservice/things")
if !ok {
    return
}
ns, known := sub.Namespace() // the caller's RBAC namespace (their username)
```

Gatekeeper keys resources by **username**, but `CheckPermissions` only reports the
**user id** — so a service could not build an owner-first resource naming its own caller
without a second round trip to `/oauth/userinfo`. `Check` closes that gap.

`Namespace()` returns two values on purpose. The tempting one-liner
`sub.Username + "/myservice/things/" + id` silently produces `/myservice/things/<id>`
when the username is absent, which matches no grant — it fails closed, so it is not a
security problem, but it is an opaque 403 with no way to distinguish a permission the
caller genuinely lacks from a namespace the service never had. The username is absent for
a client-credentials subject (an OAuth client owns no namespace) and against an older
gatekeeper that does not yet send the field, so the SDK keeps working during a rollout in
either order.

Why this matters: an unscoped resource like `myservice/things/{id}` gets the **caller's**
namespace prefixed by gatekeeper, so it evaluates identically whoever asks and authorises
every id — a per-record gate in appearance only. Sending an owner-first resource is the
fix, and it needs a namespace to name.

### `gatekeeper` — audit ingest

The same `Client` writes to gatekeeper's central audit log, so every service's trail lands
in one place instead of each keeping its own.

```go
client := &gk.Client{
    URL:        gatekeeperURL,
    Service:    "myservice",
    HTTPClient: httpClient,
    // Returned by registry.StartKeyRotation — an accessor, not a fixed string, so
    // audit writes keep working across a key rotation.
    ServiceKey: serviceKey,
}

client.Audit(ctx, "deleteThing", thingID, userID, "deleted via API")
```

`Audit` is **fire-and-forget and never blocks the request**: it detaches from the request
context (`context.WithoutCancel`) so the write survives the response returning, applies its
own timeout, and authenticates with `X-Service-Key: <service>:<key>`. It returns nothing —
an audit failure is logged, never surfaced to the caller, because losing an audit line must
not fail the operation that produced it.

It is inert without `ServiceKey`, and drops entries missing `action` or `resourceID` rather
than letting gatekeeper reject them as recurring log noise. Long `detail` values are
truncated.

> The service must be listed in gatekeeper's `GATEKEEPER_SERVICES`, or every east-west call
> — audit included — is rejected with a 401 and the trail silently disappears.

## Development

The SDK lives **in this monorepo** at `src/systems/sdk` (module path
`github.com/code-armory-app/codearmory_sdk`). Every service `replace`s it to `../sdk`, so a
change lands everywhere at once with no publish-and-bump cycle. It is a member of
`src/systems/go.work`, so a workspace build already uses the local source.

That `replace` is why service Dockerfiles build with **`src/systems` as the context**
rather than the service directory — the build needs both the service tree and `sdk/`. They
also set `GOWORK=off`, since `go.work` is deliberately not copied.

**Do not run `go work sync`.** It rewrites each module's `go.mod` to the workspace-wide
build list but leaves the hashes in `go.work.sum` rather than the module's own `go.sum`.
Workspace builds keep working, so it looks harmless — but Docker builds run in module mode
and fail with `missing go.sum entry`. To align a dependency, change it in the module and
run `GOWORK=off go mod tidy` there, then verify with `GOWORK=off go build ./...`.
