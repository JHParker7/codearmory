package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var rolloutNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

func ptr32(v int32) *int32 { return &v }

// dep builds a Deployment with `want` replicas and a status describing how far the
// rollout got. generation/observed default to converged bookkeeping.
func dep(want, updated, available, unavailable int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "codearmory-blueprints",
			Namespace:  "codearmory",
			Generation: 3,
			Labels:     map[string]string{labelManagedBy: managedByValue, labelComponent: "blueprints"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr32(want),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelComponent: "blueprints"}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  3,
			UpdatedReplicas:     updated,
			AvailableReplicas:   available,
			UnavailableReplicas: unavailable,
		},
	}
}

// pullingPod is a pod whose container is stuck on an image-pull reason.
func pullingPod(name, reason, image string, age time.Duration) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "codearmory",
			Labels:            map[string]string{labelComponent: "blueprints"},
			CreationTimestamp: metav1.NewTime(rolloutNow.Add(-age)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "blueprints",
				Image: image,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  reason,
					Message: "failed to resolve reference",
				}},
			}},
		},
	}
}

func healthyPod(name string, age time.Duration) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "codearmory",
			Labels:            map[string]string{labelComponent: "blueprints"},
			CreationTimestamp: metav1.NewTime(rolloutNow.Add(-age)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestEvaluateRollout_ConvergedIsOK(t *testing.T) {
	st := evaluateRollout(dep(2, 2, 2, 0), nil, rolloutNow, time.Minute)
	if st.Status != rolloutOK {
		t.Fatalf("status = %q (%s), want ok", st.Status, st.Message)
	}
	if st.failed() {
		t.Error("converged rollout reported as failed")
	}
}

// A status that still describes the PREVIOUS generation must never read as converged:
// right after builder applies a bad image the counts still look perfect, and trusting
// them would declare the workload healthy for exactly as long as it takes the
// controller to notice — the window the whole check exists to cover.
func TestEvaluateRollout_StaleStatusIsNotConverged(t *testing.T) {
	d := dep(2, 2, 2, 0)
	d.Status.ObservedGeneration = 2 // controller has not seen generation 3 yet
	st := evaluateRollout(d, nil, rolloutNow, time.Minute)
	if st.Status != rolloutProgressing {
		t.Fatalf("status = %q, want progressing on a stale status", st.Status)
	}
}

func TestEvaluateRollout_ImagePullPastDeadlineFails(t *testing.T) {
	pods := []corev1.Pod{
		healthyPod("blueprints-old", 4*time.Hour),
		pullingPod("blueprints-new", "ImagePullBackOff", "ghcr.io/code-armory-app/blueprints:dev", 20*time.Minute),
	}
	st := evaluateRollout(dep(1, 1, 1, 1), pods, rolloutNow, 5*time.Minute)
	if st.Status != rolloutFailed {
		t.Fatalf("status = %q, want failed", st.Status)
	}
	if st.Reason != "ImagePullBackOff" {
		t.Errorf("reason = %q", st.Reason)
	}
	// The image is the actionable part: the operator needs to see WHICH reference is
	// unpullable, since the composed one may never appear in any config they wrote.
	if !strings.Contains(st.Message, "ghcr.io/code-armory-app/blueprints:dev") {
		t.Errorf("message %q does not name the image", st.Message)
	}
	if !st.Since.Equal(rolloutNow.Add(-20 * time.Minute)) {
		t.Errorf("since = %v, want the stuck pod's creation time", st.Since)
	}
}

// Inside the deadline the same evidence is only "progressing": a slow deploy must not
// page anyone, or the failed state stops meaning anything.
func TestEvaluateRollout_ImagePullInsideDeadlineIsProgressing(t *testing.T) {
	pods := []corev1.Pod{pullingPod("blueprints-new", "ErrImagePull", "repo/x:1", time.Minute)}
	st := evaluateRollout(dep(1, 1, 1, 1), pods, rolloutNow, 5*time.Minute)
	if st.Status != rolloutProgressing {
		t.Fatalf("status = %q, want progressing", st.Status)
	}
	if st.Reason != "ErrImagePull" {
		t.Errorf("reason = %q, want the reason carried through", st.Reason)
	}
}

func TestEvaluateRollout_UnschedulablePastDeadlineFails(t *testing.T) {
	pod := healthyPod("blueprints-new", 30*time.Minute)
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  corev1.PodReasonUnschedulable,
		Message: "0/3 nodes are available: insufficient memory",
	}}
	st := evaluateRollout(dep(1, 1, 1, 1), []corev1.Pod{pod}, rolloutNow, 5*time.Minute)
	if st.Status != rolloutFailed || st.Reason != reasonUnschedulable {
		t.Fatalf("state = %+v, want failed/Unschedulable", st)
	}
	if !strings.Contains(st.Message, "insufficient memory") {
		t.Errorf("message %q drops the scheduler's explanation", st.Message)
	}
}

