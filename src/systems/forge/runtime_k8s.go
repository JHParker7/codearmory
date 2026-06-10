package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
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
// after the result is collected. An optional RuntimeClass (K8S_RUNTIME_CLASS)
// enables gVisor or other sandboxed runtimes. Resource limits are determined
// per-execution by the runner class stored in the database.
type KubernetesRuntime struct {
	client       kubernetes.Interface
	namespace    string
	runtimeClass *string
}

func newKubernetesRuntime() (*KubernetesRuntime, error) {
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

	var rc *string
	if v := os.Getenv("K8S_RUNTIME_CLASS"); v != "" {
		rc = &v
	}

	return &KubernetesRuntime{
		client:       client,
		namespace:    envOrDefault("K8S_NAMESPACE", "forge"),
		runtimeClass: rc,
	}, nil
}

// Run creates a Kubernetes Job for the execution, polls until it reaches a
// terminal state, collects logs, then deletes the job. The job is always
// deleted on return, even if Run returns an error.
func (r *KubernetesRuntime) Run(ctx context.Context, exec Execution) (RunResult, error) {
	spec, err := runnerClassSpec(ctx, exec.RunnerClass)
	if err != nil {
		return RunResult{}, fmt.Errorf("runner class: %w", err)
	}
	memLimit := resource.MustParse(fmt.Sprintf("%dMi", spec.MemoryMB))
	cpuLimit := resource.MustParse(fmt.Sprintf("%dm", spec.CPUMillicores))
	tmpSize := resource.MustParse(fmt.Sprintf("%dMi", spec.TmpfsMB))

	jobName := "forge-" + exec.ExecutionID

	envVars := make([]corev1.EnvVar, 0, len(exec.Env))
	for k, v := range exec.Env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	job := &batchv1.Job{
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
					RestartPolicy:                corev1.RestartPolicyNever,
					RuntimeClassName:             r.runtimeClass,
					// Prevent the pod from inheriting cluster credentials via
					// the default service account token.
					AutomountServiceAccountToken: ptr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
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

	if _, err := r.client.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return RunResult{}, fmt.Errorf("create job: %w", err)
	}

	result, err := r.waitAndCollect(ctx, exec, jobName)
	// Always clean up, even on error or cancellation.
	r.deleteJob(context.Background(), jobName)
	return result, err
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
					if cond.Type == batchv1.JobFailed && cond.Reason == "DeadlineExceeded" {
						timedOut = true
					}
				}
				exitCode = 1
				goto done
			}
		}
	}

done:
	stdout, stderr := r.collectLogs(exec.ExecutionID)

	if timedOut {
		return RunResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode},
			fmt.Errorf("timed out after %ds: %w", exec.TimeoutSecs, context.DeadlineExceeded)
	}

	// Attempt to get the real exit code from the pod's container status.
	if code, ok := r.podExitCode(exec.ExecutionID); ok {
		exitCode = code
	}

	return RunResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
}

func (r *KubernetesRuntime) collectLogs(executionID string) (stdout, stderr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "execution-id=" + executionID,
	})
	if err != nil || len(pods.Items) == 0 {
		return "", ""
	}
	podName := pods.Items[0].Name

	// Kubernetes pod logs combine stdout and stderr into a single stream.
	req := r.client.CoreV1().Pods(r.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: "runner",
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", ""
	}
	defer stream.Close()
	var buf bytes.Buffer
	io.Copy(&buf, io.LimitReader(stream, maxOutputBytes))
	return buf.String(), ""
}

func (r *KubernetesRuntime) podExitCode(executionID string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "execution-id=" + executionID,
	})
	if err != nil || len(pods.Items) == 0 {
		return 0, false
	}
	for _, cs := range pods.Items[0].Status.ContainerStatuses {
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
		slog.Warn("forge: failed to delete job", "job", jobName, "error", err)
	}
}
