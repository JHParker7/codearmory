package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubernetesRuntime runs executions as single-container Kubernetes Jobs.
// Jobs are created in K8S_NAMESPACE (default: forge) and deleted immediately
// after the result is collected. An optional RuntimeClass — set per-backend via
// the "runtime_class" config key, or process-wide via K8S_RUNTIME_CLASS — runs
// jobs under a sandboxed runtime such as gVisor or Kata Containers. The "kata"
// backend type wires this same runtime with a required RuntimeClass, moving the
// isolation boundary to a lightweight VM. Resource limits are determined
// per-execution by the runner class stored in the database.
type KubernetesRuntime struct {
	client       kubernetes.Interface
	namespace    string
	runtimeClass *string
}

// k8sKeyRuntimeClass is the backend config key naming the Kubernetes RuntimeClass
// to run jobs under. Optional for the "kubernetes" type (falls back to the
// K8S_RUNTIME_CLASS env var, then the cluster's default runtime); required for the
// "kata" type, which exists precisely to pin a VM-isolating RuntimeClass.
const k8sKeyRuntimeClass = "runtime_class"

// newKubernetesRuntime builds a Kubernetes runtime. configRuntimeClass is the
// backend's per-backend RuntimeClass (the "runtime_class" config key); see
// resolveRuntimeClass for how it combines with the legacy K8S_RUNTIME_CLASS env.
func newKubernetesRuntime(configRuntimeClass string) (*KubernetesRuntime, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = clientcmd.RecommendedHomeFile
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &KubernetesRuntime{
		client:       client,
		namespace:    envOrDefault("K8S_NAMESPACE", "forge"),
		runtimeClass: resolveRuntimeClass(configRuntimeClass),
	}, nil
}

// resolveRuntimeClass picks the RuntimeClass pointer for a kubernetes/kata
// backend: an explicit per-backend config value wins; otherwise the process-wide
// K8S_RUNTIME_CLASS env var (legacy single-runtime deployments) is used; a nil
// result means the cluster's default runtime (typically runc).
func resolveRuntimeClass(configRuntimeClass string) *string {
	if configRuntimeClass != "" {
		return &configRuntimeClass
	}
	if v := os.Getenv("K8S_RUNTIME_CLASS"); v != "" {
		return &v
	}
	return nil
}

// sandboxUID is the non-root UID/GID that sandboxed containers run as. Pinning
// it lets images that default to root (alpine, ubuntu, …) satisfy RunAsNonRoot
// and actually start — without it the kubelet blocks them at admission with
// "container has runAsNonRoot and image will run as root". Override with
// FORGE_SANDBOX_UID for images whose only usable account is a different fixed UID.
var sandboxUID = sandboxUIDFromEnv()

func sandboxUIDFromEnv() int64 {
	if v := os.Getenv("FORGE_SANDBOX_UID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
		slog.Warn("forge: ignoring invalid FORGE_SANDBOX_UID, using default", "value", v, "default", 1000)
	}
	return 1000
}

// Run creates a Kubernetes Job for the execution, polls until it reaches a
// terminal state, collects logs, then deletes the job. The job is always
// deleted on return, even if Run returns an error.
func (r *KubernetesRuntime) Run(ctx context.Context, exec Execution) (RunResult, error) {
	spec, err := runnerClassSpec(ctx, exec.RunnerClass)
	if err != nil {
		return RunResult{}, fmt.Errorf("runner class: %w", err)
	}
	job := r.buildJob(exec, spec)

	if _, err := r.client.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return RunResult{}, fmt.Errorf("create job: %w", err)
	}

	result, err := r.waitAndCollect(ctx, exec, job.Name)
	// Always clean up, even on error or cancellation.
	r.deleteJob(context.Background(), job.Name)
	return result, err
}

