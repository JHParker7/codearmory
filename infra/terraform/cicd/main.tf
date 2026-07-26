# Dogfooded CI/CD, managed by the codearmory Terraform provider.
#
# git_factory (push to dev): a full pipeline over a shared workspace volume —
#   create-volume -> git-clone(dev) -> go test -> build & push image (Kaniko).
# codearmory (push to main): build + test the monorepo.
#
# The rollout (kubectl set image in minikube) is NOT wired yet: forge runs under the
# kata sandbox whose egress NetworkPolicy blocks the private kube API. Loosen egress
# for CI first (see infra/terraform/cicd/README or the egress patch), then add a
# deploy step. Likewise the CI registry (192.168.53.171:3000) is a private IP the
# sandbox blocks until allowlisted, and forge/build-image's push needs it reachable.
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

variable "ci_image" {
  type        = string
  description = "Go toolchain image for test steps (must be in forge's ALLOWED_IMAGES)."
  default     = "golang:1.25"
}

variable "ci_registry" {
  type        = string
  description = "Image repo the build step pushes to (tag appended per pipeline)."
  default     = "192.168.53.171:3000/jp01/git-factory"
}

variable "registry_secret_name" {
  type        = string
  description = "Name of an existing gatekeeper secret holding a docker config.json for the image push (REGISTRY_AUTH). Leave empty for an anonymous/insecure registry. (Creating the secret needs the createSecret permission, so it is referenced, not managed here.)"
  default     = ""
}

variable "outpost_id" {
  type        = string
  description = "The outpost (with the deploy integration) that rolls out the new image into the cluster. Same outpost the monorepo pipeline uses."
  default     = "b38de6c5-6393-4dd2-aa93-b1ec5cf1bb86"
}

variable "git_factory_deployment" {
  type        = string
  description = "The k8s deployment the git_factory image rolls out to."
  default     = "ca-codearmory-git-factory"
}

provider "codearmory" {} # endpoint/token from CODEARMORY_URL / CODEARMORY_TOKEN

locals {
  # One shared run-scoped volume attach spec, reused by every step in the git_factory run.
  workspace = { workflow_id = "$${run_id}", name = "workspace", mount_path = "/workspace", workdir = true }
  # REGISTRY_AUTH secret_ref, only when a registry secret is supplied.
  registry_secret_refs = var.registry_secret_name != "" ? { REGISTRY_AUTH = "secret:${var.registry_secret_name}" } : {}
}

# ── git_factory: full CD pipeline, triggered on push to dev ──────────────────────
resource "codearmory_pipeline" "git_factory" {
  name        = "git_factory-cd"
  description = "Test, build and push the git_factory image on push to dev"

  step = [
    {
      name      = "workspace"
      action    = "forge/create-volume"
      with_json = jsonencode({ workflow_id = "$${run_id}", name = "workspace", mount_path = "/workspace", medium = "disk", size_mb = 4096 })
    },
    {
      name    = "checkout"
      action  = "forge/git-clone"
      with_json = jsonencode({
        volumes     = [local.workspace]
        secret_refs = { GIT_CLONE_URL = "git:jhparker7/codearmory_git_factory" }
        checkout    = { ref = "dev" }
      })
    },
    {
      name      = "test"
      action    = "forge/run"
      timeout   = 1800
      with      = { image = var.ci_image }
      with_json = jsonencode({
        run     = "cd src/control_plane && go build ./... && go test ./..."
        volumes = [local.workspace]
      })
    },
    {
      name    = "build-push"
      action  = "forge/build-image"
      timeout = 2400
      with_json = jsonencode(merge({
        build        = { context = "/workspace/src/control_plane", dockerfile = "Dockerfile", destinations = ["${var.ci_registry}:dev"] }
        volumes      = [{ workflow_id = "$${run_id}", name = "workspace", mount_path = "/workspace" }]
        runner_class = "ci"
      }, length(local.registry_secret_refs) > 0 ? { secret_refs = local.registry_secret_refs } : {}))
    },
    {
      # Roll the freshly-pushed image into the cluster via the outpost deploy integration —
      # the platform-correct path (the outpost actuates the cluster; a sandboxed runner can't
      # reach the kube API). Mirrors the monorepo pipeline's redeploy step.
      name    = "deploy"
      action  = "outpost-gateway/enqueueCommand"
      timeout = 300
      with_json = jsonencode({
        id          = var.outpost_id
        integration = "deploy"
        type        = "set-image"
        payload = {
          deployment     = var.git_factory_deployment
          image          = "${var.ci_registry}:dev"
          ignore_missing = true
        }
      })
    },
  ]
}

resource "codearmory_hook_rule" "git_factory_push" {
  name = "git_factory-cd-on-dev"
  # git_factory emits every repo's push to hooks' /internal/events with a SHARED
  # source constant ("codearmory_git_factory") and event "git.push"; the ref is the
  # short branch name. Repos are NOT distinguishable by source — only by ref_filter —
  # so this fires on any git_factory repo pushed to `dev`.
  source      = "codearmory_git_factory"
  events      = ["git.push"]
  ref_filter  = "dev"
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
  name = "codearmory-ci-on-main"
  # Same shared git_factory source; distinguished from the git_factory-cd rule only by
  # the branch. NOTE: the pipeline's checkout clones the Gitea copy
  # (192.168.53.171:3000/jp01/codearmory) — if the monorepo's real pushes land there
  # rather than on git_factory, wire the Gitea webhook path instead of this rule.
  source      = "codearmory_git_factory"
  events      = ["git.push"]
  ref_filter  = "main"
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
