# Containers

OCI container registry management proxy. Provides authenticated access to an external OCI/Docker registry (Docker Hub, GHCR, ECR, etc.) and exposes a management API for listing repositories, tags, and manifests. Also proxies the OCI distribution protocol so `docker push`, `docker pull`, and `docker login` can be routed through the platform.

## How it works

```
Authenticated user
  │
  └── GET /repositories ──────────────────► Containers :8089
        │  1. Verify Bearer token via Gatekeeper
        │  2. Forward management calls to REGISTRY_URL
        │  3. Return normalised JSON response
        │
  └── docker push/pull/login ─────────────► Containers :8089
        │  Transparent OCI distribution proxy (/v2/...)
        │  Passes Authorization header as-is to the upstream registry
        └─►  Upstream registry (REGISTRY_URL)
```

The management API (`/repositories`, `/tags`, `/manifests`) is protected by Gatekeeper RBAC. The OCI distribution proxy (`/v2/...`) passes requests to the upstream registry without RBAC interception — Docker clients authenticate directly with the upstream registry credentials.

## Requirements

- Go 1.25+
- An OCI-compatible container registry (Docker Hub, GHCR, ECR, any distribution-spec registry)

## Configuration

| Variable | Default | Description |
|---|---|---|
| `REGISTRY_URL` | — | **Required.** Base URL of the upstream OCI registry (e.g. `https://registry.hub.docker.com`, `https://ghcr.io`). |
| `REGISTRY_USERNAME` | — | Username for authenticating with the upstream registry. Optional for public registries. |
| `REGISTRY_PASSWORD` | — | Password or token for authenticating with the upstream registry. Optional for public registries. |
| `GATEKEEPER_URL` | `http://localhost:8081` | Gatekeeper base URL for permission checks. |
| `GATEKEEPER_SERVICE_KEY` | — | Service key for gatekeeper key rotation. |
| `PORT` | `8089` | Port the server listens on. |
| `OTEL_SERVICE_NAME` | `containers` | OTel service name. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTel Collector HTTP endpoint. Omit to disable telemetry. |
| `LOG_LEVEL` | `info` | Set to `debug` for verbose output. |

All variables support a `_FILE` suffix variant (e.g. `REGISTRY_PASSWORD_FILE`) that reads the value from a file path — useful for Docker secrets and Kubernetes secret mounts.

## Running locally

```bash
cd src/systems/containers
REGISTRY_URL=https://ghcr.io \
  REGISTRY_USERNAME=myuser \
  REGISTRY_PASSWORD=mytoken \
  GATEKEEPER_URL=http://localhost:8081 \
  go run .
```

## Docker

```bash
cd src/systems/containers
docker build -t containers:latest .

docker run -p 8089:8089 \
  -e REGISTRY_URL=https://ghcr.io \
  -e REGISTRY_USERNAME=myuser \
  -e REGISTRY_PASSWORD_FILE=/run/secrets/registry-token \
  -e GATEKEEPER_URL=http://gatekeeper:8081 \
  -e GATEKEEPER_SERVICE_KEY=containers-secret \
  containers:latest
```

## API

All management endpoints require `Authorization: Bearer <token>` verified by Gatekeeper.

### Management API

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/repositories` | `listRepository` on `containers/repositories` | List all repositories in the upstream registry |
| `GET` | `/repositories/{namespace}/{image}/tags` | `listTag` on `containers/repositories/{namespace}/{image}` | List tags for an image |
| `GET` | `/repositories/{namespace}/{image}/manifests/{reference}` | `getManifest` on `containers/repositories/{namespace}/{image}` | Get a manifest by tag or digest |
| `DELETE` | `/repositories/{namespace}/{image}/manifests/{digest}` | `deleteManifest` on `containers/repositories/{namespace}/{image}` | Delete a manifest by digest |

### OCI distribution proxy

| Path prefix | Description |
|---|---|
| `/v2/...` | Transparent OCI distribution protocol proxy. Used by `docker login`, `docker push`, and `docker pull`. Requests are forwarded to `REGISTRY_URL` with no RBAC interception. |

To use the containers service as a `docker push`/`pull` target, point Docker at the containers service host and authenticate with your upstream registry credentials.

### List repositories

```bash
curl http://containers:8089/repositories \
  -H "Authorization: Bearer <token>"
# → [{"name": "myorg/myapp"}, {"name": "myorg/mybase"}]
```

### List tags

```bash
curl http://containers:8089/repositories/myorg/myapp/tags \
  -H "Authorization: Bearer <token>"
# → {"name": "myorg/myapp", "tags": ["latest", "v1.2.0", "v1.1.0"]}
```

### Get manifest

```bash
curl http://containers:8089/repositories/myorg/myapp/manifests/v1.2.0 \
  -H "Authorization: Bearer <token>"
```

Response:
```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "digest": "sha256:abc123...",
  "config": {
    "mediaType": "application/vnd.docker.container.image.v1+json",
    "size": 7023,
    "digest": "sha256:def456..."
  },
  "layers": [...],
  "repository": "myorg/myapp",
  "reference": "v1.2.0",
  "fetched_at": "2026-06-01T12:00:00Z"
}
```

### Delete manifest

```bash
curl -X DELETE \
  http://containers:8089/repositories/myorg/myapp/manifests/sha256:abc123... \
  -H "Authorization: Bearer <token>"
# → 204 No Content
```

Manifests must be deleted by digest, not by tag. To find the digest, use `GET /manifests/{tag}` first.

## Metrics

| Metric | Description |
|--------|-------------|
| `containers.repositories.listed.total` | List repository calls |
| `containers.tags.listed.total` | List tag calls, labelled by `repository` |
| `containers.manifests.fetched.total` | Manifest fetch calls, labelled by `repository` |
| `containers.manifests.deleted.total` | Manifest delete calls, labelled by `repository` |

## Testing

```bash
# Unit tests
cd src/systems/containers
go test ./...
```
