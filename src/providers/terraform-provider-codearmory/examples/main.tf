terraform {
  required_providers {
    codearmory = {
      source = "code-armory-app/codearmory"
    }
  }
}

# Endpoint + credentials can also come from CODEARMORY_URL / CODEARMORY_CLIENT_ID /
# CODEARMORY_CLIENT_SECRET (or CODEARMORY_TOKEN).
provider "codearmory" {
  endpoint      = "http://localhost:8080"
  client_id     = var.client_id
  client_secret = var.client_secret
}

variable "client_id" { type = string }
variable "client_secret" {
  type      = string
  sensitive = true
}

resource "codearmory_runner_class" "ci" {
  name           = "ci-large"
  memory_mb      = 2048
  cpu_millicores = 2000
  pids_limit     = 256
  tmpfs_mb       = 512
  enabled        = true
}

data "codearmory_runner_class" "standard" {
  name = "standard"
}

resource "codearmory_event_trigger" "on_push" {
  name = "build-on-push"

  match = jsonencode({
    all = [
      { field = "type", op = "eq", value = "repo.push" },
      { field = "subject", op = "eq", value = "myorg/myrepo" },
      { field = "data.ref", op = "eq", value = "main" },
    ]
  })

  actions = jsonencode([{
    kind = "run_pipeline"
    config = {
      pipeline_id = "00000000-0000-0000-0000-000000000000" # a pipeline ID in your org
      inputs = {
        BRANCH = "{{ data.ref }}"
        SHA    = "{{ data.commit }}"
      }
    }
  }])
}

output "standard_runner_memory" {
  value = data.codearmory_runner_class.standard.memory_mb
}
