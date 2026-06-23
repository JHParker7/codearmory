package main

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The backend default is a replica FLOOR, not an exact target: an admin/HPA scale-up
// above it must survive reconcile, while an explicit per-service override is exact.
func TestApplyDeployment_ReplicaFloorPreservesExternalScaleUp(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Now())
	b.defaultReplicas = 2
	api := b.client.AppsV1().Deployments("codearmory")

	dep, _ := b.buildDeployment(ctx, workloadSpec{Service: "forge", Image: "x:1", Port: 8083})
	if err := b.applyDeployment(ctx, dep, true); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := api.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Fatalf("after create replicas = %v, want floor 2", got.Spec.Replicas)
	}

	// External scale-up to 5.
	five := int32(5)
	got.Spec.Replicas = &five
	if _, err := api.Update(ctx, got, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("scale up: %v", err)
	}

	// A floor reconcile must keep the higher count.
	dep, _ = b.buildDeployment(ctx, workloadSpec{Service: "forge", Image: "x:1", Port: 8083})
	if err := b.applyDeployment(ctx, dep, true); err != nil {
		t.Fatalf("floor reconcile: %v", err)
	}
	got, _ = api.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if *got.Spec.Replicas != 5 {
		t.Errorf("floor reconcile reverted scale-up: replicas = %d, want preserved 5", *got.Spec.Replicas)
	}

	// An explicit per-service override (floor=false) is authoritative and may lower.
	dep, _ = b.buildDeployment(ctx, workloadSpec{Service: "forge", Image: "x:1", Port: 8083, Replicas: 3})
	if err := b.applyDeployment(ctx, dep, false); err != nil {
		t.Fatalf("override: %v", err)
	}
	got, _ = api.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if *got.Spec.Replicas != 3 {
		t.Errorf("explicit override not applied exactly: replicas = %d, want 3", *got.Spec.Replicas)
	}
}

// An identical reconcile must skip the Update (spec-hash annotation), so steady-state
// passes don't churn resourceVersions / generate needless apiserver writes.
func TestApplyDeployment_SkipsNoOpUpdate(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Now())
	api := b.client.AppsV1().Deployments("codearmory")
	spec := workloadSpec{Service: "forge", Image: "x:1", Port: 8083}

	dep, _ := b.buildDeployment(ctx, spec)
	if err := b.applyDeployment(ctx, dep, true); err != nil {
		t.Fatalf("create: %v", err)
	}
	first, _ := api.Get(ctx, "codearmory-forge", metav1.GetOptions{})

	dep, _ = b.buildDeployment(ctx, spec)
	if err := b.applyDeployment(ctx, dep, true); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	second, _ := api.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("identical reconcile updated the Deployment (rv %s -> %s) — hash-skip not working",
			first.ResourceVersion, second.ResourceVersion)
	}
}

// merge prunes the keys listed in remove (so a cleared admin secret is revoked) while
// preserving unrelated keys it does not set (notably the rotating gatekeeper key).
func TestSecretMerge_PrunesRemovedKeys(t *testing.T) {
	ctx := context.Background()
	store := &k8sSecretStore{client: fake.NewSimpleClientset(), namespace: "codearmory"}

	if err := store.merge(ctx, "codearmory-blueprints", map[string]string{"x": "y"},
		map[string][]byte{"redis-url": []byte("redis://old"), "gatekeeper-service-key": []byte("k")}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := store.merge(ctx, "codearmory-blueprints", map[string]string{"x": "y"},
		map[string][]byte{"database-url": []byte("postgres://")}, []string{"redis-url"}); err != nil {
		t.Fatalf("merge: %v", err)
	}

	sec, _ := store.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-blueprints", metav1.GetOptions{})
	if _, ok := sec.Data["redis-url"]; ok {
		t.Error("redis-url not pruned — a removed secret lingers in the live Secret")
	}
	if string(sec.Data["gatekeeper-service-key"]) != "k" {
		t.Error("rotating gatekeeper-service-key was clobbered — merge must preserve unrelated keys")
	}
	if string(sec.Data["database-url"]) != "postgres://" {
		t.Error("database-url not written on merge")
	}
}
