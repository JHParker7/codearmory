# git_connector backend for git_factory, so forge CI runners can clone git_factory repos
# (private → they need a brokered credential). git_factory validates a bearer token, which
# it accepts as Basic-auth (username "token", password = the token).
#
# NOTE: the password should be a DEDICATED, durable git_factory access token, not a personal
# session token (which expires). Supply it out-of-band:
#   export TF_VAR_git_factory_token="<git_factory access token>"

variable "git_factory_token" {
  type        = string
  description = "A git_factory access token used as the clone credential (Basic-auth password)."
  sensitive   = true
}

resource "codearmory_git_backend" "git_factory" {
  name     = "git-factory"
  type     = "generic"
  base_url = "http://ca-codearmory-git-factory:9002"
  auth = {
    mode     = "basic"
    username = "token"
    password = var.git_factory_token
  }
}
