package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Rollout observation.
//
// The reconcile loop applies desired state and returns. Whether the workload it just
// wrote actually came up is a separate question, and nothing used to ask it: a
// Deployment whose image does not resolve is accepted by the apiserver, the new
// ReplicaSet's pods sit in ImagePullBackOff, and the reconcile reports success. The
// loop is unattended (BUILDER_RECONCILE_INTERVAL, 30s) and rotation rolls workloads on
// a timer, so a bad desired image introduced at one time surfaces much later with no
// operator action in between to correlate it to.
//
// This file only OBSERVES. It never rolls back, re-pins an image, or scales anything:
// the reconciler manages live workloads unattended, so a wrong automatic correction is
// worse than a loud unfixed one. What it does is name the failure — on the log, on a
// metric, and on the service's own row — once the workload has been stuck long enough
// that it is not merely slow.

// Rollout status values recorded on the OrgService row.
const (
	// rolloutUnmanaged means there is nothing for builder to report: no Deployment
	// (EnsureService already surfaces a create failure) or one owned by someone else
	// (the Helm chart). Never written to a row.
	rolloutUnmanaged = ""
	// rolloutOK: the current spec is fully rolled out and available.
	rolloutOK = "ok"
	// rolloutProgressing: not converged yet, but inside the deadline. Normal during a
	// deploy — recorded, not alerted on.
	rolloutProgressing = "progressing"
	// rolloutFailed: pods have been stuck past the deadline. This is the loud one.
	rolloutFailed = "failed"
)

// Reasons rolloutState carries. The image-pull family is the failure this work exists
// for; Unschedulable is the other way a surged pod never becomes Ready.
const (
	reasonProgressing   = "Progressing"
	reasonUnschedulable = "Unschedulable"
)

// terminalWaitingReasons are kubelet container-waiting reasons that mean the pod will
// not start on its own: the image cannot be resolved/pulled, or its config refers to a
// Secret/ConfigMap key that does not exist. Deliberately NOT included: ContainerCreating
// and PodInitializing (a slow but healthy start), and CrashLoopBackOff — a crash loop is
// the application's problem, it has a different fix, and the image demonstrably pulled.
var terminalWaitingReasons = map[string]bool{
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"ImageInspectError":          true,
	"ErrImageNeverPull":          true,
	"RegistryUnavailable":        true,
	"CreateContainerConfigError": true,
}

// defaultRolloutDeadline is how long a workload may sit un-converged before the state
// is called failed. Generous on purpose: the reasons above are already definitive
// (kubelet tried and failed), but an Unschedulable pod can clear on its own while a
// cluster autoscaler adds a node, and calling that a failure would train operators to
// ignore the signal.
const defaultRolloutDeadline = 5 * time.Minute

// rolloutState is one observation of a managed workload's rollout.
type rolloutState struct {
	Status  string    // rolloutUnmanaged | rolloutOK | rolloutProgressing | rolloutFailed
	Reason  string    // the kubelet/scheduler reason, e.g. ImagePullBackOff
	Message string    // human-readable detail, including the image when we know it
	Since   time.Time // when the stuck pod appeared; zero when nothing is stuck
}

// failed reports whether this observation is the loud one.
func (s rolloutState) failed() bool { return s.Status == rolloutFailed }

// rolloutDeadline reads BUILDER_ROLLOUT_DEADLINE (a Go duration). Empty or invalid
// gives defaultRolloutDeadline; a non-positive value disables rollout observation
// entirely, which is the escape hatch for an operator who does not want the extra
// reads against the apiserver.
func rolloutDeadline() time.Duration {
	v := strings.TrimSpace(os.Getenv("BUILDER_ROLLOUT_DEADLINE"))
	if v == "" {
		return defaultRolloutDeadline
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultRolloutDeadline
	}
	if d <= 0 {
		return 0 // disabled
	}
	return d
}

// RolloutState reports how the managed workload for service is faring. It is
// read-only: a Get plus (only when the workload has not converged) a pod List.
//
// A missing Deployment or one owned by something else reports rolloutUnmanaged rather
// than an error — neither is builder's to speak for, and EnsureService already fails
// loudly when it cannot create its own.
func (b *k8sBackend) RolloutState(ctx context.Context, service string, deadline time.Duration) (rolloutState, error) {
	dep, err := b.client.AppsV1().Deployments(b.namespace).Get(ctx, b.name(service), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return rolloutState{Status: rolloutUnmanaged}, nil
	}
	if err != nil {
		return rolloutState{}, err
	}
	if dep.Labels[labelManagedBy] != managedByValue {
		return rolloutState{Status: rolloutUnmanaged}, nil
	}
	if converged(dep) {
		return rolloutState{Status: rolloutOK}, nil
	}
	// Only reached when something is actually unsettled, so the steady state costs one
	// Get per service per pass and no pod List at all.
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return rolloutState{}, fmt.Errorf("selector for %s: %w", service, err)
	}
	pods, err := b.client.CoreV1().Pods(b.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return rolloutState{}, err
	}
	return evaluateRollout(dep, pods.Items, b.now(), deadline), nil
}

