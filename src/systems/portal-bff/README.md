# portal-bff

The portal Backend-for-Frontend, in Go. Serves the built React SPA and is the
browser's single entry point to the platform: the SPA only ever talks to this
server, never to conductor directly. Ported from the original Node/Express BFF
(`../portal/bff`) to remove the npm runtime from the production image — the SPA
build still uses npm/Vite, but the running server is a static Go binary with no
`node_modules`.

## Responsibilities

- **`/api/*` passthrough** — every request is proxied verbatim to conductor,
  forwarding the bearer `Authorization` header (`proxy.go`).
- **`/api/state/*`** — the one non-verbatim route. Normalizes conductor's
  200/204/423 branching into a single uniform `WorkspaceView` (`workspace.go`),
  cached per `(workspace path, token)` with a short TTL (`cache.go`); writes
  invalidate the path so reads stay write-coherent (`state.go`).
- **`/api/{svc}/ui/*`** — streams a registered service's embedded mini-portal
  (HTML/JS/CSS) with the upstream `Content-Type` verbatim, binary-safe (`ui.go`).
- **SPA** — serves static assets from `PUBLIC_DIR`, falling back to `index.html`
  for client-side routes; the fallback is per-IP rate limited (`spa.go`,
  `ratelimit.go`).
- **Observability** — OpenTelemetry traces/metrics/logs via the shared
  `codearmory_sdk/telemetry`, exported over OTLP (no Prometheus scrape endpoint).

## Config (env)

| Var | Default | Meaning |
|---|---|---|
| `PORT` | `3001` | listen port (SPA + API on one port) |
| `CONDUCTOR_URL` | `http://localhost:8080` | upstream API gateway |
| `PUBLIC_DIR` | `./public` | built SPA dir; absent → API-only (dev) |
| `STATE_CACHE_TTL_MS` | `2000` | workspace-state cache TTL |
| `TRUST_PROXY` | `1` | `false`/`0` to read client IP from `RemoteAddr` instead of `X-Forwarded-For` |
| `TLS_CA_FILE` | — | CA to trust for outbound TLS to conductor |
| `LOG_LEVEL` | `info` | slog level |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | enables OTLP export; unset → stderr logs only |

## Dev

There is no Vite middleware in the Go server. In development, run the SPA's Vite
dev server (`cd ../portal && npm run dev:spa`) and point its `server.proxy` for
`/api` at this server; run this server with no `PUBLIC_DIR` present so it serves
API-only. In production the SPA is built to `public/` and served statically.

## Build & test

```bash
go build ./...
go test ./...
# production image (context = src/systems):
docker build -f portal-bff/Dockerfile -t portal-bff ../   # from src/systems
```
