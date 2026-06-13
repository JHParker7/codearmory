# terraform-provider-codearmory

An OpenTofu / Terraform provider that configures a **codearmory** platform through its
Conductor gateway. One binary serves both OpenTofu and Terraform (they share the
plugin protocol). No backend changes are required — the provider talks to the
existing API.

## Status

Minimal scaffold. Implemented:

| Type | Name | Backend route (via Conductor) |
|------|------|-------------------------------|
| resource | `codearmory_runner_class` | `/forge/runner-classes` |
| resource | `codearmory_hook_rule` | `/hooks/rules` |
| data source | `codearmory_runner_class` | `/forge/runner-classes/{name}` |

## Authentication

The provider authenticates either with a pre-issued bearer token or via the OAuth
`client_credentials` grant (recommended for automation):

```hcl
provider "codearmory" {
  endpoint      = "https://conductor.example.com" # or CODEARMORY_URL
  client_id     = "..."                           # or CODEARMORY_CLIENT_ID
  client_secret = "..."                           # or CODEARMORY_CLIENT_SECRET
  # token       = "..."                           # or CODEARMORY_TOKEN (takes precedence)
}
```

### Bootstrapping the OAuth client (one-time, operator)

The provider's own client cannot be created through the provider (chicken-and-egg).
An operator registers it once via gatekeeper's **internal** endpoint, which is
service-key gated and not exposed through Conductor:

```
POST  /internal/oauth/clients      (X-Service-Key: gatekeeper:<service-key>)
```

Assign that client a role whose permissions cover the actions you'll manage,
scoped to your org — client tokens are authorized as `org/<orgname>/<resource>`
(e.g. `org/acme/forge/runner-classes`, or `*`).

## Build & local install

```bash
cd src/providers/terraform-provider-codearmory
go mod tidy
go build -o terraform-provider-codearmory

# Point OpenTofu/Terraform at the local binary via dev_overrides.
# ~/.tofurc  (or ~/.terraformrc for Terraform)
cat > ~/.tofurc <<EOF
provider_installation {
  dev_overrides {
    "code-armory-app/codearmory" = "$(pwd)"
  }
  direct {}
}
EOF
```

With `dev_overrides` you skip `init`; just run `tofu plan` / `tofu apply` in the
`examples/` directory.

## Notes & limitations

- **Write-only secrets.** `codearmory_hook_rule.secret` is never returned by the
  API. The configured value is kept in state but drift on it cannot be detected,
  and `import` cannot recover it (set it in config; the next apply re-sends it).
- **`workflow_id` validation.** The hooks service verifies the referenced pipeline
  exists and belongs to the caller's org, so use a real pipeline ID.
- **RBAC scoping.** A provider principal can only manage resources its role grants.
  Runner classes are global; managing them needs an `org/<orgname>/forge/runner-classes`
  (or `*`) grant on the client's role.
- **Eventual consistency.** Conductor refreshes its route table from the registry
  every ~5 min and gatekeeper caches permissions (~5 min TTL); writes invalidate
  the cache, so intra-apply ordering via `depends_on` is normally sufficient.

## Extending

Add a resource by implementing `resource.Resource` (+ `Configure` and
`ImportState`) in `internal/provider/`, then register its constructor in
`provider.go`'s `Resources()`. The HTTP plumbing is in `client.go`.
Candidates with clean CRUD: `gatekeeper` orgs/teams/roles/permissions/secrets,
`workflows` pipelines/steps, `tickets` field-defs, `gitea_integration` repos.