// now is the backend clock (overridden in tests via nowFn).
func (b *k8sBackend) now() time.Time {
	if b.nowFn != nil {
		return b.nowFn()
	}
	return time.Now()
}

// converged reports whether the Deployment's CURRENT spec is fully rolled out: the
// controller has seen this generation, every replica is on the new template, and they
// are available. Checking observedGeneration matters — right after an Update the status
// still describes the previous spec, and reading it as converged would declare a
// just-applied bad image healthy.
func converged(dep *appsv1.Deployment) bool {
	if dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	return dep.Status.UpdatedReplicas >= want &&
		dep.Status.AvailableReplicas >= want &&
		dep.Status.UnavailableReplicas == 0
}

// evaluateRollout turns a Deployment plus its pods into one observation. Pure, so the
// decision is unit-tested without a cluster.
//
// The age that is measured against the deadline is the STUCK POD's, not a timer builder
// keeps: it survives a builder restart, and it dates the problem to when the bad pod was
// created rather than to when builder happened to notice.
func evaluateRollout(dep *appsv1.Deployment, pods []corev1.Pod, now time.Time, deadline time.Duration) rolloutState {
	if converged(dep) {
		return rolloutState{Status: rolloutOK}
	}
	st, ok := stuckPod(pods)
	if !ok {
		// Un-converged with no pod in a terminal state: an ordinary in-flight rollout.
		return rolloutState{Status: rolloutProgressing, Reason: reasonProgressing,
			Message: fmt.Sprintf("%d/%d replicas updated, %d available",
				dep.Status.UpdatedReplicas, replicaCount(dep), dep.Status.AvailableReplicas)}
	}
	if deadline > 0 && !st.Since.IsZero() && now.Sub(st.Since) < deadline {
		st.Status = rolloutProgressing
		return st
	}
	st.Status = rolloutFailed
	return st
}

func replicaCount(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas != nil {
		return *dep.Spec.Replicas
	}
	return 1
}

// stuckPod returns the state described by the oldest pod that cannot start on its own,
// or ok=false when no pod is in that shape. Oldest wins so the reported age is the age
// of the problem, and so the answer is stable across passes instead of flapping between
// equally-stuck pods.
func stuckPod(pods []corev1.Pod) (rolloutState, bool) {
	ordered := make([]corev1.Pod, len(pods))
	copy(ordered, pods)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].CreationTimestamp.Time.Before(ordered[j].CreationTimestamp.Time)
	})
	for _, pod := range ordered {
		if pod.DeletionTimestamp != nil {
			continue // already on its way out; not evidence of a wedged rollout
		}
		if st, ok := podStuck(pod); ok {
			return st, true
		}
	}
	return rolloutState{}, false
}

// podStuck classifies a single pod: a container waiting on a terminal reason (an image
// that will not pull being the one this exists for), or one the scheduler cannot place.
func podStuck(pod corev1.Pod) (rolloutState, bool) {
	since := pod.CreationTimestamp.Time
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		w := cs.State.Waiting
		if w == nil || !terminalWaitingReasons[w.Reason] {
			continue
		}
		return rolloutState{
			Reason: w.Reason,
			Message: fmt.Sprintf("pod %s container %s: %s (image %s)%s",
				pod.Name, cs.Name, w.Reason, cs.Image, detail(w.Message)),
			Since: since,
		}, true
	}
	if pod.Status.Phase != corev1.PodPending {
		return rolloutState{}, false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return rolloutState{
				Reason:  reasonUnschedulable,
				Message: fmt.Sprintf("pod %s is unschedulable%s", pod.Name, detail(c.Message)),
				Since:   since,
			}, true
		}
	}
	return rolloutState{}, false
}

// detail appends the apiserver's own message when there is one, trimmed so a long
// scheduler explanation cannot dominate a log line or the stored row.
func detail(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	const max = 200
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	return ": " + msg
}

// noopBackend never deploys anything, so it has no rollout to report.
func (noopBackend) RolloutState(context.Context, string, time.Duration) (rolloutState, error) {
	return rolloutState{Status: rolloutUnmanaged}, nil
}
