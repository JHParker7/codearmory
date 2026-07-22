package main

import (
	"context"
	"testing"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseReplicas(t *testing.T) {
	cases := []struct {
		in   any
		want int32
	}{
		{float64(3), 3},
		{"2", 2},
		{2, 2},
		{float64(-1), 0},
		{float64(0), 0},
		{"nope", 0},
		{float64(500), 100}, // capped
		{nil, 0},
	}
	for _, c := range cases {
		if got := parseReplicas(c.in); got != c.want {
			t.Errorf("parseReplicas(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseRotateInterval(t *testing.T) {
	cases := []struct {
		in   any
		want time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"1h", time.Hour},
		{float64(45), 45 * time.Minute}, // number => minutes
		{15, 15 * time.Minute},
		{"45", 45 * time.Minute},           // bare numeric string => minutes (env parity)
		{float64(1e12), maxRotateInterval}, // would overflow int64 ns => clamped
		{"100000h", maxRotateInterval},     // huge but valid duration => clamped
		{"garbage", 0},
		{float64(0), 0},
		{float64(-5), 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := parseRotateInterval(c.in); got != c.want {
			t.Errorf("parseRotateInterval(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExtractDeployKnobs(t *testing.T) {
	replicas, rotate, rest := extractDeployKnobs(map[string]any{
		cfgReplicas:       float64(2),
		cfgRotateInterval: "30m",
		"ALLOWED_IMAGES":  "alpine:3.19",
	})
	if replicas != 2 {
		t.Errorf("replicas = %d, want 2", replicas)
	}
	if rotate != 30*time.Minute {
		t.Errorf("rotate = %v, want 30m", rotate)
	}
	// Reserved knobs must be stripped so they never leak in as container env.
	if _, ok := rest[cfgReplicas]; ok {
		t.Error("replicas knob leaked into env config")
	}
	if _, ok := rest[cfgRotateInterval]; ok {
		t.Error("rotateInterval knob leaked into env config")
	}
	if rest["ALLOWED_IMAGES"] != "alpine:3.19" {
		t.Errorf("rest = %v, want ALLOWED_IMAGES preserved", rest)
	}
}

func TestSpecFromRow_DeployKnobs(t *testing.T) {
	spec := specFromRow(OrgService{
		ServiceName: "forge",
		Config: map[string]any{
			cfgReplicas:       float64(3),
			cfgRotateInterval: "30m",
			"ALLOWED_IMAGES":  "alpine:3.19",
		},
	})
	if spec.Replicas != 3 || spec.RotateInterval != 30*time.Minute {
		t.Fatalf("knobs not parsed: replicas=%d rotate=%v", spec.Replicas, spec.RotateInterval)
	}
	if spec.Env["ALLOWED_IMAGES"] != "alpine:3.19" {
		t.Errorf("env = %v", spec.Env)
	}
	if _, ok := spec.Env[cfgReplicas]; ok {
		t.Error("replicas knob should not be a container env var")
	}
}

// fixedBackend builds a backend over an empty fake cluster with a fixed clock.
func fixedBackend(now time.Time) *k8sBackend {
	return &k8sBackend{
		client:    fake.NewSimpleClientset(),
		namespace: "codearmory",
		prefix:    "codearmory",
		nowFn:     func() time.Time { return now },
	}
}

func TestBuildDeployment_StrategyAndReplicas(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC))

	// Default: no override, no backend default => replicas 1, but always the
	// surge-first strategy and minReadySeconds.
	dep, err := b.buildDeployment(ctx, workloadSpec{Service: "forge", Image: "x:1", Port: 8083})
	if err != nil {
		t.Fatalf("buildDeployment: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", dep.Spec.Replicas)
	}
	if dep.Spec.MinReadySeconds != minReadySeconds {
		t.Errorf("minReadySeconds = %d, want %d", dep.Spec.MinReadySeconds, minReadySeconds)
	}
	ru := dep.Spec.Strategy.RollingUpdate
	if dep.Spec.Strategy.Type != "RollingUpdate" || ru == nil {
		t.Fatalf("strategy = %+v, want RollingUpdate", dep.Spec.Strategy)
	}
	if ru.MaxUnavailable.IntValue() != 0 || ru.MaxSurge.IntValue() != 1 {
		t.Errorf("maxUnavailable=%v maxSurge=%v, want 0/1", ru.MaxUnavailable, ru.MaxSurge)
	}
	// Rotation off by default => no annotation churn.
	if _, ok := dep.Spec.Template.Annotations[annotationRotatedAt]; ok {
		t.Error("rotation annotation set with rotation disabled")
	}

	// Backend default applies when there is no per-service override.
	b.defaultReplicas = 2
	dep, _ = b.buildDeployment(ctx, workloadSpec{Service: "forge", Image: "x:1", Port: 8083})
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want backend default 2", *dep.Spec.Replicas)
	}

	// Per-service override wins over the backend default.
	dep, _ = b.buildDeployment(ctx, workloadSpec{Service: "forge", Image: "x:1", Port: 8083, Replicas: 4})
	if *dep.Spec.Replicas != 4 {
		t.Errorf("replicas = %d, want override 4", *dep.Spec.Replicas)
	}
}

func TestBuildDeployment_RotationBucket(t *testing.T) {
	ctx := context.Background()
	spec := workloadSpec{Service: "forge", Image: "x:1", Port: 8083, RotateInterval: 30 * time.Minute}

	// 12:17 truncates to the 12:00 bucket.
	b := fixedBackend(time.Date(2026, 6, 22, 12, 17, 0, 0, time.UTC))
	d1, _ := b.buildDeployment(ctx, spec)
	if got := d1.Spec.Template.Annotations[annotationRotatedAt]; got != "2026-06-22T12:00:00Z" {
		t.Fatalf("bucket = %q, want 12:00", got)
	}

	// 12:29 — same window => identical value (a reconcile here is a no-op, no churn).
	b = fixedBackend(time.Date(2026, 6, 22, 12, 29, 0, 0, time.UTC))
	d2, _ := b.buildDeployment(ctx, spec)
	if d2.Spec.Template.Annotations[annotationRotatedAt] != d1.Spec.Template.Annotations[annotationRotatedAt] {
		t.Error("annotation changed within the same window — would churn pods every reconcile")
	}

	// 12:31 — crossed the boundary => new value rolls the pods.
	b = fixedBackend(time.Date(2026, 6, 22, 12, 31, 0, 0, time.UTC))
	d3, _ := b.buildDeployment(ctx, spec)
	if got := d3.Spec.Template.Annotations[annotationRotatedAt]; got != "2026-06-22T12:30:00Z" {
		t.Errorf("bucket = %q, want 12:30 after boundary", got)
	}
}

func TestApplyPDB(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Now())
	api := b.client.PolicyV1().PodDisruptionBudgets("codearmory")

	// replicas >= 2 => PDB with minAvailable = replicas-1, labelled ours.
	if err := b.applyPDB(ctx, "forge", 3); err != nil {
		t.Fatalf("applyPDB: %v", err)
	}
	pdb, err := api.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pdb: %v", err)
	}
	if pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Errorf("minAvailable = %v, want 2", pdb.Spec.MinAvailable)
	}
	if pdb.Labels[labelManagedBy] != managedByValue {
		t.Errorf("pdb not labelled managed-by builder: %v", pdb.Labels)
	}

	// Scaling to a single replica removes the PDB (it would wedge every drain).
	if err := b.applyPDB(ctx, "forge", 1); err != nil {
		t.Fatalf("applyPDB scale-down: %v", err)
	}
	if _, err := api.Get(ctx, "codearmory-forge", metav1.GetOptions{}); err == nil {
		t.Error("expected single-replica PDB to be removed")
	}
}

func TestApplyPDB_UpdatesExisting(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Now())
	api := b.client.PolicyV1().PodDisruptionBudgets("codearmory")

	// First apply creates the PDB at minAvailable = 2 (replicas 3).
	if err := b.applyPDB(ctx, "forge", 3); err != nil {
		t.Fatalf("applyPDB create: %v", err)
	}
	// Re-applying at a higher replica count must update the existing object in place
	// (the Get-then-Update branch), not error or duplicate.
	if err := b.applyPDB(ctx, "forge", 5); err != nil {
		t.Fatalf("applyPDB update: %v", err)
	}
	pdb, err := api.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pdb: %v", err)
	}
	if pdb.Spec.MinAvailable.IntValue() != 4 {
		t.Errorf("minAvailable = %v, want 4 after update", pdb.Spec.MinAvailable)
	}
}

