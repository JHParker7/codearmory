#!/usr/bin/env bash
# Bring up JuiceFS shared storage for git_factory on a local minikube, and move the
# repo store onto it (ARCHITECTURE §5 Step 2). Idempotent — safe to re-run.
#
#   ./setup.sh              # full bring-up, migrating any existing repo store
#   SKIP_MIGRATE=1 ./setup.sh   # fresh install: no old store to copy
#
# See README.md for what each piece is and how to verify it afterwards.
set -euo pipefail

NS_JFS=juicefs
NS_APP=${NS_APP:-gitfactory}
CSI_VERSION=0.32.0
IMAGE_TAG=${IMAGE_TAG:-git-factory:juicefs}
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Every image the node needs. The minikube node has IP connectivity but cannot
# resolve registry hosts, so nothing here can be pulled in-cluster: each image is
# pulled on the HOST and side-loaded, and every manifest/chart value pins
# imagePullPolicy to IfNotPresent (or Never) to match.
IMAGES=(
  redis:7-alpine
  minio/minio:RELEASE.2025-04-22T22-12-26Z
  juicedata/juicefs-csi-driver:v${CSI_VERSION}
  juicedata/mount:ce-v1.3.0
  registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.9.0
  registry.k8s.io/sig-storage/livenessprobe:v2.12.0
  registry.k8s.io/sig-storage/csi-resizer:v1.9.0
)

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

say "Side-loading images into minikube"
for img in "${IMAGES[@]}"; do
  if minikube image ls | grep -q "^\(docker.io/\)\?${img}$"; then
    echo "  present: $img"
  else
    echo "  loading: $img"
    docker pull "$img" >/dev/null
    minikube image load "$img"
  fi
done

say "Building and loading git-factory ($IMAGE_TAG)"
docker build -f "$HERE/../../../src/systems/git-factory/Dockerfile" \
  -t "$IMAGE_TAG" "$HERE/../../../src/systems" >/dev/null
minikube image load "$IMAGE_TAG"

say "Metadata engine (Redis + Sentinel) + object store"
kubectl apply -f "$HERE/00-namespace.yaml" -f "$HERE/10-meta-redis.yaml" -f "$HERE/20-minio.yaml"
kubectl rollout status statefulset/juicefs-meta -n "$NS_JFS" --timeout=180s
kubectl rollout status deploy/juicefs-sentinel -n "$NS_JFS" --timeout=180s
kubectl rollout status deploy/juicefs-minio -n "$NS_JFS" --timeout=180s

# Sentinel has to have agreed on a primary before the CSI driver formats against
# it, or the format fails on a master name that resolves to nothing yet. This also
# catches the common misconfiguration early, where Sentinel is up but monitoring a
# host that never becomes reachable.
say "Waiting for Sentinel to agree on a primary"
for i in $(seq 1 30); do
  master=$(kubectl exec -n "$NS_JFS" deploy/juicefs-sentinel -- \
             redis-cli -p 26379 sentinel get-master-addr-by-name jfs-meta 2>/dev/null | head -1 || true)
  if [[ -n "$master" ]]; then
    echo "  primary: $master"
    break
  fi
  [[ $i == 30 ]] && { echo "Sentinel never reported a primary for jfs-meta" >&2; exit 1; }
  sleep 2
done

say "JuiceFS CSI driver"
helm repo add juicefs https://juicedata.github.io/charts/ >/dev/null 2>&1 || true
helm repo update juicefs >/dev/null
helm upgrade --install juicefs-csi-driver juicefs/juicefs-csi-driver \
  --namespace "$NS_JFS" --version "$CSI_VERSION" \
  -f "$HERE/csi-driver-values.yaml" --wait --timeout 5m

say "Filesystem, StorageClass and the ReadWriteMany claim"
# The CSI controller formats the volume the first time it provisions against the
# secret — there is no separate `juicefs format` step.
kubectl apply -f "$HERE/30-juicefs-fs.yaml"
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/git-factory-repos -n "$NS_APP" --timeout=180s

if [[ "${SKIP_MIGRATE:-0}" != "1" ]]; then
  say "Migrating the existing repo store onto JuiceFS"
  # Quiesce first: repo paths derive from the row id, so a copy taken while a push
  # is in flight can capture a half-written ref.
  if kubectl get deploy/git-factory -n "$NS_APP" >/dev/null 2>&1; then
    kubectl scale deploy/git-factory -n "$NS_APP" --replicas=0
    kubectl wait --for=delete pod -l app=git-factory -n "$NS_APP" --timeout=120s || true
  fi
  kubectl delete job/git-factory-repos-migrate -n "$NS_APP" --ignore-not-found
  kubectl apply -f "$HERE/40-migrate-job.yaml"
  kubectl wait --for=condition=complete job/git-factory-repos-migrate -n "$NS_APP" --timeout=600s
  kubectl logs job/git-factory-repos-migrate -n "$NS_APP" | tail -2
fi

say "git_factory on shared storage, 2 replicas"
kubectl apply -f "$HERE/50-git-factory.yaml"
kubectl rollout status deploy/git-factory -n "$NS_APP" --timeout=300s

say "Done"
kubectl get pods -n "$NS_APP" -l app=git-factory
echo
echo "Verify the cross-pod half (what the in-process preflight cannot prove):"
echo "  ./verify.sh"