// A crash loop is the application's problem, not an unresolvable image, and it has a
// different fix — the image demonstrably pulled. Classifying it here would fire this
// alert for something it cannot say anything useful about.
func TestEvaluateRollout_CrashLoopIsNotAPullFailure(t *testing.T) {
	pod := pullingPod("blueprints-new", "CrashLoopBackOff", "repo/x:1", time.Hour)
	st := evaluateRollout(dep(1, 1, 1, 1), []corev1.Pod{pod}, rolloutNow, 5*time.Minute)
	if st.Status != rolloutProgressing || st.Reason != reasonProgressing {
		t.Fatalf("state = %+v, want progressing (not a rollout failure)", st)
	}
}

// Likewise an image that is simply still downloading.
func TestEvaluateRollout_ContainerCreatingIsNotStuck(t *testing.T) {
	pod := pullingPod("blueprints-new", "ContainerCreating", "repo/x:1", time.Hour)
	st := evaluateRollout(dep(1, 1, 1, 1), []corev1.Pod{pod}, rolloutNow, 5*time.Minute)
	if st.Status != rolloutProgressing || st.Reason != reasonProgressing {
		t.Fatalf("state = %+v, want progressing", st)
	}
}

func TestEvaluateRollout_InitContainerCounts(t *testing.T) {
	pod := healthyPod("blueprints-new", time.Hour)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  "init",
		Image: "repo/init:bad",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "InvalidImageName"}},
	}}
	st := evaluateRollout(dep(1, 1, 1, 1), []corev1.Pod{pod}, rolloutNow, 5*time.Minute)
	if st.Status != rolloutFailed || st.Reason != "InvalidImageName" {
		t.Fatalf("state = %+v, want failed on the init container", st)
	}
}

// A pod already being deleted is not evidence of a wedged rollout — it is the rollout
// working. Counting it would keep a service "failed" after the fix landed.
func TestEvaluateRollout_TerminatingPodIgnored(t *testing.T) {
	pod := pullingPod("blueprints-old", "ImagePullBackOff", "repo/x:1", time.Hour)
	now := metav1.NewTime(rolloutNow)
	pod.DeletionTimestamp = &now
	st := evaluateRollout(dep(1, 1, 1, 1), []corev1.Pod{pod}, rolloutNow, 5*time.Minute)
	if st.Status != rolloutProgressing {
		t.Fatalf("state = %+v, want progressing (terminating pod ignored)", st)
	}
}

// The oldest stuck pod wins so the reported age dates the problem, and so repeated
// passes report the same pod instead of flapping between equally-stuck ones.
func TestEvaluateRollout_OldestStuckPodWins(t *testing.T) {
	pods := []corev1.Pod{
		pullingPod("new", "ImagePullBackOff", "repo/x:2", 2*time.Minute),
		pullingPod("old", "ImagePullBackOff", "repo/x:1", 30*time.Minute),
	}
	st := evaluateRollout(dep(2, 2, 0, 2), pods, rolloutNow, 5*time.Minute)
	if !strings.Contains(st.Message, "repo/x:1") {
		t.Fatalf("message = %q, want the oldest stuck pod", st.Message)
	}
	if st.Status != rolloutFailed {
		t.Errorf("status = %q, want failed", st.Status)
	}
}

