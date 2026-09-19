package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// jobs under a sandboxed runtime such as gVisor or Kata Containers. The "kata" and
// "gvisor" backend types wire this same runtime with a required RuntimeClass, moving
// the isolation boundary to a lightweight VM (kata) or a userspace kernel (gvisor).
// Resource limits are determined per-execution by the runner class stored in the
// database.
type KubernetesRuntime struct {
	client kubernetes.Interface
	// restConfig is retained solely for pods/exec, which needs to build its own
	// SPDY/WebSocket transport rather than going through the typed clientset. It is
	// nil in tests that construct the runtime directly with a fake clientset, so the
	// lease exec path must nil-check it and report a clear error instead of panicking.
	restConfig   *rest.Config
	namespace    string
	runtimeClass *string
	// egressProxy is the HTTP proxy URL (FORGE_EGRESS_PROXY, e.g.
	// "http://egress-proxy:3128") injected as HTTP(S)_PROXY into every exec pod so
	// sandbox traffic routes through the egress allowlist. The NetworkPolicy confines
	// exec pods to that proxy, so without these env vars git/curl dial hosts directly
	// and get blocked — a clone fails right after "Cloning into…". Empty when the
	// backend isolates egress itself (kata) or the proxy is disabled. Mirrors the
	// Docker runtime.
	egressProxy string
	// kernelIsolated is true for backends that give the job its OWN kernel: "kata"
	// (a hardware-virtualized microVM) and "gvisor" (the gVisor userspace kernel /
	// Sentry). There a runner class may opt into running the job as root
	// (RunnerClass.Privileged), since root is contained away from the host kernel. It
	// is false for the plain "kubernetes" backend (shared host kernel), where
	// privileged is ignored and the locked-down sandbox is always applied.
	kernelIsolated bool
	// podMemoryMB returns a pod's current memory usage in MB from the
	// metrics.k8s.io API, with ok=false when metrics are unavailable (no
	// metrics-server installed, or the pod has not been scraped yet — common for
	// short jobs). Injected in newKubernetesRuntime; left nil by tests that build
	// the runtime directly, so the sampling loop must nil-check it.
	podMemoryMB func(ctx context.Context, podName string) (int64, bool)
	// execNodeSelector / execTolerations place exec pods on the nodes that can
	// actually run them. On a multi-node cluster the kata/gvisor runtime is usually
	// installed on a subset of (often tainted) nodes; without this a sandbox pod can
	// be scheduled onto a node with no RuntimeClass handler and fail to start. The
	// idiomatic alternative is a RuntimeClass `scheduling` block, which the scheduler
	// applies automatically — these are the explicit override for when that is not set.
	execNodeSelector map[string]string
	execTolerations  []corev1.Toleration
}

// k8sKeyRuntimeClass is the backend config key naming the Kubernetes RuntimeClass
// to run jobs under. Optional for the "kubernetes" type (falls back to the
// K8S_RUNTIME_CLASS env var, then the cluster's default runtime); required for the
// "kata" and "gvisor" types, which exist precisely to pin a sandboxing RuntimeClass.
const k8sKeyRuntimeClass = "runtime_class"

// k8sPollInterval is how often waitAndCollect polls a running Job's status. Kept
// short so a finished command is observed — and its logs returned — with minimal
// lag; this is the tail slice of a fast run's time-to-first-result, previously a
// flat 2s. Override with FORGE_K8S_POLL_INTERVAL_MS.
var k8sPollInterval = time.Duration(envIntOrDefault("FORGE_K8S_POLL_INTERVAL_MS", 500)) * time.Millisecond

// metricsSampleInterval throttles how often the poll loop samples pod memory from
// metrics.k8s.io. metrics-server scrapes only every ~15s, so sampling on every
// fast status tick would be wasted apiserver load for no fresher data; sampling a
// few seconds apart captures the same peak while keeping the fast tick to the
// job Get + pod List calls the QPS budget is sized for.
const metricsSampleInterval = 5 * time.Second

// podStartupGraceSecs is extra time added to a Job's ActiveDeadlineSeconds beyond
// the command timeout, so pod startup does not consume the command's own budget.
// Startup covers scheduling, binding+attaching+mounting a shared-workspace PVC, and
// pulling the image — and a WaitForFirstConsumer StorageClass (the recommended mode)
// provisions the volume only once the mounting pod schedules, so tens of seconds of
// bind latency on network storage land in the exec Job, not create-volume. Without
// this grace a short command timeout (default 30s) elapses while the runner is still
// ContainerCreating, killing the pod before the command runs ("pod produced no logs:
// ContainerCreating"). The command timeout itself is enforced from container start in
// waitAndCollect, so this looser outer deadline never extends the command's runtime;
// it only bounds a pod that never starts at all. Override with
// FORGE_POD_STARTUP_GRACE_SECS.
var podStartupGraceSecs = int64(envIntOrDefault("FORGE_POD_STARTUP_GRACE_SECS", 300))

