package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// The Kubernetes half of LeaseRuntime.
//
// A lease is a bare Pod rather than a Job. A Job exists to drive a workload to
// completion — retries, backoff, completion tracking — and every one of those is
// wrong for a sandbox whose whole purpose is to sit still and be exec'd into. The
// pod carries exactly the security posture buildJob gives an execution: same
// contexts, same runtime class, same dropped capabilities, same egress proxy, same
// resource ceiling from the runner class. A lease must not be a way to get a
// laxer sandbox than a one-shot execution would have got.

// leasePodLabel marks a pod as backing a lease, and is what the orphan sweep
// selects on after a restart that lost every in-memory handle.
const leasePodLabel = "lease-id"

// StartLease creates the lease's sandbox and waits until it is ready to accept
// execs — which means the container is running AND any checkout has finished, not
// merely that the pod scheduled. Returning early on "running" would hand back a
// lease whose first exec races the clone.
func (r *KubernetesRuntime) StartLease(ctx context.Context, lease Lease) error {
	spec, err := runnerClassSpec(ctx, lease.RunnerClass)
	if err != nil {
		return fmt.Errorf("runner class: %w", err)
	}
	pod := r.buildLeasePod(lease, spec)
	if _, err := r.client.CoreV1().Pods(r.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create lease pod: %w", err)
	}
	if err := r.waitLeaseReady(ctx, lease, pod.Name); err != nil {
		// A sandbox that never became usable is torn down here rather than left for
		// the reaper: the caller is about to be told the lease failed, and a pod still
		// holding its memory after that is a leak the caller cannot see or address.
		// context.Background so teardown still runs when ctx is what expired.
		r.deleteLeasePod(context.Background(), pod.Name)
		return err
	}
	return nil
}

// buildLeasePod constructs the sandbox pod. Split from StartLease so the security
// context and resource limits are unit-testable without a cluster, exactly as
// buildJob is.
func (r *KubernetesRuntime) buildLeasePod(lease Lease, spec RunnerClass) *corev1.Pod {
	memLimit := resource.MustParse(fmt.Sprintf("%dMi", spec.MemoryMB))
	cpuLimit := resource.MustParse(fmt.Sprintf("%dm", spec.CPUMillicores))
	tmpSize := resource.MustParse(fmt.Sprintf("%dMi", spec.TmpfsMB))

	envVars := make([]corev1.EnvVar, 0, len(lease.Env))
	for k, v := range lease.Env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}
	if r.egressProxy != "" {
		for _, pair := range proxyEnvPairs(r.egressProxy) {
			if _, exists := lease.Env[pair[0]]; !exists {
				envVars = append(envVars, corev1.EnvVar{Name: pair[0], Value: pair[1]})
			}
		}
	}

	// Git refuses to operate on a repository whose top-level directory belongs to
	// another user ("detected dubious ownership"), and that is exactly what a lease
	// hands it: the working directory is a volume the kubelet creates as root,
	// while commands run as the sandbox UID. The checkout itself survives — it
	// writes files rather than reading a repository — so the failure appears later,
	// on the first git command an agent runs, which makes it read like a bug in the
	// agent rather than in the sandbox it was given.
	//
	// Declared through GIT_CONFIG_* rather than a `git config --global` call
	// because there is nowhere to write a global config: the root filesystem is
	// read-only, and every command would otherwise have to remember to set it.
	envVars = append(envVars,
		corev1.EnvVar{Name: "GIT_CONFIG_COUNT", Value: "1"},
		corev1.EnvVar{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
		corev1.EnvVar{Name: "GIT_CONFIG_VALUE_0", Value: lease.workDir()},
	)

	privileged := spec.Privileged && r.kernelIsolated
	podSC, containerSC := podSecurityContexts(privileged)

	volumeMounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	volumes := []corev1.Volume{{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: &tmpSize,
			},
		},
	}}

	workingDir := ""
	for i, rm := range resolveVolumeMounts(lease.Volumes) {
		volName := fmt.Sprintf("ws-%d", i)
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: volName, MountPath: rm.mountPath, ReadOnly: rm.readOnly})
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: rm.resourceName,
					ReadOnly:  rm.readOnly,
				},
			},
		})
		if rm.workdir {
			workingDir = rm.mountPath
		}
	}
	// With no shared volume claiming the working directory, the lease still needs a
	// writable one: the non-privileged sandbox runs with a read-only root filesystem,
	// so a checkout would have nowhere to land. This emptyDir is the lease's own
	// workspace — node-backed rather than memory-backed, because unlike /tmp it holds
	// a source tree and build output, and charging those against the pod's memory
	// limit would make the runner class's memory mean two different things.
	if workingDir == "" {
		workingDir = leaseWorkDir
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "workspace", MountPath: leaseWorkDir})
		volumes = append(volumes, corev1.Volume{
			Name:         "workspace",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	labels := map[string]string{"app": "forge", leasePodLabel: lease.LeaseID}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lease.resourceName(),
			Namespace: r.namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			RuntimeClassName: r.runtimeClass,
			NodeSelector:     r.execNodeSelector,
			Tolerations:      r.execTolerations,
			// A lease has no more business reading cluster credentials than an
			// execution does.
			AutomountServiceAccountToken: ptr(false),
			SecurityContext:              podSC,
			// Kubernetes enforces the lease's own lifetime as a second, independent
			// backstop to the reaper and the idle `sleep`: if forge dies holding leases,
			// the cluster still collects them. The startup grace matches the job path's,
			// so a slow image pull or PVC bind does not eat the lease's usable life.
			ActiveDeadlineSeconds: ptr(lease.MaxLifetimeSecs + podStartupGraceSecs),
			Containers: []corev1.Container{{
				Name:            "runner",
				Image:           lease.Image,
				ImagePullPolicy: imagePullPolicy,
				Command:         lease.startCommand(),
				Env:             envVars,
				WorkingDir:      workingDir,
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
				SecurityContext: containerSC,
				VolumeMounts:    volumeMounts,
			}},
			Volumes: volumes,
		},
	}
}

