# Dogfooded CI: build+test pipelines and push triggers for the platform's own repos,
# managed by the codearmory Terraform provider.
#
# The repos themselves already exist (they are not managed here — the hook only needs
# their namespace/name), so this config provisions, per repo:
#   * a build-and-test pipeline (codearmory_pipeline), and
#   * a push webhook rule that triggers it on main (codearmory_hook_rule).
#
# Apply:
#   export CODEARMORY_URL=http://localhost:8090
#   export CODEARMORY_TOKEN=<bearer token>
#   export TF_VAR_webhook_secret=<shared secret>
#   TF_CLI_CONFIG_FILE=<dev.tfrc> terraform apply

terraform {
  required_providers {
    codearmory = {
      source = "code-armory-app/codearmory"
    }
  }
}

variable "webhook_secret" {
  type        = string
  description = "Shared secret the git host signs push webhooks with."
  sensitive   = true
}

provider "codearmory" {} # endpoint/token from CODEARMORY_URL / CODEARMORY_TOKEN

locals {
  ci_image = "golang:1.26"

  # Existing repos (namespace/name) and the command that builds + tests each.
  repos = {
    codearmory_git_factory = {
      source = "jhparker7/codearmory_git_factory"
      run    = "cd src/control_plane && go build ./... && go test ./..."
    }
    codearmory = {
      source = "jhparker7/codearmory"
      run    = "cd src/systems && go test ./... && cd ../cli && go build ./..."
    }
  }
}

# A build+test pipeline per repo.
resource "codearmory_pipeline" "ci" {
  for_each    = local.repos
  name        = "${each.key}-ci"
  description = "Build & test ${each.key} on push to main"

  step = [
    {
      name    = "build-test"
      action  = "forge/run"
      timeout = 1800
      with = {
        image = local.ci_image
        run   = each.value.run
      }
    }
  ]
}

# Trigger each pipeline on a push to main of its repo.
resource "codearmory_hook_rule" "on_push" {
  for_each    = local.repos
  name        = "${each.key}-build-on-push"
  source      = each.value.source
  events      = ["push"]
  ref_filter  = "refs/heads/main"
  workflow_id = codearmory_pipeline.ci[each.key].id
  secret      = var.webhook_secret

  input_mapping = {
    BRANCH = "ref"
    SHA    = "commit"
  }
}

output "pipelines" {
  description = "The created CI pipeline IDs, by repo."
  value       = { for k, p in codearmory_pipeline.ci : k => p.id }
}