// newKubernetesRuntime builds a Kubernetes runtime. configRuntimeClass is the
// backend's per-backend RuntimeClass (the "runtime_class" config key); see
// resolveRuntimeClass for how it combines with the legacy K8S_RUNTIME_CLASS env.
// kernelIsolated is true for the kata and gvisor backends, gating the privileged-job
// opt-in.
func newKubernetesRuntime(configRuntimeClass string, kernelIsolated bool) (*KubernetesRuntime, error) {
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

	// client-go defaults to QPS=5/Burst=10 — far too low for a job-polling
	// controller. waitAndCollect fires a job Get + pod List every k8sPollInterval
	// (500ms) per running job — 4x the rate of the old 2s poll now that fast
	// time-to-first-log matters — so a handful of concurrent executions would
	// saturate the client-side limiter and Get calls fail with "client rate
	// limiter Wait ... would exceed context deadline". Raise the ceiling
	// (env-overridable) so polling scales with concurrency; the apiserver's own
	// API Priority & Fairness is the real backstop. Metrics sampling is throttled
	// separately (metricsSampleInterval), so it is not part of the fast-tick load.
	cfg.QPS = float32(envFloatOrDefault("FORGE_K8S_QPS", 100))
	cfg.Burst = envIntOrDefault("FORGE_K8S_BURST", 200)

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	rt := &KubernetesRuntime{
		client:           client,
		restConfig:       cfg,
		namespace:        envOrDefault("K8S_NAMESPACE", "forge"),
		runtimeClass:     resolveRuntimeClass(configRuntimeClass),
		kernelIsolated:   kernelIsolated,
		egressProxy:      envOrDefault("FORGE_EGRESS_PROXY", ""),
		execNodeSelector: parseNodeSelector(os.Getenv("FORGE_EXEC_NODE_SELECTOR")),
		execTolerations:  parseTolerations(os.Getenv("FORGE_EXEC_TOLERATE_KEYS")),
	}
	rt.podMemoryMB = rt.fetchPodMemoryMB
	return rt, nil
}

// parseNodeSelector reads a "key=value,key2=value2" string into a label selector.
// Blank or malformed entries are skipped rather than failing startup — a runner
// scheduling constraint is not worth crashing the service over.
func parseNodeSelector(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.TrimSpace(k) != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseTolerations reads a comma-separated list of taint KEYS into tolerations, each
// an Exists toleration for that key. This covers the common case — "the RuntimeClass
// nodes are tainted with X, tolerate it" — without asking an operator to hand-write a
// toleration struct; the value/effect are left unset so it tolerates any value/effect.
func parseTolerations(s string) []corev1.Toleration {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []corev1.Toleration
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, corev1.Toleration{Key: k, Operator: corev1.TolerationOpExists})
		}
	}
	return out
}

// ClusterAllocatable sums the allocatable CPU and memory across all schedulable,
// Ready nodes — the capacity a percent-based admission budget is taken from.
//
// Allocatable, not Capacity: capacity minus kubelet/system-reserved is what pods can
// actually be scheduled into. Cordoned (unschedulable) and NotReady nodes are skipped
// because they cannot run a runner, so counting them would let forge admit work that
// then sits Pending. Requires list access to nodes (a cluster-scoped read) in forge's
// RBAC; on failure the caller keeps the prior budget rather than dropping to zero.
func (r *KubernetesRuntime) ClusterAllocatable(ctx context.Context) (cpuMillicores, memMB int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	nodes, err := r.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, err
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable || !nodeIsReady(n) {
			continue
		}
		cpuMillicores += n.Status.Allocatable.Cpu().MilliValue()
		memMB += n.Status.Allocatable.Memory().Value() / (1024 * 1024)
	}
	if cpuMillicores == 0 && memMB == 0 {
		return 0, 0, fmt.Errorf("no schedulable Ready nodes reported allocatable capacity")
	}
	return cpuMillicores, memMB, nil
}

// nodeIsReady reports whether a node's Ready condition is True.
func nodeIsReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// fetchPodMemoryMB reads the pod's summed container memory usage from the
// metrics.k8s.io API. It goes through the existing client's REST client via
// AbsPath so it needs no extra dependency or config — the request hits the same
// apiserver with the same credentials. ok=false on any failure (no metrics-server,
// pod not yet scraped, parse error) so the caller simply records no usage.
func (r *KubernetesRuntime) fetchPodMemoryMB(ctx context.Context, podName string) (int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := r.client.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", r.namespace, "pods", podName).
		DoRaw(ctx)
	if err != nil {
		return 0, false
	}
	return parsePodMetricsMemoryMB(raw)
}

