# Egress Proxy

Allowlist-enforcing HTTP CONNECT proxy for Forge execution containers. Provides controlled outbound internet access for CI/CD workloads (package downloads, module fetches, release uploads) while preventing containers from reaching internal services or arbitrary internet hosts.

## How it works

```
Forge execution container
  │
  └── HTTP_PROXY=http://egress-proxy:3128
        │
        ▼
   Egress Proxy :3128
        │  1. Receive HTTP CONNECT tunnel request
        │  2. Extract target hostname
        │  3. Check hostname against PROXY_ALLOWED_DOMAINS allowlist
        │     - Exact match: "github.com"
        │     - Wildcard prefix: "*.github.com" (matches any subdomain)
        │  4. Allowed → open TCP tunnel to target
        │     Denied  → 403 Forbidden
        │
        ▼
   Target host (e.g. registry.terraform.io)
```

The proxy speaks standard HTTP CONNECT tunnelling. Any HTTP client that respects `HTTP_PROXY`/`HTTPS_PROXY` environment variables works without code changes.

## Requirements

- Go 1.25+
- Network access to the internet from the host running the proxy

## Configuration

| Variable | Default | Description |
|---|---|---|
| `PROXY_ALLOWED_DOMAINS` | — | Comma-separated list of permitted hostnames. Each entry is either an exact hostname (`github.com`) or a wildcard prefix (`*.github.com`). When empty, all CONNECT requests are denied. |
| `PORT` | `3128` | Port the proxy listens on. |

## Running locally

```bash
cd src/systems/egress-proxy
PROXY_ALLOWED_DOMAINS="github.com,*.github.com,registry.npmjs.org,pypi.org" \
  go run .
```

## Docker

```bash
cd src/systems/egress-proxy
docker build -t egress-proxy:latest .

docker run -p 3128:3128 \
  -e PROXY_ALLOWED_DOMAINS="github.com,*.github.com,registry.npmjs.org,pypi.org,files.pythonhosted.org,proxy.golang.org,sum.golang.org" \
  egress-proxy:latest
```

## Integration with Forge

The egress proxy is designed to be used alongside Forge. The recommended setup:

1. Create an internal Docker network that Forge execution containers are placed on:
   ```bash
   docker network create --internal forge-exec
   ```
   The `--internal` flag prevents containers from directly routing to the internet.

2. Connect the egress proxy to both `forge-exec` and an internet-facing network so it can relay approved traffic.

3. Configure Forge:
   ```
   FORGE_NETWORK_MODE=forge-exec
   FORGE_EGRESS_PROXY=http://egress-proxy:3128
   ```

Forge injects `HTTP_PROXY` and `HTTPS_PROXY` into every execution container automatically when `FORGE_EGRESS_PROXY` is set, so tools like `tofu`, `npm`, `pip`, and `curl` route through the proxy without per-execution configuration.

The `infra/local/compose.yml` ships a ready-to-use configuration of this pattern.

## Default allowed domains

The local compose stack configures these domains by default (set via `FORGE_PROXY_ALLOWED_DOMAINS`):

| Domain | Purpose |
|--------|---------|
| `registry.terraform.io` | OpenTofu/Terraform module registry |
| `releases.hashicorp.com` | Provider binary downloads |
| `github.com`, `*.github.com` | GitHub source and releases |
| `raw.githubusercontent.com`, `objects.githubusercontent.com` | GitHub raw content |
| `registry.npmjs.org` | npm packages |
| `pypi.org`, `files.pythonhosted.org` | Python packages |
| `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com` | Go modules |

Adjust `PROXY_ALLOWED_DOMAINS` to match your workload's requirements.

## Allowlist syntax

| Pattern | Matches |
|---------|---------|
| `github.com` | `github.com` only |
| `*.github.com` | `api.github.com`, `uploads.github.com`, etc. — any subdomain |
| `*.github.com` | Does **not** match `github.com` itself — add both if needed |

## Security model

- The proxy only implements HTTP CONNECT tunnelling — it cannot inspect TLS traffic.
- Connections to disallowed hosts are refused with `403 Forbidden` before any data is exchanged.
- **A dial-time IP guard runs on every request, whatever the allowlist says.** The hostname check and the address check are separate: a name that passes the allowlist (including the default `*`) is still refused if it *resolves* to an address the proxy must not reach. That covers what Go's own `net.IP` predicates already know — loopback, RFC1918 private, link-local (incl. the `169.254.169.254` cloud-metadata address), multicast, unspecified — plus four ranges they miss and a cluster genuinely lands in:
  - `100.64.0.0/10` (CGNAT) — where EKS, GKE and OKE commonly place the pod/node network, and where Alibaba Cloud serves instance metadata (`100.100.100.200`).
  - `0.0.0.0/8` ("this network") — non-routable, and treated as the local host by many stacks even though only `0.0.0.0` itself is `IsUnspecified`.
  - `240.0.0.0/4` (reserved/future use), which includes the `255.255.255.255` broadcast address.
  - `fec0::/10`, deprecated IPv6 site-local. (Its replacement, unique-local `fc00::/7`, is already covered by `IsPrivate`.)
- **The IPv4 ranges cannot be dodged by writing the address as IPv6.** An IPv4-mapped form like `::ffff:10.0.0.1` is normalised to its 4-byte address before any of the checks run.
- **NAT64 addresses are judged by the IPv4 they embed, not blocked outright.** An address in the well-known `64:ff9b::/96` prefix (RFC 6052) carries an IPv4 address in its low 32 bits, and an IPv6-only cluster reaches the public IPv4 internet through exactly that prefix. The proxy extracts the embedded address and applies the same rules to it — so NAT64 to a public host works, and NAT64 to `10.0.0.1` does not.
- The proxy does not authenticate clients. It relies on network-level isolation (execution containers on an `--internal` Docker network) to restrict which processes can reach it.
- In Kubernetes, use a `NetworkPolicy` to restrict egress from Forge job pods to only the egress proxy pod.

## Testing

```bash
cd src/systems/egress-proxy
go test ./...
```