func TestRolloutState_ReadsClusterAndRespectsOwnership(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(rolloutNow)

	// Nothing deployed => nothing to report.
	st, err := b.RolloutState(ctx, "blueprints", 5*time.Minute)
	if err != nil || st.Status != rolloutUnmanaged {
		t.Fatalf("absent deployment: (%+v, %v), want unmanaged", st, err)
	}

	d := dep(1, 1, 0, 1)
	if _, err := b.client.AppsV1().Deployments("codearmory").Create(ctx, d, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	pod := pullingPod("codearmory-blueprints-abc", "ImagePullBackOff", "ghcr.io/code-armory-app/blueprints:dev", time.Hour)
	if _, err := b.client.CoreV1().Pods("codearmory").Create(ctx, &pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	st, err = b.RolloutState(ctx, "blueprints", 5*time.Minute)
	if err != nil {
		t.Fatalf("RolloutState: %v", err)
	}
	if st.Status != rolloutFailed || st.Reason != "ImagePullBackOff" {
		t.Fatalf("state = %+v, want failed/ImagePullBackOff", st)
	}

	// A Helm-owned workload is not builder's to speak for.
	d2 := d.DeepCopy()
	d2.Labels[labelManagedBy] = "Helm"
	if _, err := b.client.AppsV1().Deployments("codearmory").Update(ctx, d2, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update deployment: %v", err)
	}
	if st, err := b.RolloutState(ctx, "blueprints", 5*time.Minute); err != nil || st.Status != rolloutUnmanaged {
		t.Fatalf("helm-owned: (%+v, %v), want unmanaged", st, err)
	}
}

// The blast radius of a bad desired image rests entirely on the surge-first rollout:
// maxUnavailable=0 means the old, healthy pod is not retired until its replacement is
// Ready, so an unpullable image wedges the rollout instead of taking the service down.
// applyDeployment replaces the whole spec, which is what makes that hold on an EXISTING
// workload — including one drifted (or created by an older builder) onto a strategy that
// terminates first. Assert the reconcile restores it rather than adopting what it found.
func TestApplyDeployment_RestoresSurgeFirstStrategyOnDrift(t *testing.T) {
	ctx := context.Background()
	b := fixedBackend(rolloutNow)

	if err := b.EnsureService(ctx, workloadSpec{Service: "forge", Image: "good:1", Port: 8083}); err != nil {
		t.Fatalf("EnsureService: %v", err)
	}
	// Drift: something rewrote the strategy to one that takes the old pod down first.
	drifted, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	drifted.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	if _, err := b.client.AppsV1().Deployments("codearmory").Update(ctx, drifted, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Reconcile with a NEW (and, as it happens, unpullable) image — the git_factory shape.
	if err := b.EnsureService(ctx, workloadSpec{Service: "forge", Image: "ghcr.io/code-armory-app/forge:does-not-exist", Port: 8083}); err != nil {
		t.Fatalf("EnsureService: %v", err)
	}
	got, err := b.client.AppsV1().Deployments("codearmory").Get(ctx, "codearmory-forge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	ru := got.Spec.Strategy.RollingUpdate
	if got.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil {
		t.Fatalf("strategy = %+v, want the surge-first RollingUpdate restored", got.Spec.Strategy)
	}
	if ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("maxUnavailable = %v, want 0 — a healthy pod must not be retired for an unproven image", ru.MaxUnavailable)
	}
	if ru.MaxSurge.IntValue() != 1 {
		t.Errorf("maxSurge = %v, want 1", ru.MaxSurge)
	}
}

func TestRolloutDeadline(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultRolloutDeadline},
		{"90s", 90 * time.Second},
		{"garbage", defaultRolloutDeadline},
		{"0", 0},
		{"-1m", 0},
	}
	for _, c := range cases {
		t.Setenv("BUILDER_ROLLOUT_DEADLINE", c.env)
		if got := rolloutDeadline(); got != c.want {
			t.Errorf("rolloutDeadline(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// A wedged workload must make the reconcile pass fail and land on the service's row.
// The failure mode this guards against is not that the rollout breaks — it is that
// nothing anywhere says it did.
func TestApplyDesired_WedgedRolloutIsReportedAndFailsThePass(t *testing.T) {
	fb := &fakeBackend{
		managed: []string{"blueprints", "hooks"},
		rollouts: map[string]rolloutState{
			"blueprints": {Status: rolloutFailed, Reason: "ImagePullBackOff", Message: "image ghcr.io/x/blueprints:dev"},
			"hooks":      {Status: rolloutOK},
		},
	}
	rec, written := newTestReconciler(fb)
	desired := map[string]workloadSpec{
		"blueprints": {Service: "blueprints"},
		"hooks":      {Service: "hooks"},
	}

	err := rec.applyDesired(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "blueprints") {
		t.Fatalf("applyDesired err = %v, want it to name the wedged service", err)
	}
	if got := written["blueprints"]; len(got) != 1 || got[0].Status != rolloutFailed {
		t.Fatalf("blueprints status writes = %+v, want one failed", got)
	}
	if got := written["hooks"]; len(got) != 1 || got[0].Status != rolloutOK {
		t.Fatalf("hooks status writes = %+v, want one ok", got)
	}

	// A steady state is not rewritten on every 30s pass.
	if err := rec.applyDesired(context.Background(), desired); err == nil {
		t.Fatal("second pass should still report the wedged rollout")
	}
	if got := written["blueprints"]; len(got) != 1 {
		t.Errorf("unchanged status written %d times, want 1", len(got))
	}

	// Recovery is written through.
	fb.rollouts["blueprints"] = rolloutState{Status: rolloutOK}
	if err := rec.applyDesired(context.Background(), desired); err != nil {
		t.Fatalf("recovered pass: %v", err)
	}
	if got := written["blueprints"]; len(got) != 2 || got[1].Status != rolloutOK {
		t.Errorf("recovery writes = %+v, want a trailing ok", got)
	}
}

// A real apply failure is the more useful error, so it must not be replaced by the
// rollout observation that follows it.
func TestApplyDesired_EnsureErrorWinsOverRolloutError(t *testing.T) {
	fb := &fakeBackend{
		ensureErr: errors.New("boom"),
		rollouts:  map[string]rolloutState{"blueprints": {Status: rolloutFailed, Reason: "ImagePullBackOff"}},
	}
	rec, _ := newTestReconciler(fb)
	err := rec.applyDesired(context.Background(), map[string]workloadSpec{"blueprints": {Service: "blueprints"}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the ensure error", err)
	}
}

// Teardown clears the status so a stale "failed" does not outlive the workload.
func TestApplyDesired_TeardownClearsRolloutStatus(t *testing.T) {
	fb := &fakeBackend{managed: []string{"blueprints"}}
	rec, written := newTestReconciler(fb)
	if err := rec.applyDesired(context.Background(), map[string]workloadSpec{}); err != nil {
		t.Fatalf("applyDesired: %v", err)
	}
	if got := written["blueprints"]; len(got) != 1 || got[0].Status != rolloutUnmanaged {
		t.Fatalf("teardown writes = %+v, want one cleared status", got)
	}
}

// The escape hatch: deadline <= 0 turns observation off entirely, including the extra
// apiserver reads.
func TestCheckRollouts_DisabledByDeadline(t *testing.T) {
	fb := &fakeBackend{rollouts: map[string]rolloutState{"blueprints": {Status: rolloutFailed}}}
	rec, written := newTestReconciler(fb)
	rec.rolloutDeadline = 0
	if err := rec.checkRollouts(context.Background(), map[string]workloadSpec{"blueprints": {Service: "blueprints"}}); err != nil {
		t.Fatalf("checkRollouts: %v", err)
	}
	if len(fb.checked) != 0 {
		t.Errorf("backend queried %v with observation disabled", fb.checked)
	}
	if len(written) != 0 {
		t.Errorf("status written %v with observation disabled", written)
	}
}

// A cluster read that fails must not fail the reconcile: it is an observation, and a
// transient apiserver error is not evidence that anything is wrong with the workload.
func TestCheckRollouts_ReadErrorDoesNotFailThePass(t *testing.T) {
	fb := &fakeBackend{rolloutErr: errors.New("apiserver unavailable")}
	rec, written := newTestReconciler(fb)
	if err := rec.checkRollouts(context.Background(), map[string]workloadSpec{"blueprints": {Service: "blueprints"}}); err != nil {
		t.Fatalf("checkRollouts = %v, want nil on a read error", err)
	}
	if len(written) != 0 {
		t.Errorf("status written from a failed read: %v", written)
	}
}

// An unmanaged (externally-owned / absent) workload is never written to a row, so
// builder does not claim a rollout status for a service the chart owns.
func TestCheckRollouts_UnmanagedIsNotRecorded(t *testing.T) {
	fb := &fakeBackend{rollouts: map[string]rolloutState{"forge": {Status: rolloutUnmanaged}}}
	rec, written := newTestReconciler(fb)
	if err := rec.checkRollouts(context.Background(), map[string]workloadSpec{"forge": {Service: "forge"}}); err != nil {
		t.Fatalf("checkRollouts: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("unmanaged workload recorded: %v", written)
	}
}