// parsePodMetricsMemoryMB sums the memory usage across a PodMetrics' containers
// and returns whole MB (MiB). Split from the REST call so it can be unit-tested
// without a metrics API. Returns ok=false when the payload carries no usable
// memory figure.
func parsePodMetricsMemoryMB(raw []byte) (int64, bool) {
	var pm struct {
		Containers []struct {
			Usage struct {
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(raw, &pm); err != nil {
		return 0, false
	}
	var totalBytes int64
	for _, c := range pm.Containers {
		q, err := resource.ParseQuantity(c.Usage.Memory)
		if err != nil {
			continue
		}
		totalBytes += q.Value()
	}
	if totalBytes <= 0 {
		return 0, false
	}
	return totalBytes / bytesPerMiB, true
}

// resolveRuntimeClass picks the RuntimeClass pointer for a kubernetes/kata/gvisor
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

// imagePullPolicy controls whether the kubelet re-pulls the runner image when a
// copy is already cached on the node. The default, IfNotPresent, turns the node's
// image cache into a warm layer: the first run on a node pulls the image, every
// later run reuses it and skips the pull — usually the single largest slice of a
// cold start. (Kubernetes otherwise defaults :latest / untagged images to Always,
// re-pulling on every run.) Set FORGE_IMAGE_PULL_POLICY=Always to force a fresh
// pull each run (e.g. a mutable :latest that must never be stale), or Never.
var imagePullPolicy = imagePullPolicyFromEnv()

func imagePullPolicyFromEnv() corev1.PullPolicy {
	v := os.Getenv("FORGE_IMAGE_PULL_POLICY")
	switch corev1.PullPolicy(v) {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		return corev1.PullPolicy(v)
	default:
		if v != "" {
			slog.Warn("forge: ignoring invalid FORGE_IMAGE_PULL_POLICY, using default", "value", v, "default", string(corev1.PullIfNotPresent))
		}
		return corev1.PullIfNotPresent
	}
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
	// Stamp the configured limit whenever the job actually ran — it produced an
	// exit code, or we sampled memory before a cancellation. A never-scheduled
	// execution has neither and must not report a limit for work that never
	// happened.
	if result.ExitCode != nil || result.MemoryUsedMB != nil {
		result.MemoryLimitMB = ptr(spec.MemoryMB)
	}
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
	// Route sandbox egress through the proxy the NetworkPolicy confines exec pods to.
	// A user-set value in exec.Env wins, matching the Docker runtime.
	if r.egressProxy != "" {
		for _, pair := range proxyEnvPairs(r.egressProxy) {
			if _, exists := exec.Env[pair[0]]; !exists {
				envVars = append(envVars, corev1.EnvVar{Name: pair[0], Value: pair[1]})
			}
		}
	}

	// privileged is honoured only on a kernel-isolated backend (kata's microVM or
	// gvisor's Sentry): there root in the job is contained away from the host kernel,
	// so it is safe. On a shared-kernel container backend the flag is dropped and the
	// locked-down sandbox always applies.
	privileged := spec.Privileged && r.kernelIsolated
	podSC, containerSC := podSecurityContexts(privileged)

	// Start from the always-present tmpfs /tmp, then attach any shared workspace
	// volumes: each binds a pre-created PVC (POST /volumes) at its path, and a mount
	// that sets workdir pins the container's working directory there so the command
	// runs inside the volume (e.g. a checked-out repo). FSGroup on the pod security
	// context makes the PVC group-writable by the sandbox UID, same as /tmp.
	volumeMounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	volumes := []corev1.Volume{{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			// Memory-backed (tmpfs), matching the docker runtime's `--tmpfs /tmp`
			// and the `TmpfsMB` runner-class field. Critically, this is what makes
			// Firecracker (kata-fc) work: Firecracker has no virtio-fs/9p
			// filesystem sharing, so a default node-backed emptyDir cannot be
			// shared into the microVM and the container never starts. A Memory
			// emptyDir lives inside the guest and needs no host sharing. The size
			// counts against the pod memory limit, same as the docker tmpfs.
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: &tmpSize,
			},
		},
	}}
	workingDir := ""
	for i, rm := range resolveVolumeMounts(exec.Volumes) {
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
			// The deadline is the command timeout plus a startup grace, since it
			// counts from pod creation and so also covers ContainerCreating (volume
			// bind/attach/mount + image pull). waitAndCollect enforces the command
			// timeout precisely from container start, so this larger bound only trips
			// for a pod that never starts. See podStartupGraceSecs.
			ActiveDeadlineSeconds: ptr(exec.TimeoutSecs + podStartupGraceSecs),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "forge", "execution-id": exec.ExecutionID},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					RuntimeClassName: r.runtimeClass,
					NodeSelector:     r.execNodeSelector,
					Tolerations:      r.execTolerations,
					// Prevent the pod from inheriting cluster credentials via
					// the default service account token.
					AutomountServiceAccountToken: ptr(false),
					SecurityContext:              podSC,
					Containers: []corev1.Container{{
						Name:            "runner",
						Image:           exec.Image,
						ImagePullPolicy: imagePullPolicy,
						Command:         exec.Command,
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
			},
		},
	}
}

