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
  name = "git-factory"
  type = "generic"
  # The ingress URL, not the in-cluster service address. git_connector resolves a
  # brokered clone URL against this base and hands it to a forge sandbox, and a sandbox
  # under the kata egress policy can reach public IPs but no private range — so a
  # ClusterIP base produces a credential for an address the runner can never connect
  # to. The failure is a silent 134s connect timeout, not an auth error, which reads
  # like a broken credential rather than a network policy.
  #
  # /git is the ingress path prefix for git_factory on this host; the rewrite strips it
  # before the service sees the request.
  base_url = "https://exp.codearmory.app/git"
  auth = {
    mode     = "basic"
    username = "token"
    password = var.git_factory_token
  }
}
