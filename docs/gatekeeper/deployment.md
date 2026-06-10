# Deployment

## Local (Docker Compose)

The local stack in `infra/local/` runs the full application alongside its dependencies.

```bash
cd infra/local
docker compose up --build
```

Services:

| Service | Port | Description |
|---------|------|-------------|
| gatekeeper | 8081 | Auth, RBAC, user management |
| blueprints | 8084 | OpenTofu/Terraform state backend |
| conductor | 8080 | API gateway |
| forge | 8083 | Sandboxed execution service |
| registry | 8082 | Service discovery and endpoint registry |
| postgres | 5432 | Primary datastore |
| redis | 6379 | Permission-check and user cache |

Images are built from source on each `docker compose up --build`. Pre-built images are available on GHCR:

```bash
docker pull ghcr.io/code-armory-app/gatekeeper:alpha-latest
```

## Kubernetes (Helm)

The Helm chart is at `infra/helm/gatekeeper`. It deploys:

- **Deployment** — gatekeeper pods (replica count managed by HPA)
- **Service** — ClusterIP on port 8081
- **Ingress** — nginx ingress controller routes external HTTP(S) traffic to the service
- **HorizontalPodAutoscaler** — scales between 3 and 10 replicas based on CPU (70%) and memory (80%)

### Prerequisites

- Kubernetes 1.23+ (for `autoscaling/v2`)
- [`ingress-nginx`](https://kubernetes.github.io/ingress-nginx/) deployed in the cluster
- [`metrics-server`](https://github.com/kubernetes-sigs/metrics-server) deployed (required for HPA memory scaling)

### Install

```bash
helm install gatekeeper ./infra/helm/gatekeeper \
  --set database.url="postgres://user:pass@host:5432/gatekeeper" \
  --set ingress.host="gatekeeper.example.com" \
  --set env.OTEL_EXPORTER_OTLP_ENDPOINT="http://otelcol.monitoring:4318"
```

### Upgrade

```bash
helm upgrade gatekeeper ./infra/helm/gatekeeper \
  --set database.url="postgres://user:pass@host:5432/gatekeeper" \
  --set ingress.host="gatekeeper.example.com"
```

### Values Reference

| Value | Default | Description |
|-------|---------|-------------|
| `image.repository` | `gatekeeper` | Container image repository |
| `image.tag` | `0.0.4` | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `service.type` | `ClusterIP` | Kubernetes service type |
| `service.port` | `8081` | Service port |
| `ingress.enabled` | `true` | Enable nginx Ingress |
| `ingress.className` | `nginx` | Ingress class |
| `ingress.host` | `""` | Hostname to route (required) |
| `ingress.tls.enabled` | `false` | Enable TLS termination |
| `ingress.tls.secretName` | `""` | Kubernetes Secret containing the TLS certificate |
| `ingress.annotations` | `{}` | Additional Ingress annotations |
| `autoscaling.minReplicas` | `3` | Minimum pod count |
| `autoscaling.maxReplicas` | `10` | Maximum pod count |
| `autoscaling.targetCPUUtilizationPercentage` | `70` | CPU utilisation target (% of request) |
| `autoscaling.targetMemoryUtilizationPercentage` | `80` | Memory utilisation target (% of request) |
| `resources.requests.cpu` | `100m` | CPU request (also the HPA baseline) |
| `resources.requests.memory` | `128Mi` | Memory request (also the HPA baseline) |
| `resources.limits.cpu` | `500m` | CPU limit |
| `resources.limits.memory` | `256Mi` | Memory limit |
| `database.url` | `""` | Creates a Secret containing this value |
| `database.existingSecret` | `""` | Name of a pre-existing Secret to use instead |
| `database.secretKey` | `database-url` | Key within the Secret |
| `env.OTEL_SERVICE_NAME` | `gatekeeper` | OTel service name |
| `env.OTEL_EXPORTER_OTLP_ENDPOINT` | `""` | OTel Collector endpoint |

### TLS with cert-manager

```yaml
# values-prod.yaml
ingress:
  host: gatekeeper.example.com
  tls:
    enabled: true
    secretName: gatekeeper-tls
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
```

```bash
helm upgrade gatekeeper ./infra/helm/gatekeeper -f values-prod.yaml \
  --set database.url="postgres://..."
```

### Using an Existing Secret for DATABASE_URL

If you manage secrets externally (e.g. via Vault or Sealed Secrets), skip `database.url` and point to the existing secret:

```bash
helm install gatekeeper ./infra/helm/gatekeeper \
  --set database.existingSecret="gatekeeper-db-secret" \
  --set database.secretKey="url" \
  --set ingress.host="gatekeeper.example.com"
```

### Autoscaling Behaviour

The HPA scales up aggressively (up to +2 replicas per minute, no stabilisation delay) and scales down conservatively (max −1 replica per minute, 5-minute stabilisation window) to avoid flapping under bursty traffic.

The `replicas` field is omitted from the Deployment intentionally — setting it would race with the HPA on every `helm upgrade`.

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `DATABASE_URL` | Yes | PostgreSQL write connection string (`postgres://user:pass@host/db`) |
| `DATABASE_READ_URL` | No | PostgreSQL read-replica connection string. Falls back to `DATABASE_URL` if unset. |
| `REDIS_URL` | No | Redis connection string (`redis://host:6379/0`). Omit to disable caching. |
| `TLS_CERT_FILE` | No | Path to the PEM-encoded TLS certificate. Required together with `TLS_KEY_FILE` to enable HTTPS. |
| `TLS_KEY_FILE` | No | Path to the PEM-encoded TLS private key. Required together with `TLS_CERT_FILE` to enable HTTPS. |
| `TLS_CLIENT_AUTH` | No | Set to `require` to enable mTLS client certificate verification. |
| `PERMITTED_SERVICES` | No | Comma-separated service name allowlist for `Permissions` records. Defaults to `gatekeeper,blueprints,forge`. |
| `TRUSTED_PROXY_CIDRS` | No | Comma-separated CIDRs of trusted reverse proxies for real-IP extraction. |
| `OTEL_SERVICE_NAME` | No | Service name reported in traces and metrics (default: `gatekeeper`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No | OTel Collector HTTP endpoint. Omit to disable telemetry. |