// podSecurityContexts returns the pod- and container-level security contexts for a
// job. The default (privileged=false) is the locked-down sandbox applied to every
// container backend: non-root on a pinned UID, read-only rootfs, all capabilities
// dropped, no privilege escalation, seccomp RuntimeDefault. When privileged is true
// — only ever set for a VM-isolated kata backend whose runner class opted in — the
// job runs as root with a writable rootfs and privilege escalation allowed so
// package managers (apt/pacman/dnf) work; the microVM, not the container, is the
// isolation boundary. It deliberately does NOT set container Privileged or host
// namespaces, so the pod stays within the PodSecurity "baseline" level (the forge
// namespace must therefore not enforce "restricted" for privileged classes).
func podSecurityContexts(privileged bool) (*corev1.PodSecurityContext, *corev1.SecurityContext) {
	if privileged {
		return &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(false),
				RunAsUser:      ptr(int64(0)),
				RunAsGroup:     ptr(int64(0)),
				FSGroup:        ptr(int64(0)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			}, &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr(true),
				ReadOnlyRootFilesystem:   ptr(false),
				RunAsNonRoot:             ptr(false),
				RunAsUser:                ptr(int64(0)),
				RunAsGroup:               ptr(int64(0)),
			}
	}
	return &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr(true),
			RunAsUser:      ptr(sandboxUID),
			RunAsGroup:     ptr(sandboxUID),
			FSGroup:        ptr(sandboxUID), // make the /tmp emptyDir writable by the sandbox group
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}, &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(true),
			RunAsNonRoot:             ptr(true),
			RunAsUser:                ptr(sandboxUID),
			RunAsGroup:               ptr(sandboxUID),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
}

