# Minimal, forge-controlled runner image used by the forge/git-clone step (and any
# checkout-only execution that supplies no image of its own), and by the synthesised
# copy / resolve-paths / artifact executions. It ships git, an SSH client, TLS roots
# and curl — nothing else — so the common "clone a repo into a shared volume" step no
# longer re-installs git on every run.
#
# curl is here for the artifact transfer: it streams a tar straight into a PUT body
# and fails loudly on an HTTP error. busybox wget (which alpine already has) can do
# neither, so a quota rejection would look like a successful empty save.
#
# It deliberately sets NO ENTRYPOINT so forge's command (["sh","-c", ...]) runs
# directly under both runtimes: the Docker runtime only sets the container Cmd (a
# stock alpine/git with ENTRYPOINT ["git"] would run `git sh -c ...` and break),
# while the Kubernetes runtime sets command, which overrides any image entrypoint.
FROM alpine:3.21
RUN apk add --no-cache git openssh-client ca-certificates curl
