package main

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// When the Helm chart already deployed a service (forge/workflows), builder must manage
// its config but never touch the k8s workload: EnsureService skips it, leaving the
// chart's Deployment untouched and creating no builder-owned Service.
func TestEnsureService_SkipsHelmOwnedWorkload(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Now())
	deps := b.client.AppsV1().Deployments("codearmory")

	// The chart's forge Deployment: owned by Helm, with a distinctive image we can check
	// builder never overwrites.
	helmDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "codearmory-forge",
			Namespace: "codearmory",
			Labels:    map[string]string{labelManagedBy: "Helm"},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "forge", Image: "forge:helm"}},
				},
			},
		},
	}
	if _, err := deps.Create(ctx, helmDep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed helm deployment: %v", err)
	}

	// Builder is asked to ensure forge with a different image — it must NOT apply it.
	if err := b.EnsureService(ctx, workloadSpec{Service: "forge", Image: "forge:builder", Port: 8083}); err != nil {
		t.Fatalf("EnsureService: %v", err)
	}

	got, err := deps.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.Labels[labelManagedBy] != "Helm" {
		t.Errorf("deployment re-stamped managed-by=%q, want Helm", got.Labels[labelManagedBy])
	}
	if img := got.Spec.Template.Spec.Containers[0].Image; img != "forge:helm" {
		t.Errorf("deployment image = %q, want forge:helm (builder overwrote a chart-owned workload)", img)
	}

	// No builder-owned Service should have been created for an externally-managed workload.
	if _, err := b.client.CoreV1().Services("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("builder created a Service for a Helm-owned workload (err=%v)", err)
	}
}