func (r *KubernetesRuntime) waitAndCollect(ctx context.Context, exec Execution, jobName string) (RunResult, error) {
	// Add 60 s over the job's ActiveDeadlineSeconds (the command timeout plus the
	// startup grace) so the poll loop doesn't time out before K8s marks the job
	// failed — otherwise we'd return an ambiguous context error instead of the clear
	// "timed out after Ns" one.
	deadline := time.Duration(exec.TimeoutSecs+podStartupGraceSecs+60) * time.Second
	pollCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(k8sPollInterval)
	defer ticker.Stop()

	var timedOut bool
	var exitCode int
	var failedMsg string
	// lastPod retains the most recent pod snapshot seen while polling. A kata pod
	// that fails (a non-zero exit, StartError, a microVM that boots then dies,
	// OOMKilled) can be evicted or garbage-collected before the job is observed
	// terminal, leaving the findPod() at `done` empty — and then the container
	// state, exit code, and failure reason are gone with it. Snapshotting each tick
	// preserves the pod's last-known state so the real cause is surfaced instead of
	// the opaque "no pod found" message.
	var lastPod *corev1.Pod
	// peakMemMB tracks the highest memory sample seen from metrics.k8s.io across the
	// run. metrics-server scrapes roughly every 15 s, so a short job may never be
	// sampled (peak stays 0 → recorded as no usage); the limit is set separately.
	var peakMemMB int64
	// lastMetricsSample throttles the metrics fetch to metricsSampleInterval so the
	// fast status tick doesn't spam metrics.k8s.io for data that only refreshes every
	// ~15s. The zero value makes the first eligible tick sample immediately.
	var lastMetricsSample time.Time
	// cmdDeadline is when the command's own timeout expires, measured from the moment
	// the runner container is first observed running — not from pod creation, so slow
	// startup (volume bind + image pull, all ContainerCreating) does not count against
	// it. Zero until the container starts; the Job's larger ActiveDeadlineSeconds is
	// the backstop for a pod that never starts.
	var cmdDeadline time.Time

	for {
		select {
		case <-pollCtx.Done():
			// Caller cancelled (DELETE request) or our safety deadline hit.
			// Preserve any memory sampled before cancellation — the job did run
			// and consume it, so report it like the docker runtime does.
			var memUsed *int64
			if peakMemMB > 0 {
				memUsed = ptr(peakMemMB)
			}
			return RunResult{MemoryUsedMB: memUsed}, pollCtx.Err()
		case <-ticker.C:
			j, err := r.client.BatchV1().Jobs(r.namespace).Get(pollCtx, jobName, metav1.GetOptions{})
			if err != nil {
				return RunResult{}, fmt.Errorf("get job: %w", err)
			}
			if p, perr := r.findPod(pollCtx, exec.ExecutionID); perr == nil && p != nil {
				lastPod = p
			}
			if r.podMemoryMB != nil && lastPod != nil && time.Since(lastMetricsSample) >= metricsSampleInterval {
				lastMetricsSample = time.Now()
				if mb, ok := r.podMemoryMB(pollCtx, lastPod.Name); ok && mb > peakMemMB {
					peakMemMB = mb
				}
			}
			// Start the command clock the moment the runner container leaves
			// ContainerCreating, so pod-startup latency (which the Job's
			// ActiveDeadlineSeconds also covers) is never charged against the command.
			if cmdDeadline.IsZero() && runnerStarted(lastPod) {
				cmdDeadline = time.Now().Add(time.Duration(exec.TimeoutSecs) * time.Second)
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
					// Treat the outer deadline as a timeout only if the container
					// actually started and overran; if it never started, the deadline
					// bounded a stuck startup (unbindable volume, ImagePullBackOff), so
					// leave timedOut false and let podFailureDetail below name the real
					// blocker instead of a misleading "timed out after Ns".
					if cond.Reason == "DeadlineExceeded" && runnerStarted(lastPod) {
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

			// The command overran its own timeout, measured from container start. The
			// Job's ActiveDeadlineSeconds is a looser bound (it also covers startup), so
			// enforce the precise timeout here; the deferred deleteJob in Run terminates
			// the still-running container on return.
			if !cmdDeadline.IsZero() && time.Now().After(cmdDeadline) {
				timedOut = true
				exitCode = 1
				goto done
			}
		}
	}

done:
	// Resolve the pod once for the freshest state, then derive both logs and the
	// real exit code from the same snapshot. Fall back to the last snapshot seen
	// while polling: a failed kata pod can be evicted/GC'd between the terminal-job
	// observation and this lookup, and that snapshot still carries its container
	// state and exit code. A nil here means no pod was ever observed on any tick —
	// the genuine "never scheduled" case, left to the events-based noPodError.
	pod, podErr := r.findPod(pollCtx, exec.ExecutionID)
	podGone := false
	if pod == nil && podErr == nil {
		pod = lastPod
		// A non-nil snapshot here means the pod existed while polling but its live
		// object is now gone (evicted/GC'd). There are no logs left to stream, so
		// the cause must come from the snapshot's state and the surviving events.
		podGone = pod != nil
	}

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
	case podGone:
		// The pod was removed before we could read it — most often an eviction (a
		// memory-backed /tmp that exceeds its SizeLimit, or node memory pressure)
		// or pod garbage collection. Calling collectLogs would only yield a raw
		// "pods ... not found"; instead recover the cause from the snapshot's
		// container/pod state and the Warning events that outlive the pod, with a
		// clean, actionable message as the last resort.
		if detail := r.podFailureDetail(pod, jobName); detail != "" {
			logErr = fmt.Errorf("%s", clarifyNoLogFailure(detail, exec.TimeoutSecs+podStartupGraceSecs))
		} else {
			logErr = podRemovedError()
		}
	default:
		stdout, logErr = r.collectLogs(pod.Name)
		// A pod that never started its container (CreateContainerError,
		// ImagePullBackOff, OOMKilled, a kata microVM that failed to boot) has no
		// logs to stream — the real cause is in the container state / pod events,
		// not the empty log stream. Surface it instead of an opaque stream error.
		// This only reaches the caller when the command actually failed (the stderr
		// gate below requires a non-zero exit), so a silent successful run is safe.
		if strings.TrimSpace(stdout) == "" {
			if detail := r.podFailureDetail(pod, jobName); detail != "" {
				logErr = fmt.Errorf("%s", clarifyNoLogFailure(detail, exec.TimeoutSecs+podStartupGraceSecs))
			} else if apierrors.IsNotFound(logErr) {
				// The pod was found at `done` but deleted out from under us before
				// the log stream opened (the eviction/GC race), so collectLogs
				// returned a raw "pods ... not found". Don't leak it.
				logErr = podRemovedError()
			}
		}
	}

	// memUsed is the peak memory sample, nil when metrics never yielded one.
	var memUsed *int64
	if peakMemMB > 0 {
		memUsed = ptr(peakMemMB)
	}

	if timedOut {
		return RunResult{Stdout: stdout, ExitCode: ptr(exitCode), MemoryUsedMB: memUsed},
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
	return RunResult{Stdout: stdout, Stderr: stderr, ExitCode: ptr(exitCode), MemoryUsedMB: memUsed}, nil
}

// findPod returns the single pod for an execution, or (nil, nil) if none exists
// yet (e.g. evicted or never scheduled).
func (r *KubernetesRuntime) findPod(ctx context.Context, executionID string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
	case strings.Contains(eventReason, "Insufficient cpu"), strings.Contains(eventReason, "Insufficient memory"):
		return fmt.Errorf("no pod could be scheduled — the runner class requests more CPU/memory "+
			"than any node has free. Choose a smaller runner class or add cluster capacity. "+
			"(kubernetes reported: %s)", eventReason)
	case eventReason != "":
		return fmt.Errorf("no pod ran for execution: %s", eventReason)
	case condMsg != "":
		return fmt.Errorf("no pod ran for execution: %s (no pod was scheduled)", condMsg)
	default:
		return fmt.Errorf("no pod found for execution — logs unavailable (the pod may have been evicted or never scheduled; verify the backend's RuntimeClass exists and a node can run it)")
	}
}

// podRemovedError is the diagnostic for a pod that vanished before its logs could
// be read, leaving no surviving container/pod state or event to explain why. The
// overwhelmingly common cause is an eviction — and the memory-backed /tmp the kata
// runtime requires (Firecracker cannot share a node-backed volume into the microVM)
// counts against the pod memory limit, so a job that fills /tmp past its SizeLimit
// is evicted and its pod removed before we can read it.
func podRemovedError() error {
	return fmt.Errorf("pod was removed before its logs could be read — likely evicted (a memory-backed /tmp exceeding its SizeLimit, or node memory pressure) or garbage-collected")
}

// jobFailureReason returns a short, human-readable reason the job produced no
// usable pod, taken from the most recent Warning event for the job or one of its
// pods. The job emits FailedCreate/BackoffLimitExceeded (e.g. a missing
// RuntimeClass); the scheduler and kubelet emit FailedScheduling and
// FailedCreatePodSandBox on the *pod* (e.g. no kata-capable node, or a microVM
// that fails to boot) — and those pod events survive even after the pod object is
// gone, which is the common no-pod case. Returns "" when no informative event
// exists. The field selector narrows to Warnings on a real API server; the
// client-side filter keeps it correct under the fake clientset, which ignores it.
func (r *KubernetesRuntime) jobFailureReason(jobName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	evs, err := r.client.CoreV1().Events(r.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "type=" + corev1.EventTypeWarning,
	})
	if err != nil {
		return ""
	}

	var best *corev1.Event
	for i := range evs.Items {
		e := &evs.Items[i]
		if e.Type != corev1.EventTypeWarning || !involvesJob(e, jobName) {
			continue
		}
		if best == nil || eventTime(e).After(eventTime(best)) {
			best = e
		}
	}
	if best == nil {
		return ""
	}
	return joinReasonMessage(best.Reason, best.Message)
}