func TestApplyPDB_RefusesForeign(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Now())
	api := b.client.PolicyV1().PodDisruptionBudgets("codearmory")

	// A PDB someone else owns (e.g. the Helm chart) must never be hijacked.
	minAvail := intstr.FromInt(1)
	foreign := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "codearmory-forge",
			Namespace: "codearmory",
			Labels:    map[string]string{labelManagedBy: "Helm"},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvail},
	}
	if _, err := api.Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed foreign pdb: %v", err)
	}
	if err := b.applyPDB(ctx, "forge", 3); err == nil {
		t.Error("expected applyPDB to refuse overwriting a foreign-owned PDB")
	}
	// The foreign PDB must be left untouched.
	got, err := api.Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pdb: %v", err)
	}
	if got.Spec.MinAvailable.IntValue() != 1 || got.Labels[labelManagedBy] != "Helm" {
		t.Errorf("foreign PDB was modified: minAvailable=%v labels=%v", got.Spec.MinAvailable, got.Labels)
	}
}

func TestRotationBucket_NonDivisorInterval(t *testing.T) {
	interval := 7 * time.Hour
	// 7h buckets anchored to UTC midnight: 00:00, 07:00, 14:00, 21:00, then reset.
	cases := []struct {
		now    time.Time
		bucket time.Time
	}{
		{time.Date(2026, 6, 22, 6, 59, 0, 0, time.UTC), time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 6, 22, 7, 1, 0, 0, time.UTC), time.Date(2026, 6, 22, 7, 0, 0, 0, time.UTC)},
		{time.Date(2026, 6, 22, 21, 30, 0, 0, time.UTC), time.Date(2026, 6, 22, 21, 0, 0, 0, time.UTC)},
		{time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)}, // resets at midnight
	}
	for _, c := range cases {
		if got := rotationBucket(c.now, interval); !got.Equal(c.bucket) {
			t.Errorf("rotationBucket(%v, 7h) = %v, want %v", c.now, got, c.bucket)
		}
	}
}

func TestEnsureService_CreatesPDBAndStrategy(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(time.Now())
	b.defaultReplicas = 2 // provisioning off (prov.enabled false) => EnsureService skips it

	if err := b.EnsureService(ctx, workloadSpec{Service: "forge", Image: "x:1", Port: 8083}); err != nil {
		t.Fatalf("EnsureService: %v", err)
	}
	dep, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if *dep.Spec.Replicas != 2 || dep.Spec.Strategy.RollingUpdate == nil {
		t.Errorf("deployment missing replica floor/strategy: %+v", dep.Spec)
	}
	pdb, err := b.client.PolicyV1().PodDisruptionBudgets("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PDB created by EnsureService: %v", err)
	}
	if pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %v, want 1 (replicas-1)", pdb.Spec.MinAvailable)
	}

	// Teardown removes the PDB alongside the Deployment/Service.
	if err := b.RemoveService(ctx, "forge"); err != nil {
		t.Fatalf("RemoveService: %v", err)
	}
	if _, err := b.client.PolicyV1().PodDisruptionBudgets("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{}); err == nil {
		t.Error("expected PDB deleted on teardown")
	}
}
