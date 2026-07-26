# Dogfood example: manage the platform's own repos and CI with the codearmory provider.
#
# For each of the two repos we host — the git plane (git_factory) and the core
# monorepo (codearmory) — this creates:
#   * a hosted git repository (codearmory_git_repository),
#   * a build-and-test pipeline (codearmory_pipeline), and
#   * a push webhook rule that triggers the pipeline on main (codearmory_hook_rule).
#
# Apply with:
#   export CODEARMORY_URL="http://localhost:8080"
#   export CODEARMORY_TOKEN="<a bearer token>"
#   terraform apply -var 'webhook_secret=<shared secret>'

terraform {
  required_providers {
    codearmory = {
      source = "code-armory-app/codearmory"
    }
  }
}

variable "endpoint" {
  type        = string
  description = "Conductor gateway URL. Falls back to CODEARMORY_URL."
  default     = "http://localhost:8080"
}

variable "token" {
  type        = string
  description = "Bearer token. Falls back to CODEARMORY_TOKEN."
  sensitive   = true
  default     = ""
}

variable "webhook_secret" {
  type        = string
  description = "Shared secret the git host signs push webhooks with."
  sensitive   = true
}

provider "codearmory" {
  endpoint = var.endpoint
  token    = var.token
}

# The two repos we dogfood, each with the command that builds and tests it.
locals {
  repos = {
    codearmory_git_factory = {
      description = "codearmory git plane (repos + Smart-HTTP)"
      image       = "golang:1.26"
      run         = "cd src/control_plane && go build ./... && go test ./..."
    }
    codearmory = {
      description = "codearmory core control-plane monorepo"
      image       = "golang:1.26"
      run         = "cd src/systems && go test ./... && cd ../cli && go build ./..."
    }
  }
}

# 1. The hosted repositories.
resource "codearmory_git_repository" "repo" {
  for_each    = local.repos
  name        = each.key
  description = each.value.description
  visibility  = "private"
}

# 2. A build+test pipeline per repo — a single forge/run step for now.
resource "codearmory_pipeline" "ci" {
  for_each    = local.repos
  name        = "${each.key}-ci"
  description = "Build & test ${each.key}"

  step = [
    {
      name    = "build-test"
      action  = "forge/run"
      timeout = 1800
      with = {
        image = each.value.image
        run   = each.value.run
      }
    }
  ]
}

# 3. Trigger each pipeline on a push to main of its repo.
resource "codearmory_hook_rule" "on_push" {
  for_each    = local.repos
  name        = "${each.key}-build-on-push"
  source      = "${codearmory_git_repository.repo[each.key].namespace}/${codearmory_git_repository.repo[each.key].name}"
  events      = ["push"]
  ref_filter  = "refs/heads/main"
  workflow_id = codearmory_pipeline.ci[each.key].id
  secret      = var.webhook_secret

  input_mapping = {
    BRANCH = "ref"
    SHA    = "commit"
  }
}

output "clone_urls" {
  description = "The Smart-HTTP clone/push URL for each dogfooded repo."
  value       = { for k, r in codearmory_git_repository.repo : k => r.http_url }
}