// buildJob constructs the sandboxed Kubernetes Job spec for an execution. It is
// separated from Run so the pod/container security context can be unit-tested
// without a live cluster.
func (r *KubernetesRuntime) buildJob(exec Execution, spec RunnerClass) *batchv1.Job {
	memLimit := resource.MustParse(fmt.Sprintf("%dMi", spec.MemoryMB))
	cpuLimit := resource.MustParse(fmt.Sprintf("%dm", spec.CPUMillicores))
	tmpSize := resource.MustParse(fmt.Sprintf("%dMi", spec.TmpfsMB))

	jobName := "forge-" + exec.ExecutionID

	envVars := make([]corev1.EnvVar, 0, len(exec.Env))
	for k, v := range exec.Env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: r.namespace,
			Labels:    map[string]string{"app": "forge", "execution-id": exec.ExecutionID},
		},
		Spec: batchv1.JobSpec{
			// BackoffLimit=0: a container failure is a valid result, not something
			// to retry. Retrying would change the exit code and produce duplicate logs.
			BackoffLimit: ptr(int32(0)),
			// TTLSecondsAfterFinished is a safety net: Run() deletes the job
			// immediately on completion, but if the process crashes before that,
			// K8s will clean it up after 5 minutes.
			TTLSecondsAfterFinished: ptr(int32(300)),
			ActiveDeadlineSeconds:   &exec.TimeoutSecs,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "forge", "execution-id": exec.ExecutionID},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					RuntimeClassName: r.runtimeClass,
					// Prevent the pod from inheriting cluster credentials via
					// the default service account token.
					AutomountServiceAccountToken: ptr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						RunAsUser:      ptr(sandboxUID),
						RunAsGroup:     ptr(sandboxUID),
						FSGroup:        ptr(sandboxUID), // make the /tmp emptyDir writable by the sandbox group
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "runner",
						Image:   exec.Image,
						Command: exec.Command,
						Env:     envVars,
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: memLimit,
								corev1.ResourceCPU:    cpuLimit,
							},
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: memLimit,
								corev1.ResourceCPU:    cpuLimit,
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(true),
							RunAsNonRoot:             ptr(true),
							RunAsUser:                ptr(sandboxUID),
							RunAsGroup:               ptr(sandboxUID),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
					}},
					Volumes: []corev1.Volume{{
						Name: "tmp",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpSize},
						},
					}},
				},
			},
		},
	}
}

func (r *KubernetesRuntime) waitAndCollect(ctx context.Context, exec Execution, jobName string) (RunResult, error) {
	// Add 60 s over the job's ActiveDeadlineSeconds so the poll loop doesn't
	// time out before K8s marks the job failed — otherwise we'd return an
	// ambiguous context error instead of the clear "timed out after Ns" one.
	deadline := time.Duration(exec.TimeoutSecs+60) * time.Second
	pollCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var timedOut bool
	var exitCode int
	var failedMsg string

	for {
		select {
		case <-pollCtx.Done():
			// Caller cancelled (DELETE request) or our safety deadline hit.
			return RunResult{}, pollCtx.Err()
		case <-ticker.C:
			j, err := r.client.BatchV1().Jobs(r.namespace).Get(pollCtx, jobName, metav1.GetOptions{})
			if err != nil {
				return RunResult{}, fmt.Errorf("get job: %w", err)
			}

			if j.Status.Succeeded > 0 {
				exitCode = 0
				goto done
			}
			if j.Status.Failed > 0 {
				for _, cond := range j.Status.Conditions {
					if cond.Type != batchv1.JobFailed {
						continue
					}
					if cond.Reason == "DeadlineExceeded" {
						timedOut = true
					}
					// Keep the job's own failure message (e.g. "Job has reached the
					// specified backoff limit") as a fallback diagnostic for the
					// no-pod case below; the events lookup yields a better one.
					if m := strings.TrimSpace(cond.Message); m != "" {
						failedMsg = m
					} else if cond.Reason != "" {
						failedMsg = cond.Reason
					}
				}
				exitCode = 1
				goto done
			}
		}
	}

done:
	// Resolve the pod once and derive both logs and the real exit code from the
	// same snapshot (avoids two separate List calls against the API server).
	pod, podErr := r.findPod(exec.ExecutionID)

	var stdout string
	var logErr error
	switch {
	case podErr != nil:
		logErr = podErr
	case pod == nil:
		if timedOut {
			// The timeout return below discards logErr, so skip the
			// network-bound job-event lookup on this path.
			break
		}
		logErr = noPodError(r.jobFailureReason(jobName), failedMsg)
	default:
		stdout, logErr = r.collectLogs(pod.Name)
	}

	if timedOut {
		return RunResult{Stdout: stdout, ExitCode: ptr(exitCode)},
			fmt.Errorf("timed out after %ds: %w", exec.TimeoutSecs, context.DeadlineExceeded)
	}

	// Prefer the real container exit code over the job-level guess.
	if pod != nil {
		if code, ok := podExitCode(pod); ok {
			exitCode = code
		}
	}

	// Only surface a log-collection problem when the command actually failed; on
	// a successful run an unreadable-logs note would masquerade as the job's own
	// stderr (and a completed execution is expected to have empty stderr).
	stderr := ""
	if logErr != nil && exitCode != 0 {
		stderr = "forge: " + logErr.Error()
	}
	return RunResult{Stdout: stdout, Stderr: stderr, ExitCode: ptr(exitCode)}, nil
}

