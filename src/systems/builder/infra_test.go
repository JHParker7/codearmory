package main

import (
	"context"
	"encoding/hex"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func enableDerivation(t *testing.T) {
	t.Helper()
	resetDerivation()
	t.Setenv("BUILDER_SECRETS_KEY", testSecretsKey)
	initSecretDerivation()
	t.Cleanup(resetDerivation)
}

// envValue / envRefKey extract a plain value / secret-ref key for an env var.
func envValue(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, e.ValueFrom == nil
		}
	}
	return "", false
}

func envRefKey(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return e.ValueFrom.SecretKeyRef.Key
		}
	}
	return ""
}

func TestProvision_WritesDerivedAndRegistrySecrets(t *testing.T) {
	httpClient = initHTTPClient()
	enableDerivation(t)
	rec := &registerRecorder{}
	b := newTestBackend(t, rec)
	ctx := context.Background()

	if err := b.provision(ctx, "workflows", "postgres://wf@db/wf", nil); err != nil {
		t.Fatalf("provision workflows: %v", err)
	}
	sec, err := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-workflows", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	// Shared + private derived keys, and the registry read-account key (workflows pulls
	// GET /actions). All are 64-hex.
	for _, key := range []string{"hooks-trigger-key", "token-key", "registry-service-key"} {
		v := string(sec.Data[key])
		if raw, err := hex.DecodeString(v); err != nil || len(raw) != 32 {
			t.Errorf("%s: not 64-hex (got %q)", key, v)
		}
	}
	// The shared key must equal the deterministic derivation (so other consumers agree).
	if string(sec.Data["hooks-trigger-key"]) != deriveSharedKey("hooks-trigger-key") {
		t.Error("hooks-trigger-key does not match deriveSharedKey")
	}

	// blueprints gets its private encryption-key.
	if err := b.provision(ctx, "blueprints", "", nil); err != nil {
		t.Fatalf("provision blueprints: %v", err)
	}
	bp, _ := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-blueprints", metav1.GetOptions{})
	if raw, err := hex.DecodeString(string(bp.Data["encryption-key"])); err != nil || len(raw) != 32 {
		t.Errorf("blueprints encryption-key not 64-hex: %q", bp.Data["encryption-key"])
	}
}

func TestTemplatePod_DeterministicEnvAndForgeSA(t *testing.T) {
	b := &k8sBackend{prefix: "codearmory", registry: "ghcr.io/x", tag: "v1", namespace: "codearmory"}
	pt := b.templatePod(workloadSpec{Service: "forge"})
	c := pt.Spec.Containers[0]

	if v, plain := envValue(c, "GATEKEEPER_URL"); !plain || v != "http://codearmory-gatekeeper:8081" {
		t.Errorf("GATEKEEPER_URL = %q", v)
	}
	// ${PREFIX} substituted in inter-service env.
	if v, _ := envValue(c, "FORGE_EGRESS_PROXY"); v != "http://codearmory-egress-proxy:3128" {
		t.Errorf("FORGE_EGRESS_PROXY = %q", v)
	}
	if v, _ := envValue(c, "RUNTIME"); v != "kubernetes" {
		t.Errorf("RUNTIME = %q", v)
	}
	// Derived secret wired as a secret ref to its key.
	if k := envRefKey(c, "GITEA_INTERNAL_KEY"); k != "gitea-internal-key" {
		t.Errorf("GITEA_INTERNAL_KEY ref = %q, want gitea-internal-key", k)
	}
	if k := envRefKey(c, "GATEKEEPER_SERVICE_KEY"); k != "gatekeeper-service-key" {
		t.Errorf("GATEKEEPER_SERVICE_KEY ref = %q", k)
	}
	// forge runs under its own ServiceAccount.
	if pt.Spec.ServiceAccountName != "codearmory-forge" {
		t.Errorf("serviceAccountName = %q, want codearmory-forge", pt.Spec.ServiceAccountName)
	}
	if c.ReadinessProbe == nil || c.LivenessProbe == nil {
		t.Error("missing probes")
	}
	// Deterministic: a second render is byte-identical (no churn).
	if !reflect.DeepEqual(c.Env, b.templatePod(workloadSpec{Service: "forge"}).Spec.Containers[0].Env) {
		t.Error("templatePod env is not deterministic")
	}
}

func TestTemplatePod_GiteaUsesK8sName(t *testing.T) {
	b := &k8sBackend{prefix: "codearmory", registry: "ghcr.io/x", tag: "v1", namespace: "codearmory"}
	// Secret ref must target the DNS-1123 object name, not the underscore registry name.
	for _, e := range b.templatePod(workloadSpec{Service: "gitea_integration"}).Spec.Containers[0].Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			if got := e.ValueFrom.SecretKeyRef.Name; got != "codearmory-gitea-integration" {
				t.Fatalf("secret ref name = %q, want codearmory-gitea-integration", got)
			}
		}
	}
}

func TestEnsureInfra_ForgeDeploysEgressProxyAndRBAC(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &registerRecorder{}
	b := newTestBackend(t, rec)
	ctx := context.Background()

	if err := b.ensureInfra(ctx, workloadSpec{Service: "forge"}); err != nil {
		t.Fatalf("ensureInfra: %v", err)
	}
	// egress-proxy Deployment + Service exist and the Deployment is labelled infra-of=forge.
	dep, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-egress-proxy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("egress-proxy deployment: %v", err)
	}
	if dep.Labels[labelInfraOf] != "forge" {
		t.Errorf("egress-proxy infra-of = %q, want forge", dep.Labels[labelInfraOf])
	}
	if _, err := b.client.CoreV1().Services("codearmory").Get(ctx, "codearmory-egress-proxy", metav1.GetOptions{}); err != nil {
		t.Errorf("egress-proxy service: %v", err)
	}
	// forge RBAC: SA + Role + RoleBinding in the release (= default exec) namespace.
	if _, err := b.client.CoreV1().ServiceAccounts("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{}); err != nil {
		t.Errorf("forge serviceaccount: %v", err)
	}
	if _, err := b.client.RbacV1().Roles("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{}); err != nil {
		t.Errorf("forge role: %v", err)
	}
	if _, err := b.client.RbacV1().RoleBindings("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{}); err != nil {
		t.Errorf("forge rolebinding: %v", err)
	}

	// ListManaged must NOT return the egress-proxy (it is infra, not a top-level service).
	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	for _, s := range managed {
		if s == egressProxyComponent {
			t.Error("ListManaged returned egress-proxy (would be reclaimed as an orphan)")
		}
	}

	// Teardown removes the infra.
	b.teardownInfra(ctx, "forge")
	if _, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-egress-proxy", metav1.GetOptions{}); err == nil {
		t.Error("egress-proxy deployment not torn down")
	}
}

func TestEnsureService_GiteaNamedWithHyphen(t *testing.T) {
	httpClient = initHTTPClient()
	rec := &registerRecorder{}
	b := newTestBackend(t, rec)
	ctx := context.Background()

	if err := b.EnsureService(ctx, workloadSpec{Service: "gitea_integration"}); err != nil {
		t.Fatalf("EnsureService gitea: %v", err)
	}
	// Object name is the DNS-1123 k8sName, never the underscore registry name.
	if _, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-gitea-integration", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected deployment codearmory-gitea-integration: %v", err)
	}
	// Labelled by the registry name (for the desired-state diff).
	dep, _ := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-gitea-integration", metav1.GetOptions{})
	if dep.Labels[labelComponent] != "gitea_integration" {
		t.Errorf("component label = %q, want gitea_integration", dep.Labels[labelComponent])
	}
}
