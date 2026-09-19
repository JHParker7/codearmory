#!/usr/bin/env bash
# Build and push every codearmory service image to a registry.
#
#   NS=docker.io/jhp73 TAG=alpha-0.1.0 ./scripts/publish-images.sh
#
# Every service Dockerfile lives in src/systems/<svc>/ but must be built with
# src/systems as the CONTEXT: each go.mod replaces the shared SDK with ../sdk,
# so the build needs both trees. portal-bff additionally needs portal/ for the
# SPA sources — the same context covers it.
#
# Images are x86-only. Override PLATFORMS if you need another architecture;
# building for more than one keeps buildx from loading the result locally, so
# PUSH=0 only works with a single platform.
set -euo pipefail

NS="${NS:?set NS to your registry namespace, e.g. docker.io/jhp73}"
TAG="${TAG:-alpha-0.1.0}"
MOVING_TAG="${MOVING_TAG:-alpha-latest}"
PLATFORMS="${PLATFORMS:-linux/amd64}"
PUSH="${PUSH:-1}"

# "<image-repo>:<dockerfile-dir>" — the two differ only for portal, which is
# built from portal-bff/Dockerfile but published under the name the Helm chart
# expects (values.yaml: portal.image.repository).
SERVICES=(
  gatekeeper:gatekeeper
  registry:registry
  conductor:conductor
  portal:portal-bff
  forge:forge
  egress-proxy:egress-proxy
  workflows:workflows
  artifacts:artifacts
  tickets:tickets
  events:events
  outpost-gateway:outpost-gateway
  git:git
  git-factory:git-factory
  containers:containers
  builder:builder
  outpost:outpost
)

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/src/systems"

output_args=(--load)
if [ "$PUSH" = "1" ]; then
  output_args=(--push)
  registry_host="${NS%%/*}"
  if ! grep -q "$registry_host" "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null; then
    echo "warning: no stored credentials for $registry_host — run: docker login $registry_host" >&2
  fi
fi

for pair in "${SERVICES[@]}"; do
  repo="${pair%%:*}"
  dir="${pair##*:}"
  echo "==> ${NS}/${repo}:${TAG}"
  docker buildx build \
    --platform "$PLATFORMS" \
    -f "${dir}/Dockerfile" \
    -t "${NS}/${repo}:${TAG}" \
    -t "${NS}/${repo}:${MOVING_TAG}" \
    "${output_args[@]}" \
    .
done

# runner-git is not a service: it is the minimal git image forge runs for checkout-only
# steps, and forge composes its reference from the SAME registry prefix as the chart
# (FORGE_GIT_IMAGE = <imageRegistry>/runner-git:<forge tag>). Publishing it here is what
# lets the chart leave forge.gitImage empty — omit it and every clone step fails to pull.
# Its Dockerfile is standalone, so it builds from its own directory as the context.
echo "==> ${NS}/runner-git:${TAG}"
docker buildx build \
  --platform "$PLATFORMS" \
  -t "${NS}/runner-git:${TAG}" \
  -t "${NS}/runner-git:${MOVING_TAG}" \
  "${output_args[@]}" \
  "$ROOT/infra/runner-images/git"

echo "done: $((${#SERVICES[@]} + 1)) images at ${NS}/*:${TAG}"