// findPod returns the single pod for an execution, or (nil, nil) if none exists
// yet (e.g. evicted or never scheduled).
func (r *KubernetesRuntime) findPod(executionID string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "execution-id=" + executionID,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

// noPodError composes the diagnostic returned when a job reaches a terminal
// state but no pod can be found. It prefers a reason pulled from the job's
// events (which names the real cause — most often a RuntimeClass that does not
// exist or has no node to run it, so "VM"/kata runners fail before a pod is ever
// admitted), then the job's own failure-condition message, then a generic hint.
func noPodError(eventReason, condMsg string) error {
	switch {
	case eventReason != "":
		return fmt.Errorf("no pod ran for execution: %s", eventReason)
	case condMsg != "":
		return fmt.Errorf("no pod ran for execution: %s (no pod was scheduled)", condMsg)
	default:
		return fmt.Errorf("no pod found for execution — logs unavailable (the pod may have been evicted or never scheduled; verify the backend's RuntimeClass exists and a node can run it)")
	}
}

// jobFailureReason returns a short, human-readable reason the job produced no
// pod, taken from the job's most recent Warning event (e.g. a FailedCreate that
// names a missing RuntimeClass). Returns "" when no informative event exists.
// The field selector narrows the query on a real API server; the client-side
// filter keeps it correct under the fake clientset, which ignores it.
func (r *KubernetesRuntime) jobFailureReason(jobName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	evs, err := r.client.CoreV1().Events(r.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Job,involvedObject.name=" + jobName,
	})
	if err != nil {
		return ""
	}

	var best *corev1.Event
	for i := range evs.Items {
		e := &evs.Items[i]
		if e.InvolvedObject.Name != jobName || e.Type != corev1.EventTypeWarning {
			continue
		}
		if best == nil || eventTime(e).After(eventTime(best)) {
			best = e
		}
	}
	if best == nil {
		return ""
	}

	msg := strings.TrimSpace(best.Message)
	reason := strings.TrimSpace(best.Reason)
	switch {
	case reason != "" && msg != "":
		return reason + ": " + msg
	case reason != "":
		return reason
	default:
		return msg
	}
}

// eventTime returns the most recent timestamp an event carries. Aggregated
// (series) events populate EventTime and leave the legacy LastTimestamp zero, so
// taking whichever is later keeps "most recent Warning wins" correct on modern
// clusters instead of comparing two zero LastTimestamps.
func eventTime(e *corev1.Event) time.Time {
	t := e.LastTimestamp.Time
	if e.EventTime.Time.After(t) {
		t = e.EventTime.Time
	}
	return t
}

// collectLogs streams the runner container's combined stdout+stderr (Kubernetes
// merges the two into a single log stream).
func (r *KubernetesRuntime) collectLogs(podName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := r.client.CoreV1().Pods(r.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: "runner",
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream pod logs: %w", err)
	}
	defer stream.Close()
	var buf bytes.Buffer
	io.Copy(&buf, io.LimitReader(stream, maxOutputBytes)) //nolint:errcheck
	return buf.String(), nil
}

// podExitCode reads the runner container's terminated exit code from a pod
// snapshot, if it has terminated.
func podExitCode(pod *corev1.Pod) (int, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "runner" && cs.State.Terminated != nil {
			return int(cs.State.Terminated.ExitCode), true
		}
	}
	return 0, false
}

// Cancel deletes the Kubernetes Job for the execution, which terminates the
// running pod. The context passed to Run will also be cancelled by the worker.
func (r *KubernetesRuntime) Cancel(_ context.Context, executionID string) error {
	r.deleteJob(context.Background(), "forge-"+executionID)
	return nil
}

func (r *KubernetesRuntime) deleteJob(ctx context.Context, jobName string) {
	policy := metav1.DeletePropagationForeground
	if err := r.client.BatchV1().Jobs(r.namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	}); err != nil {
		slog.WarnContext(ctx, "forge: failed to delete job", "job", jobName, "error", err)
	}
}