// involvesJob reports whether a namespace event belongs to the given job — either
// the Job object itself (FailedCreate, BackoffLimitExceeded) or one of the pods it
// created, named "<jobName>-<suffix>" (FailedScheduling, FailedCreatePodSandBox).
// The trailing dash keeps "forge-exec-1" from matching "forge-exec-12"'s pods.
func involvesJob(e *corev1.Event, jobName string) bool {
	o := e.InvolvedObject
	switch o.Kind {
	case "Job":
		return o.Name == jobName
	case "Pod":
		return strings.HasPrefix(o.Name, jobName+"-")
	default:
		return false
	}
}

// joinReasonMessage formats a Reason/Message pair into a single line without a
// dangling colon when either half is empty.
func joinReasonMessage(reason, message string) string {
	reason = strings.TrimSpace(reason)
	message = strings.TrimSpace(message)
	switch {
	case reason != "" && message != "":
		return reason + ": " + message
	case reason != "":
		return reason
	default:
		return message
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

// clarifyNoLogFailure turns the opaque Kubernetes reason behind a no-logs failure
// (detail, as produced by podFailureDetail) into a plain-language, actionable
// message, and returns the full "forge:"-less error string. It recognises the two
// classes users actually hit and cannot decode themselves:
//
//   - startup timeout: the Job's ActiveDeadlineSeconds fired while the runner was
//     still ContainerCreating, so the kubelet either leaves the container Waiting
//     ("ContainerCreating") or, having torn the pod down, marks it terminated
//     ("ContainerStatusUnknown: The container could not be located when the pod was
//     terminated"). Either way the command never ran — this is a startup timeout,
//     not a mysterious internal error, and the fix is more time or a lighter image.
//   - unschedulable: the scheduler placed the pod on no node ("FailedScheduling"),
//     almost always because the runner class requests more CPU/memory than any node
//     has free ("Insufficient cpu"/"Insufficient memory").
//
// startupBudget is the pod's total ActiveDeadlineSeconds (command timeout + startup
// grace) so the message can state exactly how long it waited. Any reason it does not
// recognise is returned verbatim under the original "pod produced no logs:" prefix,
// so a novel failure still surfaces its raw detail.
func clarifyNoLogFailure(detail string, startupBudget int64) string {
	switch {
	case strings.Contains(detail, "ContainerStatusUnknown"),
		strings.Contains(detail, "ContainerCreating"):
		return fmt.Sprintf("timed out after %ds while the container was still starting — "+
			"it never began running the command. This is usually a slow image pull or volume bind; "+
			"raise the runner timeout or FORGE_POD_STARTUP_GRACE_SECS, or use a smaller/pre-pulled image. "+
			"(kubernetes reported: %s)", startupBudget, detail)
	case strings.Contains(detail, "Insufficient cpu"), strings.Contains(detail, "Insufficient memory"):
		return fmt.Sprintf("could not be scheduled onto any node — the runner class requests more "+
			"CPU/memory than any node has free. Choose a smaller runner class or add cluster capacity. "+
			"(kubernetes reported: %s)", detail)
	case strings.Contains(detail, "FailedScheduling"):
		return fmt.Sprintf("could not be scheduled onto any node. "+
			"Check node taints, selectors, and the RuntimeClass. (kubernetes reported: %s)", detail)
	default:
		return "pod produced no logs: " + detail
	}
}

// podFailureDetail explains why a pod that exists produced no usable logs: first
// the runner container's own waiting/terminated state (the precise cause), then
// the most recent Warning event for the job or its pods. Returns "" when nothing
// informative is available, so a genuinely silent failure falls back to the exit
// code alone.
func (r *KubernetesRuntime) podFailureDetail(pod *corev1.Pod, jobName string) string {
	if s := containerStateReason(pod); s != "" {
		return s
	}
	if s := podStatusReason(pod); s != "" {
		return s
	}
	return r.jobFailureReason(jobName)
}

// podStatusReason surfaces a failure recorded on the pod's own status rather than
// the container's — most importantly Eviction, which the kubelet reports by
// setting Status.Reason ("Evicted") and Status.Message when it removes the pod out
// from under a still-Running container (a memory-backed /tmp that exceeds its
// SizeLimit, or node memory pressure). The container status then shows no
// terminated state, so containerStateReason misses it. Returns "" for a pod that
// failed by an ordinary container exit, leaving that to the exit code.
func podStatusReason(pod *corev1.Pod) string {
	if pod.Status.Phase != corev1.PodFailed && strings.TrimSpace(pod.Status.Reason) == "" {
		return ""
	}
	return joinReasonMessage(pod.Status.Reason, pod.Status.Message)
}

// containerStateReason extracts a failure reason from the runner container's
// status when it produced no logs: a Waiting reason (CreateContainerError,
// ImagePullBackOff, ErrImagePull, a kata sandbox that failed to boot) or an
// abnormal Terminated state (OOMKilled, ContainerCannotRun, StartError, or any
// terminated container carrying a message). A plain non-zero "Error" exit with no
// message is left to the exit code and reported as "" so a normal command failure
// is not dressed up as an infrastructure error.
func containerStateReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "runner" {
			continue
		}
		if w := cs.State.Waiting; w != nil && strings.TrimSpace(w.Reason) != "" {
			return joinReasonMessage(w.Reason, w.Message)
		}
		if t := cs.State.Terminated; t != nil {
			reason := strings.TrimSpace(t.Reason)
			if strings.TrimSpace(t.Message) != "" || (reason != "" && reason != "Error" && reason != "Completed") {
				return joinReasonMessage(t.Reason, t.Message)
			}
		}
	}
	return ""
}

