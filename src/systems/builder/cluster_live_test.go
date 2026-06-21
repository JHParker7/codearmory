package main

import (
	"context"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestLiveReconcile exercises the k8s backend against a real cluster. It is opt-in
// (BUILDER_LIVE_TEST=1 + a reachable KUBECONFIG) and non-destructive: it deploys a
// throwaway-prefixed clone of an existing non-core service (default: tickets) — so
// it reuses that service's existing Secret/DB without touching the platform's own
// Deployment — waits for the pod to become Ready, then tears it down.
//
//	KUBECONFIG=~/talos/devops/devops BUILDER_LIVE_TEST=1 \
//	  go test -run TestLiveReconcile -v -timeout 240s ./...
func TestLiveReconcile(t *testing.T) {
	if os.Getenv("BUILDER_LIVE_TEST") != "1" {
		t.Skip("set BUILDER_LIVE_TEST=1 and KUBECONFIG to run the live reconcile test")
	}
	ns := envOrDefault("BUILDER_LIVE_NAMESPACE", "codearmory")
	svc := envOrDefault("BUILDER_LIVE_SERVICE", "tickets")
	const prefix = "codearmory-verify"

	b, err := newK8sBackend(ns, prefix, "", "")
	if err != nil {
		t.Fatalf("connect to cluster: %v", err)
	}
	ctx := context.Background()
	name := prefix + "-" + svc
	t.Cleanup(func() { _ = b.RemoveService(ctx, svc) })

	if err := b.EnsureService(ctx, workloadSpec{Service: svc}); err != nil {
		t.Fatalf("EnsureService(%s): %v", svc, err)
	}
	t.Logf("created Deployment+Service %s in %s (cloned from the platform %s)", name, ns, svc)

	// Poll for the cloned Deployment to report a ready replica.
	deadline := time.Now().Add(150 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		dep, err := b.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && dep.Status.ReadyReplicas >= 1 {
			ready = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !ready {
		t.Fatalf("Deployment %s did not become ready within the timeout", name)
	}
	t.Logf("Deployment %s is Ready", name)

	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	found := false
	for _, m := range managed {
		if m == svc {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListManaged = %v, expected to contain %q", managed, svc)
	}

	if err := b.RemoveService(ctx, svc); err != nil {
		t.Fatalf("RemoveService(%s): %v", svc, err)
	}
	if _, err := b.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Logf("note: Deployment %s still terminating (foreground delete)", name)
	}
	t.Logf("tore down %s — live reconcile verified", name)
}
