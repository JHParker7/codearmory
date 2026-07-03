# Minimal, forge-controlled git runner image used by the forge/git-clone step (and
# any checkout-only execution that supplies no image of its own). It ships git, an
# SSH client, and TLS roots — nothing else — so the common "clone a repo into a
# shared volume" step no longer re-installs git on every run.
#
# It deliberately sets NO ENTRYPOINT so forge's command (["sh","-c", ...]) runs
# directly under both runtimes: the Docker runtime only sets the container Cmd (a
# stock alpine/git with ENTRYPOINT ["git"] would run `git sh -c ...` and break),
# while the Kubernetes runtime sets command, which overrides any image entrypoint.
FROM alpine:3.21
RUN apk add --no-cache git openssh-client ca-certificates