// runnerStarted reports whether the runner container has left ContainerCreating —
// it is running or has already terminated. The command timeout starts from this
// point, and a Job that hits its ActiveDeadlineSeconds without ever reaching it is a
// startup failure, not a command timeout. A nil pod (never scheduled) is not started.
func runnerStarted(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "runner" {
			return cs.State.Running != nil || cs.State.Terminated != nil
		}
	}
	return false
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

// CreateVolume provisions a PersistentVolumeClaim to back a shared workspace. The
// StorageClass (FORGE_VOLUME_STORAGE_CLASS, empty = cluster default) decides the
// backing medium — point it at a RAM/tmpfs-backed class to keep the volume in
// memory. AccessMode defaults to ReadWriteOnce (FORGE_VOLUME_ACCESS_MODE); set it to
// ReadWriteMany on a class that supports it if a run's steps span nodes. An
// already-existing PVC is treated as success so a retried create is idempotent.
func (r *KubernetesRuntime) CreateVolume(ctx context.Context, spec VolumeSpec) error {
	size := resource.MustParse(fmt.Sprintf("%dMi", spec.SizeMB))
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.ResourceName,
			Namespace: r.namespace,
			Labels:    map[string]string{"app": "forge", "component": "workspace"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{parseAccessMode(volumeAccessMode)},
			StorageClassName: storageClassName(),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	created, err := r.client.CoreV1().PersistentVolumeClaims(r.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // idempotent: a retried create-volume step must not fail
		}
		return fmt.Errorf("create pvc: %w", err)
	}
	// Fail fast when the PVC resolves to no StorageClass. Forge treats an empty
	// FORGE_VOLUME_STORAGE_CLASS as "use the cluster default"; the DefaultStorageClass
	// admission plugin then stamps the default class name onto the created object. A
	// still-empty class here means the cluster has no default StorageClass (and none
	// was configured), so the PVC can never bind and any step that later mounts it
	// fails to schedule with an opaque "pod has unbound immediate
	// PersistentVolumeClaims". Surface that at the create-volume step instead, with an
	// actionable message, and delete the orphan PVC so it does not linger unbound.
	if created.Spec.StorageClassName == nil || *created.Spec.StorageClassName == "" {
		_ = r.client.CoreV1().PersistentVolumeClaims(r.namespace).Delete(ctx, spec.ResourceName, metav1.DeleteOptions{})
		return fmt.Errorf("shared volume %q cannot be provisioned: no StorageClass — the cluster has no default StorageClass and FORGE_VOLUME_STORAGE_CLASS is unset; set forge.volumes.storageClass to a provisionable class", spec.ResourceName)
	}
	return nil
}

