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

func TestProvision_WritesDerivedSecrets(t *testing.T) {
	httpClient = initHTTPClient()
	enableDerivation(t)
	rec := &registerRecorder{}
	b := newTestBackend(t, rec)
	ctx := context.Background()

	// chaos declares a shared derived key (events-trigger-key).
	if err := b.provision(ctx, "chaos", "postgres://h@db/h", nil); err != nil {
		t.Fatalf("provision chaos: %v", err)
	}
	sec, err := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-chaos", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	// The shared key is 64-hex and equals the deterministic derivation (so consumers agree).
	v := string(sec.Data["events-trigger-key"])
	if raw, err := hex.DecodeString(v); err != nil || len(raw) != 32 {
		t.Errorf("events-trigger-key: not 64-hex (got %q)", v)
	}
	if v != deriveSharedKey("events-trigger-key") {
		t.Error("events-trigger-key does not match deriveSharedKey")
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

func TestTemplatePod_DeterministicEnv(t *testing.T) {
	b := &k8sBackend{prefix: "codearmory", registry: "ghcr.io/x", tag: "v1", namespace: "codearmory"}
	pt := b.templatePod(workloadSpec{Service: "chaos"})
	c := pt.Spec.Containers[0]

	if v, plain := envValue(c, "GATEKEEPER_URL"); !plain || v != "http://codearmory-gatekeeper:8081" {
		t.Errorf("GATEKEEPER_URL = %q", v)
	}
	// ${PREFIX} substituted in inter-service env.
	if v, _ := envValue(c, "EVENTS_URL"); v != "http://codearmory-events:8093" {
		t.Errorf("EVENTS_URL = %q", v)
	}
	// Derived secret wired as a secret ref to its key.
	if k := envRefKey(c, "EVENTS_TRIGGER_KEY"); k != "events-trigger-key" {
		t.Errorf("EVENTS_TRIGGER_KEY ref = %q, want events-trigger-key", k)
	}
	if k := envRefKey(c, "GATEKEEPER_SERVICE_KEY"); k != "gatekeeper-service-key" {
		t.Errorf("GATEKEEPER_SERVICE_KEY ref = %q", k)
	}
	if c.ReadinessProbe == nil || c.LivenessProbe == nil {
		t.Error("missing probes")
	}
	// Deterministic: a second render is byte-identical (no churn).
	if !reflect.DeepEqual(c.Env, b.templatePod(workloadSpec{Service: "chaos"}).Spec.Containers[0].Env) {
		t.Error("templatePod env is not deterministic")
	}
}

func TestEnsureInfra_ManagedRedisDeploysWhenNoURL(t *testing.T) {
	httpClient = initHTTPClient()
	b := newTestBackend(t, &registerRecorder{})
	ctx := context.Background()

	// blueprints declares managedRedis and is given no external REDIS_URL.
	if err := b.ensureInfra(ctx, workloadSpec{Service: "blueprints"}); err != nil {
		t.Fatalf("ensureInfra: %v", err)
	}
	dep, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-blueprints-redis", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("managed redis deployment: %v", err)
	}
	if dep.Labels[labelInfraOf] != "blueprints" {
		t.Errorf("managed redis infra-of = %q, want blueprints", dep.Labels[labelInfraOf])
	}
	// Persistence is off and it runs non-root.
	c := dep.Spec.Template.Spec.Containers[0]
	if got := c.Command; len(got) < 5 || got[1] != "--save" || got[2] != "" {
		t.Errorf("redis command = %v, want persistence disabled", got)
	}
	if sc := dep.Spec.Template.Spec.SecurityContext; sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("managed redis must run as non-root")
	}
	if _, err := b.client.CoreV1().Services("codearmory").Get(ctx, "codearmory-blueprints-redis", metav1.GetOptions{}); err != nil {
		t.Errorf("managed redis service: %v", err)
	}

	// ListManaged must NOT return the redis (it is infra, not a top-level service).
	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	for _, s := range managed {
		if s == "blueprints-redis" {
			t.Error("ListManaged returned managed redis (would be reclaimed as an orphan)")
		}
	}

	// Teardown removes it with the parent.
	b.teardownInfra(ctx, "blueprints")
	if _, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-blueprints-redis", metav1.GetOptions{}); err == nil {
		t.Error("managed redis deployment not torn down")
	}
}

func TestEnsureInfra_ManagedRedisSkippedAndCleanedWhenURLSupplied(t *testing.T) {
	httpClient = initHTTPClient()
	b := newTestBackend(t, &registerRecorder{})
	ctx := context.Background()

	// First pass with no URL deploys the managed redis.
	if err := b.ensureInfra(ctx, workloadSpec{Service: "blueprints"}); err != nil {
		t.Fatalf("ensureInfra (managed): %v", err)
	}
	if _, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-blueprints-redis", metav1.GetOptions{}); err != nil {
		t.Fatalf("managed redis should exist: %v", err)
	}
	// Supplying an external REDIS_URL on a later reconcile tears the builder one down.
	spec := workloadSpec{Service: "blueprints", Secrets: map[string]string{"REDIS_URL": "redis://external:6379"}}
	if err := b.ensureInfra(ctx, spec); err != nil {
		t.Fatalf("ensureInfra (external): %v", err)
	}
	if _, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-blueprints-redis", metav1.GetOptions{}); err == nil {
		t.Error("managed redis not cleaned up after external REDIS_URL supplied")
	}
}

func TestEnsureServiceSecret_ManagedRedisURLWired(t *testing.T) {
	httpClient = initHTTPClient()
	b := newTestBackend(t, &registerRecorder{})
	ctx := context.Background()

	// No admin REDIS_URL: the Secret gets the in-cluster URL under redis-url, and the
	// pod wires REDIS_URL as a secret ref to it.
	if err := b.provision(ctx, "blueprints", "postgres://b@db/b", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	sec, err := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-blueprints", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got := string(sec.Data["redis-url"]); got != "redis://codearmory-blueprints-redis:6379" {
		t.Errorf("redis-url = %q, want the managed in-cluster URL", got)
	}
	c := b.templatePod(workloadSpec{Service: "blueprints"}).Spec.Containers[0]
	if k := envRefKey(c, "REDIS_URL"); k != "redis-url" {
		t.Errorf("REDIS_URL ref = %q, want redis-url", k)
	}

	// Admin-supplied REDIS_URL wins over the managed value (no override to in-cluster).
	if err := b.provision(ctx, "blueprints", "postgres://b@db/b", map[string]string{"REDIS_URL": "redis://external:6379"}); err != nil {
		t.Fatalf("provision (external): %v", err)
	}
	sec, _ = b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-blueprints", metav1.GetOptions{})
	if got := string(sec.Data["redis-url"]); got != "redis://external:6379" {
		t.Errorf("redis-url = %q, want the admin-supplied external URL", got)
	}
}

// Note: builder's underscore→hyphen k8s naming (registryName "gitea_integration" →
// object "gitea-integration") was exercised here against gitea_integration. That service
// is now core (Helm-deployed), so builder no longer manages it; no remaining catalog def
// has an underscore name. The k8sName-vs-registryName invariant for defs is still guarded
// by TestEmbeddedDefs_K8sNamesAreDNS1123 in embed_test.go.