// waitLeaseReady polls until the sandbox prints its ready marker, the pod fails, or
// the context expires.
func (r *KubernetesRuntime) waitLeaseReady(ctx context.Context, lease Lease, podName string) error {
	ticker := time.NewTicker(k8sPollInterval)
	defer ticker.Stop()
	for {
		pod, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("lease sandbox disappeared while starting")
			}
			return fmt.Errorf("get lease pod: %w", err)
		}
		switch pod.Status.Phase {
		case corev1.PodFailed, corev1.PodSucceeded:
			// Succeeded is a failure for a lease: the start command is meant to block
			// forever on `sleep`, so reaching completion means the checkout exited
			// non-zero under `set -e`, or the image's shell is not where we think.
			logs, _ := r.collectLogs(podName)
			return fmt.Errorf("lease sandbox exited while starting: %s", firstNonEmptyLine(logs, r.podFailureDetail(pod, podName)))
		case corev1.PodRunning:
			logs, logErr := r.collectLogs(podName)
			if logErr == nil && strings.Contains(logs, leaseReadyMarker) {
				slog.InfoContext(ctx, "lease sandbox ready", "lease_id", lease.LeaseID, "pod", podName)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			// Distinguish "we ran out of patience" from "the caller went away": the
			// first is a lease that is too slow to start and wants its start timeout
			// raised, the second is a cancelled request and not a fault at all.
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("lease sandbox did not become ready within %ds", leaseStartTimeoutSecs)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Exec runs one command inside a started lease.
//
// The command is delivered over the pods/exec subresource, which starts a NEW
// process in the existing container. That is the whole saving — no scheduling, no
// image pull, no sandbox boot — and also the constraint that shapes the rest of
// the lease design: the process inherits the container's environment and working
// directory as they were at boot, and nothing an exec does can change them for the
// next one.
func (r *KubernetesRuntime) Exec(ctx context.Context, lease Lease, exec Execution) (RunResult, error) {
	if r.restConfig == nil {
		return RunResult{}, fmt.Errorf("this runtime was built without a REST config and cannot exec into a lease")
	}
	podName := lease.resourceName()

	// The command timeout is the lease exec's only deadline. Unlike a job there is no
	// pod startup to account for — the sandbox is already up — so the command gets its
	// full budget and nothing else needs to be added to it.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(exec.TimeoutSecs)*time.Second)
	defer cancel()

	req := r.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(r.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "runner",
			Command:   exec.Command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	streamer, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", req.URL())
	if err != nil {
		return RunResult{}, fmt.Errorf("prepare lease exec: %w", err)
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	streamErr := streamer.StreamWithContext(runCtx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	result := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	// Sample memory best-effort, as the job path does. It reports the whole sandbox's
	// usage, which for a lease spans everything run in it so far rather than this
	// command alone — worth knowing when reading the number.
	if r.podMemoryMB != nil {
		if mb, ok := r.podMemoryMB(ctx, podName); ok {
			result.MemoryUsedMB = ptr(mb)
		}
	}
	result.MemoryLimitMB = nil

	if streamErr != nil {
		// A non-zero exit arrives as an error carrying the status, not as a transport
		// failure. Unwrapping it here is what keeps "the command failed" distinct from
		// "forge could not run the command" — the worker's classifyResult depends on
		// exactly that distinction to decide whether a run is a user failure or a
		// platform one.
		var codeErr interface{ ExitStatus() int }
		if asExitStatus(streamErr, &codeErr) {
			code := codeErr.ExitStatus()
			result.ExitCode = &code
			return result, nil
		}
		if runCtx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("command timed out after %ds", exec.TimeoutSecs)
		}
		return result, fmt.Errorf("lease exec: %w", streamErr)
	}

	code := 0
	result.ExitCode = &code
	slog.DebugContext(ctx, "lease exec complete", "lease_id", lease.LeaseID, "execution_id", exec.ExecutionID, "duration", time.Since(start))
	return result, nil
}

// StopLease deletes the sandbox pod. Idempotent — a missing pod is success, so an
// explicit release racing the reaper is harmless.
func (r *KubernetesRuntime) StopLease(ctx context.Context, lease Lease) error {
	return r.deleteLeasePod(ctx, lease.resourceName())
}

func (r *KubernetesRuntime) deleteLeasePod(ctx context.Context, podName string) error {
	// Zero grace: the sandbox holds nothing worth flushing, and every second of
	// termination is memory another lease cannot have.
	err := r.client.CoreV1().Pods(r.namespace).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: ptr(int64(0)),
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete lease pod %s: %w", podName, err)
	}
	return nil
}

// OrphanedLeasePods lists lease sandbox pods whose lease id is not in known. These
// are pods forge lost track of — created by a process that died before committing
// the row, or belonging to leases already finished — and each one holds a runner
// class's worth of memory until something deletes it.
func (r *KubernetesRuntime) OrphanedLeasePods(ctx context.Context, known map[string]bool) ([]string, error) {
	list, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: leasePodLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("list lease pods: %w", err)
	}
	var orphans []string
	for _, pod := range list.Items {
		if id := pod.Labels[leasePodLabel]; id != "" && !known[id] {
			orphans = append(orphans, pod.Name)
		}
	}
	return orphans, nil
}

// DeleteLeasePodByName tears down a sandbox addressed by pod name, for orphans
// whose lease row no longer exists to derive it from.
func (r *KubernetesRuntime) DeleteLeasePodByName(ctx context.Context, podName string) error {
	return r.deleteLeasePod(ctx, podName)
}

// asExitStatus reports whether err (or anything it wraps) carries a command exit
// status, assigning it to out. remotecommand returns a CodeExitError for a non-zero
// exit; matching on the behaviour rather than the concrete type keeps this working
// if that type moves, and costs nothing.
func asExitStatus(err error, out *interface{ ExitStatus() int }) bool {
	for e := err; e != nil; {
		if es, ok := e.(interface{ ExitStatus() int }); ok {
			*out = es
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// firstNonEmptyLine picks the most useful of several candidate diagnostics: the
// first that has any content. Used to turn a failed start into one clear line.
func firstNonEmptyLine(candidates ...string) string {
	for _, c := range candidates {
		for _, line := range strings.Split(c, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				return s
			}
		}
	}
	return "no diagnostic available"
}