// DeleteVolume removes the PVC backing a shared workspace. A missing PVC is treated
// as success so teardown is idempotent.
func (r *KubernetesRuntime) DeleteVolume(ctx context.Context, resourceName string) error {
	if err := r.client.CoreV1().PersistentVolumeClaims(r.namespace).Delete(ctx, resourceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pvc: %w", err)
	}
	return nil
}

// VolumeStatus reports whether the PVC backing a shared workspace has provisioned.
// A create-volume workflow step polls it so a slow dynamic provisioner (Ceph RBD is
// ~35s here) finishes before a downstream step mounts the volume — otherwise, since
// WaitForFirstConsumer defers provisioning until the mounting pod schedules, that
// latency lands inside the exec Job's ActiveDeadlineSeconds (the command timeout,
// default 30s) and the pod is killed mid-bind ("pod was removed before its logs
// could be read").
func (r *KubernetesRuntime) VolumeStatus(ctx context.Context, resourceName string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	pvc, err := r.client.CoreV1().PersistentVolumeClaims(r.namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The claim we created is gone (torn down, or never persisted) — there is no
			// volume to become ready, so fail rather than poll forever.
			return volumeReadyFailed, "volume claim not found", nil
		}
		return "", "", fmt.Errorf("get pvc: %w", err)
	}

	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		return volumeReadyReady, "", nil
	case corev1.ClaimLost:
		return volumeReadyFailed, "volume claim lost its backing PersistentVolume", nil
	}

	// Still Pending. A WaitForFirstConsumer StorageClass deliberately holds the claim
	// unbound until a consumer pod exists — which only happens at the mount step, never
	// here — so there is nothing to wait for: report ready and let the mount trigger
	// binding, exactly as before this endpoint existed. Otherwise it is genuinely still
	// provisioning; surface the latest Warning as detail for a caller inspecting it.
	deferred, warning := r.pvcEventState(ctx, resourceName)
	if deferred {
		return volumeReadyReady, "", nil
	}
	return volumeReadyProvisioning, warning, nil
}

// pvcEventState scans a PVC's events once, returning whether its StorageClass defers
// binding to first consumer (the WaitForFirstConsumer event — nothing to wait for)
// and the latest Warning reason (e.g. ProvisioningFailed) to surface as detail. The
// field selector narrows to this PVC on a real API server; the client-side filter
// keeps it correct under the fake clientset, which ignores field selectors.
func (r *KubernetesRuntime) pvcEventState(ctx context.Context, pvcName string) (deferred bool, warning string) {
	evs, err := r.client.CoreV1().Events(r.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + pvcName,
	})
	if err != nil {
		return false, ""
	}
	var best *corev1.Event
	for i := range evs.Items {
		e := &evs.Items[i]
		if e.InvolvedObject.Kind != "PersistentVolumeClaim" || e.InvolvedObject.Name != pvcName {
			continue
		}
		if e.Reason == "WaitForFirstConsumer" {
			deferred = true
		}
		if e.Type == corev1.EventTypeWarning && (best == nil || eventTime(e).After(eventTime(best))) {
			best = e
		}
	}
	if best != nil {
		warning = joinReasonMessage(best.Reason, best.Message)
	}
	return deferred, warning
}

// parseAccessMode maps the configured access-mode string to the corev1 constant,
// defaulting to ReadWriteOnce for an unset or unrecognised value.
func parseAccessMode(mode string) corev1.PersistentVolumeAccessMode {
	switch mode {
	case "ReadWriteMany":
		return corev1.ReadWriteMany
	case "ReadOnlyMany":
		return corev1.ReadOnlyMany
	default:
		return corev1.ReadWriteOnce
	}
}

// storageClassName returns the configured StorageClass pointer, or nil to use the
// cluster's default StorageClass.
func storageClassName() *string {
	if volumeStorageClass == "" {
		return nil
	}
	return &volumeStorageClass
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
