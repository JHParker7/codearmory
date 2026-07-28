# Dogfooded CI/CD, managed by the codearmory Terraform provider.
#
# git_factory (push to dev): test -> build a SHA-tagged image -> retarget builder ->
#   verify the rollout. Defined as pipeline-as-code in infra/ci/git-factory-cd.yaml.
# codearmory (push to dev): build + test the monorepo, infra/ci/codearmory-ci.yaml.
#
# The rollout IS wired now, and deliberately does not use `set-image`: git_factory is
# the one deployment builder owns, so patching the Deployment directly drifts from
# builder's desired state and is reconciled back. The pipeline tells builder instead,
# and codearmory_org_service.git_factory below is what makes `terraform apply` the
# thing that owns that service rather than whatever the API was last told.
#
# Apply:
#   export CODEARMORY_URL=http://localhost:8090  CODEARMORY_TOKEN=<token>
#   export TF_VAR_webhook_secret=<secret>
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

variable "git_factory_image_registry" {
  type        = string
  description = <<-EOT
    Registry the git_factory image is published to, WITHOUT the repository segment.
    Set per-service on builder rather than relying on the chart-wide
    BUILDER_IMAGE_REGISTRY, which points at ghcr.io — a tag composed against that
    global resolves to an image that does not exist here, and a reconcile then takes
    the service down with ErrImagePull.
  EOT
  default     = "192.168.53.171:3000/jp01"
}

variable "git_factory_image_tag" {
  type        = string
  description = <<-EOT
    Tag builder should deploy. Empty (the default) means Terraform does not pin one,
    leaving whatever the CD pipeline's retarget step last set — which is the normal
    steady state, since the pipeline deploys per-commit SHAs. Set it to roll back to
    a known build, or to pin an environment.
  EOT
  default     = ""
}

provider "codearmory" {} # endpoint/token from CODEARMORY_URL / CODEARMORY_TOKEN

# ── git_factory: builder's deployment target ─────────────────────────────────────
# git_factory is the ONE deployment builder owns (app.kubernetes.io/managed-by=
# codearmory-builder). Everything else on this cluster — events, hooks,
# outpost-gateway — is Helm-managed, so a direct `set-image` is fine for them and
# reconciled away for this one. Declaring the row here makes `terraform apply` the
# thing that updates git_factory, instead of drifting against whatever the API was
# last told.
#
# registry is pinned per-service on purpose: the chart-wide BUILDER_IMAGE_REGISTRY is
# ghcr.io/code-armory-app while these images publish to the Forgejo registry, so a tag
# composed against the global resolves to nothing and a reconcile ErrImagePulls.
#
# Ownership is split: Terraform owns that the service exists, where its
# images come from and how they are pulled; the CD pipeline owns WHICH build is live.
#
# tag is therefore null unless someone pins it. The attribute is Optional+Computed, so
# a null config means Terraform reads back whatever the pipeline last set and produces
# no diff — apply never reverts the running build. Setting var.git_factory_image_tag
# flips that: the pin becomes desired state and the next apply rolls the service back
# to it. (lifecycle.ignore_changes would NOT work here — it is unconditional, so it
# would silently make the variable do nothing.)
resource "codearmory_org_service" "git_factory" {
  service     = "codearmory_git_factory"
  enabled     = true
  registry    = var.git_factory_image_registry
  tag         = var.git_factory_image_tag != "" ? var.git_factory_image_tag : null
  pull_policy = "IfNotPresent"
}

# ── git_factory: full CD pipeline, triggered on push to dev ──────────────────────
# Pipeline-as-code, matching how the monorepo pipeline is managed: the source of
# truth is infra/ci/git-factory-cd.yaml and Terraform just deploys it. Authored as a
# file rather than inline `step` blocks because the resource has no timeout_secs
# attribute — and the run cap is load-bearing here (build-push alone may take 2400s,
# so the old 1800 killed runs mid-build).
resource "codearmory_pipeline" "git_factory" {
  definition_json = jsonencode(yamldecode(file("${path.module}/../../ci/git-factory-cd.yaml")))
  project         = "codearmory" # files it into project/codearmory so the project's admins/developers control it
}

resource "codearmory_hook_rule" "git_factory_push" {
  name = "git_factory-cd-on-dev"
  # git_factory emits EVERY repo's push under one shared source constant
  # ("codearmory_git_factory"), so source alone cannot tell the repos apart. repo_filter
  # is what scopes the rule to this one — without it a push to the monorepo would also
  # fire this pipeline, and vice versa.
  source      = "codearmory_git_factory"
  events      = ["git.push"]
  ref_filter  = "dev"
  repo_filter = "admin/codearmory-git-factory"
  workflow_id = codearmory_pipeline.git_factory.id
  secret      = var.webhook_secret # unused for internal events (HMAC-verified), required by the API

  input_mapping = {
    HOOK_REF    = "ref"
    HOOK_COMMIT = "commit"
    HOOK_REPO   = "repo"
  }
}

# ── codearmory monorepo: its full CI/CD pipeline, authored as YAML in the repo ────
# Pipeline-as-code — the source of truth is infra/ci/codearmory-ci.yaml (checkout ->
# per-module build+test -> image builds -> deploy via the outpost deploy integration,
# with persistent go/git caches and a tracking ticket). Terraform just deploys it.
resource "codearmory_pipeline" "codearmory" {
  definition_json = jsonencode(yamldecode(file("${path.module}/../../ci/codearmory-ci.yaml")))
}

resource "codearmory_hook_rule" "codearmory_push" {
  name = "codearmory-ci-on-dev"
  # Same shared git_factory source as the rule above; the two are told apart by
  # repo_filter, not by branch (both watch `dev`).
  source      = "codearmory_git_factory"
  events      = ["git.push"]
  ref_filter  = "dev"
  repo_filter = "admin/codearmory"
  workflow_id = codearmory_pipeline.codearmory.id
  secret      = var.webhook_secret

  # Feed the pipeline's declared inputs from the webhook payload.
  input_mapping = {
    HOOK_REF    = "ref"
    HOOK_COMMIT = "commit"
    HOOK_REPO   = "repo"
  }
}

output "pipelines" {
  description = "The created pipeline IDs, by repo."
  value = {
    git_factory = codearmory_pipeline.git_factory.id
    codearmory  = codearmory_pipeline.codearmory.id
  }
}
