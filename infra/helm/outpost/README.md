# CodeArmory Outpost chart

The **outpost** is the single customer-deployed agent that is the *sole*
in-cluster actuator/collector for CodeArmory cluster integrations (chaos, argo).
It dials out to the control-plane **outpost-gateway** over HTTPS — long-polling
for commands and POSTing events. **No inbound access to your cluster is
required.** Self-hosted and SaaS use the identical mechanism; the only
difference is whether the gateway is in the same cluster or reached over a
private link.

## Install

1. In the CodeArmory portal, add an outpost and choose its modules. Copy the
   single-use **enrollment token**.
2. Install:

   ```bash
   helm install my-outpost infra/helm/outpost \
     --namespace codearmory-outpost --create-namespace \
     --set controlPlaneURL=https://gateway.example.com \
     --set modules="chaos\,argo" \
     --set enrollmentToken=<token>
   ```

On first start the outpost exchanges the enrollment token for a long-lived key
and persists it to a small PVC (`persistence.enabled`), so restarts do not need
a fresh (single-use) token.

## Modules and RBAC

RBAC is granted per enabled module, least-privilege:

| Module | Cluster access |
|--------|----------------|
| `chaos` | `litmuschaos.io` ChaosEngines/Experiments/Results (create/delete/watch). Plus a runner ServiceAccount (`chaos.runnerServiceAccount`, default `litmus-admin`) + Role in each `chaos.targetNamespaces`, used by the Litmus operator to execute experiment pods. |
| `argo`  | `argoproj.io` Applications (get/list/watch/patch) in `argo.namespace`. |

The control plane holds **zero** cluster credentials — everything cluster-facing
is in this outpost.

## Litmus operator (chaos)

The chaos module creates `litmuschaos.io/v1alpha1` ChaosEngines, which require
the Litmus **chaos-operator** and its CRDs to be installed in-cluster. Litmus
3.x's Helm chart only ships ChaosCenter and dropped the operator subchart, so the
operator is vendored upstream at
`codearmory_saas/infra/litmus/operator/litmus-operator-v3.29.0.yaml`. Apply it
before installing this chart (or set `chaos.installOperator` workflows in your
GitOps):

```bash
kubectl apply -f https://litmuschaos.github.io/litmus/litmus-operator-v3.29.0.yaml
```

This chart intentionally does not embed the ~200KB upstream bundle.

## Key values

| Key | Default | Notes |
|-----|---------|-------|
| `controlPlaneURL` | `""` (required) | outpost-gateway base URL reachable from this cluster |
| `modules` | `chaos` | csv of enabled modules |
| `enrollmentToken` | `""` | single-use token from the portal |
| `existingSecret` | `""` | Secret with `enrollment-token` and/or `outpost-id`/`outpost-key` |
| `persistence.enabled` | `true` | persist the outpost identity/key across restarts |
| `chaos.targetNamespaces` | `[default]` | namespaces where experiments run |
| `argo.namespace` | `argocd` | where Argo CD Applications live |
