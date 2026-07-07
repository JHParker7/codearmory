package main

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func stdRunnerSpec() RunnerClass {
	return RunnerClass{Name: "standard", MemoryMB: 256, CPUMillicores: 500, PidsLimit: 64, TmpfsMB: 64, Enabled: true}
}

// TestBuildJob_NonRootSecurityContext guards the fix for root-default images
// (alpine, ubuntu, …): RunAsNonRoot must be paired with a pinned non-root
// RunAsUser/RunAsGroup, otherwise the kubelet blocks the container at admission
// ("container has runAsNonRoot and image will run as root") and nothing runs.
func TestBuildJob_NonRootSecurityContext(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	exec := Execution{ExecutionID: "exec-1", Image: "alpine:3.19", Command: []string{"ls", "-la"}, TimeoutSecs: 30}

	job := r.buildJob(exec, stdRunnerSpec())
	pod := job.Spec.Template.Spec

	if pod.SecurityContext == nil {
		t.Fatal("pod security context is nil")
	}
	if got := pod.SecurityContext.RunAsUser; got == nil || *got != sandboxUID {
		t.Errorf("pod RunAsUser = %v, want %d", got, sandboxUID)
	}
	if got := pod.SecurityContext.RunAsNonRoot; got == nil || !*got {
		t.Error("pod RunAsNonRoot should be true")
	}
	if got := pod.SecurityContext.FSGroup; got == nil || *got != sandboxUID {
		t.Errorf("pod FSGroup = %v, want %d", got, sandboxUID)
	}

	if len(pod.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(pod.Containers))
	}
	sc := pod.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container security context is nil")
	}
	// The crux of the bug fix.
	if sc.RunAsUser == nil || *sc.RunAsUser != sandboxUID {
		t.Errorf("container RunAsUser = %v, want %d (RunAsNonRoot without it blocks root-default images)", sc.RunAsUser, sandboxUID)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != sandboxUID {
		t.Errorf("container RunAsGroup = %v, want %d", sc.RunAsGroup, sandboxUID)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("container RunAsNonRoot should be true")
	}
	// Other hardening must remain intact.
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("container ReadOnlyRootFilesystem should be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("container AllowPrivilegeEscalation should be false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container should drop ALL capabilities, got %+v", sc.Capabilities)
	}
}

// TestBuildJob_EgressProxyEnv verifies the exec pod gets the egress-proxy env vars
// when FORGE_EGRESS_PROXY is set: the NetworkPolicy confines exec pods to the proxy,
// so without HTTP(S)_PROXY a clone dials the host directly and is blocked, failing
// right after "Cloning into…". A user-set value in exec.Env must win.
func TestBuildJob_EgressProxyEnv(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge", egressProxy: "http://egress-proxy:3128"}
	exec := Execution{
		ExecutionID: "exec-1",
		Image:       "alpine:3.19",
		Command:     []string{"sh", "-c", "git clone $GIT_CLONE_URL ."},
		TimeoutSecs: 30,
		Env:         map[string]string{"NO_PROXY": "example.internal"}, // user override
	}

	env := map[string]string{}
	for _, e := range r.buildJob(exec, stdRunnerSpec()).Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}

	if got := env["HTTPS_PROXY"]; got != "http://egress-proxy:3128" {
		t.Errorf("HTTPS_PROXY = %q, want the egress proxy URL", got)
	}
	if got := env["HTTP_PROXY"]; got != "http://egress-proxy:3128" {
		t.Errorf("HTTP_PROXY = %q, want the egress proxy URL", got)
	}
	// User-set NO_PROXY wins over the injected default.
	if got := env["NO_PROXY"]; got != "example.internal" {
		t.Errorf("NO_PROXY = %q, want the user override to win", got)
	}

	// With no proxy configured (e.g. kata), nothing is injected.
	bare := &KubernetesRuntime{namespace: "forge"}
	for _, e := range bare.buildJob(exec, stdRunnerSpec()).Spec.Template.Spec.Containers[0].Env {
		if e.Name == "HTTP_PROXY" || e.Name == "HTTPS_PROXY" {
			t.Errorf("no proxy configured but %s was injected", e.Name)
		}
	}
}

// TestBuildJob_PrivilegedKata verifies the kata (VM-isolated) opt-in: a runner
// class with Privileged runs the job as root with a writable rootfs and privilege
// escalation allowed so package managers work — the microVM is the boundary.
func TestBuildJob_PrivilegedKata(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge", kernelIsolated: true}
	spec := stdRunnerSpec()
	spec.Privileged = true
	exec := Execution{ExecutionID: "exec-1", Image: "ubuntu:22.04", Command: []string{"apt-get", "update"}, TimeoutSecs: 30}

	pod := r.buildJob(exec, spec).Spec.Template.Spec

	if got := pod.SecurityContext.RunAsNonRoot; got == nil || *got {
		t.Error("pod RunAsNonRoot should be false when privileged")
	}
	if got := pod.SecurityContext.RunAsUser; got == nil || *got != 0 {
		t.Errorf("pod RunAsUser = %v, want 0", got)
	}
	sc := pod.Containers[0].SecurityContext
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Errorf("container RunAsUser = %v, want 0", sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || *sc.RunAsNonRoot {
		t.Error("container RunAsNonRoot should be false when privileged")
	}
	if sc.ReadOnlyRootFilesystem == nil || *sc.ReadOnlyRootFilesystem {
		t.Error("container ReadOnlyRootFilesystem should be false when privileged (apt needs a writable rootfs)")
	}
	if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
		t.Error("container AllowPrivilegeEscalation should be true when privileged")
	}
	if sc.Capabilities != nil && len(sc.Capabilities.Drop) > 0 {
		t.Errorf("privileged container should not drop capabilities, got %+v", sc.Capabilities)
	}
}

// TestBuildJob_PrivilegedIgnoredWithoutVMIsolation is the security gate: a
// Privileged runner class on a non-VM-isolated (plain kubernetes/runc) backend
// MUST stay fully locked down — root in a shared-kernel container is an escape
// risk, so the flag is dropped at runtime regardless of what the class requests.
func TestBuildJob_PrivilegedIgnoredWithoutVMIsolation(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge", kernelIsolated: false}
	spec := stdRunnerSpec()
	spec.Privileged = true
	exec := Execution{ExecutionID: "exec-1", Image: "ubuntu:22.04", Command: []string{"apt-get", "update"}, TimeoutSecs: 30}

	pod := r.buildJob(exec, spec).Spec.Template.Spec

	if got := pod.SecurityContext.RunAsNonRoot; got == nil || !*got {
		t.Error("pod RunAsNonRoot must remain true on a non-VM backend even with privileged set")
	}
	if got := pod.SecurityContext.RunAsUser; got == nil || *got != sandboxUID {
		t.Errorf("pod RunAsUser = %v, want %d (privileged must be ignored off kata)", got, sandboxUID)
	}
	sc := pod.Containers[0].SecurityContext
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("container ReadOnlyRootFilesystem must remain true on a non-VM backend")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("container AllowPrivilegeEscalation must remain false on a non-VM backend")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container must still drop ALL capabilities on a non-VM backend, got %+v", sc.Capabilities)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != sandboxUID {
		t.Errorf("container RunAsUser = %v, want %d", sc.RunAsUser, sandboxUID)
	}
}

// TestBuildJob_PlumbsExecutionFields checks command/image/naming/timeout and the
// service-account-token isolation are wired into the Job.
func TestBuildJob_PlumbsExecutionFields(t *testing.T) {
	r := &KubernetesRuntime{namespace: "ns-test"}
	exec := Execution{
		ExecutionID: "abc-123",
		Image:       "python:3.12",
		Command:     []string{"python", "-c", "print(1)"},
		Env:         map[string]string{"K": "v"},
		TimeoutSecs: 42,
	}

	job := r.buildJob(exec, stdRunnerSpec())
	pod := job.Spec.Template.Spec

	if job.Name != "forge-abc-123" {
		t.Errorf("job name = %q, want forge-abc-123", job.Name)
	}
	if job.Namespace != "ns-test" {
		t.Errorf("job namespace = %q, want ns-test", job.Namespace)
	}
	if pod.Containers[0].Image != "python:3.12" {
		t.Errorf("image = %q", pod.Containers[0].Image)
	}
	if cmd := pod.Containers[0].Command; len(cmd) != 3 || cmd[0] != "python" {
		t.Errorf("command = %v", cmd)
	}
	// ActiveDeadlineSeconds is the command timeout plus the startup grace: it counts
	// from pod creation, so it must leave room for ContainerCreating (volume bind +
	// image pull) on top of the command's own budget.
	if want := int64(42) + podStartupGraceSecs; job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != want {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d", job.Spec.ActiveDeadlineSeconds, want)
	}
	if amt := pod.AutomountServiceAccountToken; amt == nil || *amt {
		t.Error("AutomountServiceAccountToken should be false (no cluster credentials in the sandbox)")
	}
}

// TestNoPodError checks the precedence of the no-pod diagnostic: an events-derived
// reason wins, then the job's failure-condition message, then the generic hint.
func TestNoPodError(t *testing.T) {
	const want = `RuntimeClass "kata-qemu" not found`
	if got := noPodError(want, "backoff limit").Error(); !strings.Contains(got, want) {
		t.Errorf("event reason should win, got %q", got)
	}
	if got := noPodError("", "Job has reached the specified backoff limit").Error(); !strings.Contains(got, "backoff limit") {
		t.Errorf("condition message should be used when no event reason, got %q", got)
	}
	if got := noPodError("", "").Error(); !strings.Contains(got, "RuntimeClass") {
		t.Errorf("generic hint should mention RuntimeClass, got %q", got)
	}
}

// TestClarifyNoLogFailure verifies the opaque Kubernetes reasons behind a no-logs
// failure are rewritten into plain-language, actionable messages, while an
// unrecognised reason is passed through under the original prefix.
func TestClarifyNoLogFailure(t *testing.T) {
	const budget = int64(330)
	cases := []struct {
		name   string
		detail string
		want   []string // all substrings must be present
	}{
		{
			name:   "still ContainerCreating reads as a startup timeout",
			detail: "ContainerCreating",
			want:   []string{"timed out after 330s", "still starting", "ContainerCreating"},
		},
		{
			name:   "ContainerStatusUnknown reads as a startup timeout",
			detail: "ContainerStatusUnknown: The container could not be located when the pod was terminated",
			want:   []string{"timed out after 330s", "still starting", "FORGE_POD_STARTUP_GRACE_SECS"},
		},
		{
			name:   "insufficient cpu names the runner class as the cause",
			detail: "FailedScheduling: 0/3 nodes are available: 3 Insufficient cpu.",
			want:   []string{"could not be scheduled", "runner class requests more", "Insufficient cpu"},
		},
		{
			name:   "generic scheduling failure keeps the raw detail",
			detail: "FailedScheduling: 0/3 nodes are available: no node has the kata-fc handler",
			want:   []string{"could not be scheduled", "RuntimeClass", "kata-fc"},
		},
		{
			name:   "unrecognised reason passes through under the original prefix",
			detail: "OOMKilled",
			want:   []string{"pod produced no logs: OOMKilled"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clarifyNoLogFailure(tc.detail, budget)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("clarifyNoLogFailure(%q) = %q, want substring %q", tc.detail, got, w)
				}
			}
		})
	}
}

// TestNoPodError_InsufficientResources checks the pod-never-scheduled path also
// names the runner class when the scheduler reported insufficient CPU/memory.
func TestNoPodError_InsufficientResources(t *testing.T) {
	got := noPodError("FailedScheduling: 0/3 nodes are available: 3 Insufficient cpu.", "").Error()
	if !strings.Contains(got, "runner class") || !strings.Contains(got, "Insufficient cpu") {
		t.Errorf("noPodError = %q, want the runner-class hint plus the raw reason", got)
	}
}

// TestJobFailureReason verifies forge surfaces the real cause of a VM/kata runner
// that never scheduled a pod — the FailedCreate Warning event naming a missing
// RuntimeClass — rather than the opaque "no pod found" message. It also confirms
// Normal events are ignored and the most recent Warning wins.
func TestJobFailureReason(t *testing.T) {
	const jobName = "forge-exec-1"
	t0 := time.Unix(1_700_000_000, 0)

	cs := fake.NewSimpleClientset(
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-normal", Namespace: "forge"},
			InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: jobName},
			Type:           corev1.EventTypeNormal,
			Reason:         "SuccessfulCreate",
			Message:        "Created pod: forge-exec-1-abcde",
			LastTimestamp:  metav1.NewTime(t0.Add(10 * time.Second)),
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-warn", Namespace: "forge"},
			InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: jobName},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedCreate",
			Message:        `Error creating: pods "forge-exec-1-" is forbidden: pod rejected: RuntimeClass "kata-qemu" not found`,
			LastTimestamp:  metav1.NewTime(t0.Add(5 * time.Second)),
		},
		// An unrelated job's warning must not bleed into this execution.
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-other", Namespace: "forge"},
			InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: "forge-other"},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedCreate",
			Message:        "some other job's failure",
			LastTimestamp:  metav1.NewTime(t0.Add(20 * time.Second)),
		},
	)
	r := &KubernetesRuntime{client: cs, namespace: "forge"}

	got := r.jobFailureReason(jobName)
	if !strings.Contains(got, "FailedCreate") || !strings.Contains(got, `RuntimeClass "kata-qemu" not found`) {
		t.Errorf("jobFailureReason = %q, want the FailedCreate/RuntimeClass reason", got)
	}
	if strings.Contains(got, "some other job") {
		t.Errorf("jobFailureReason leaked an unrelated job's event: %q", got)
	}
}

// TestJobFailureReason_NoEvents returns empty when nothing informative exists, so
// noPodError falls back to the condition message or generic hint.
func TestJobFailureReason_NoEvents(t *testing.T) {
	r := &KubernetesRuntime{client: fake.NewSimpleClientset(), namespace: "forge"}
	if got := r.jobFailureReason("forge-exec-1"); got != "" {
		t.Errorf("jobFailureReason with no events = %q, want empty", got)
	}
}

// Aggregated/series events leave the legacy LastTimestamp zero and populate
// EventTime instead; jobFailureReason must still pick the most recent Warning by
// EventTime rather than returning whichever event happened to be listed first.
func TestJobFailureReason_UsesEventTimeWhenLastTimestampZero(t *testing.T) {
	const jobName = "forge-exec-1"
	t0 := time.Unix(1_700_000_000, 0)

	cs := fake.NewSimpleClientset(
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-old", Namespace: "forge"},
			InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: jobName},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedScheduling",
			Message:        "0/3 nodes are available",
			EventTime:      metav1.NewMicroTime(t0.Add(1 * time.Second)),
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-new", Namespace: "forge"},
			InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: jobName},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedCreate",
			Message:        `RuntimeClass "kata-qemu" not found`,
			EventTime:      metav1.NewMicroTime(t0.Add(5 * time.Second)),
		},
	)
	r := &KubernetesRuntime{client: cs, namespace: "forge"}

	got := r.jobFailureReason(jobName)
	if !strings.Contains(got, "kata-qemu") {
		t.Errorf("jobFailureReason = %q, want the most-recent (EventTime) FailedCreate reason", got)
	}
}

// A Warning event with a Reason but an empty Message must not yield a dangling
// "Reason:" with a trailing colon.
func TestJobFailureReason_NoDanglingColon(t *testing.T) {
	const jobName = "forge-exec-1"
	cs := fake.NewSimpleClientset(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev", Namespace: "forge"},
		InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: jobName},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackoffLimitExceeded",
		Message:        "",
		LastTimestamp:  metav1.NewTime(time.Unix(1_700_000_000, 0)),
	})
	r := &KubernetesRuntime{client: cs, namespace: "forge"}

	if got := r.jobFailureReason(jobName); got != "BackoffLimitExceeded" {
		t.Errorf("jobFailureReason = %q, want %q with no trailing colon", got, "BackoffLimitExceeded")
	}
}

// TestBuildJob_TmpfsMemoryBacked guards that /tmp is a memory-backed emptyDir.
// A default node-backed emptyDir cannot be shared into a Firecracker (kata-fc)
// microVM — Firecracker has no virtio-fs/9p — so the container never starts and
// the execution fails with no pod logs. Memory-backed matches the docker runtime's
// tmpfs and works under every VMM.
func TestBuildJob_TmpfsMemoryBacked(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	exec := Execution{ExecutionID: "exec-1", Image: "alpine:3.19", Command: []string{"true"}, TimeoutSecs: 30}

	vols := r.buildJob(exec, stdRunnerSpec()).Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].Name != "tmp" {
		t.Fatalf("want a single 'tmp' volume, got %+v", vols)
	}
	ed := vols[0].EmptyDir
	if ed == nil {
		t.Fatal("tmp volume is not an emptyDir")
	}
	if ed.Medium != corev1.StorageMediumMemory {
		t.Errorf("tmp emptyDir medium = %q, want Memory (required for Firecracker/kata-fc)", ed.Medium)
	}
	if ed.SizeLimit == nil || ed.SizeLimit.IsZero() {
		t.Error("tmp emptyDir should carry a SizeLimit from TmpfsMB")
	}
}

// TestContainerStateReason checks the runner container's state is turned into a
// useful reason for the no-logs path: a Waiting reason and abnormal Terminated
// states surface, while a plain non-zero "Error" exit is left to the exit code.
func TestContainerStateReason(t *testing.T) {
	mk := func(state corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "runner", State: state},
		}}}
	}

	cases := []struct {
		name  string
		pod   *corev1.Pod
		want  string // substring; "" means expect empty
		empty bool
	}{
		{
			name: "waiting reason surfaces",
			pod:  mk(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerError", Message: "failed to create shim: boot microVM"}}),
			want: "CreateContainerError",
		},
		{
			name: "oomkilled surfaces",
			pod:  mk(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}),
			want: "OOMKilled",
		},
		{
			name:  "plain error exit is left to the exit code",
			pod:   mk(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}),
			empty: true,
		},
		{
			name:  "no runner container",
			pod:   &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "other"}}}},
			empty: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := containerStateReason(tc.pod)
			if tc.empty {
				if got != "" {
					t.Errorf("containerStateReason = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("containerStateReason = %q, want substring %q", got, tc.want)
			}
		})
	}
}

// TestJobFailureReason_IncludesPodEvents verifies the diagnostic also reads pod
// events — FailedCreatePodSandBox is emitted on the pod, not the job, and is the
// real cause when a kata microVM fails to boot. An unrelated job's pod event must
// not leak in.
func TestJobFailureReason_IncludesPodEvents(t *testing.T) {
	const jobName = "forge-exec-1"
	t0 := time.Unix(1_700_000_000, 0)

	cs := fake.NewSimpleClientset(
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-pod", Namespace: "forge"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "forge-exec-1-abcde"},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedCreatePodSandBox",
			Message:        "Failed to create pod sandbox: firecracker does not support filesystem sharing",
			LastTimestamp:  metav1.NewTime(t0.Add(5 * time.Second)),
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-other-pod", Namespace: "forge"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "forge-exec-12-zzzzz"},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedCreatePodSandBox",
			Message:        "a different execution's failure",
			LastTimestamp:  metav1.NewTime(t0.Add(20 * time.Second)),
		},
	)
	r := &KubernetesRuntime{client: cs, namespace: "forge"}

	got := r.jobFailureReason(jobName)
	if !strings.Contains(got, "FailedCreatePodSandBox") || !strings.Contains(got, "filesystem sharing") {
		t.Errorf("jobFailureReason = %q, want the pod's FailedCreatePodSandBox reason", got)
	}
	if strings.Contains(got, "different execution") {
		t.Errorf("jobFailureReason leaked another execution's pod event: %q", got)
	}
}

// TestPodFailureDetail checks the container state wins over events, and that the
// event lookup is the fallback when the container carries no reason.
func TestPodFailureDetail(t *testing.T) {
	const jobName = "forge-exec-1"
	cs := fake.NewSimpleClientset(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev-pod", Namespace: "forge"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "forge-exec-1-abcde"},
		Type:           corev1.EventTypeWarning,
		Reason:         "FailedScheduling",
		Message:        "0/3 nodes are available: no node has the kata-fc handler",
		LastTimestamp:  metav1.NewTime(time.Unix(1_700_000_000, 0)),
	})
	r := &KubernetesRuntime{client: cs, namespace: "forge"}

	withState := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "runner", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
	}}}
	if got := r.podFailureDetail(withState, jobName); !strings.Contains(got, "ImagePullBackOff") {
		t.Errorf("podFailureDetail should prefer the container state, got %q", got)
	}

	noState := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "runner"}}}}
	if got := r.podFailureDetail(noState, jobName); !strings.Contains(got, "FailedScheduling") {
		t.Errorf("podFailureDetail should fall back to events, got %q", got)
	}
}

// TestWaitAndCollect_UsesLastPodSnapshotWhenPodVanishes guards the kata no-pod
// regression: a pod that ran and failed, then was evicted or garbage-collected
// before the job was observed terminal, leaves findPod() empty at `done`. Without
// the retained snapshot the run reports the job-level exit guess and the opaque
// "no pod found" message; with it, the pod's real terminated exit code survives.
func TestWaitAndCollect_UsesLastPodSnapshotWhenPodVanishes(t *testing.T) {
	const execID = "exec-1"
	const jobName = "forge-" + execID

	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "forge"},
		Status:     batchv1.JobStatus{Failed: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abcde",
			Namespace: "forge",
			Labels:    map[string]string{"execution-id": execID},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "runner",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "StartError", Message: "failed to start microVM", ExitCode: 128,
			}},
		}}},
	}

	cs := fake.NewSimpleClientset(failedJob)
	// The pod is visible on the first List (the snapshot captured while polling)
	// and gone on every later List (evicted/GC'd before the done lookup).
	var podLists int
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		podLists++
		if podLists == 1 {
			return true, &corev1.PodList{Items: []corev1.Pod{*pod}}, nil
		}
		return true, &corev1.PodList{}, nil
	})

	r := &KubernetesRuntime{client: cs, namespace: "forge"}
	exec := Execution{ExecutionID: execID, TimeoutSecs: 30}

	res, err := r.waitAndCollect(context.Background(), exec, jobName)
	if err != nil {
		t.Fatalf("waitAndCollect error: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 128 {
		t.Errorf("exit code = %v, want 128 recovered from the last pod snapshot (got the job-level guess instead)", res.ExitCode)
	}
	// The cause is recovered from the snapshot, not leaked as a raw "pods ... not
	// found" stream error nor flattened to the generic no-pod message.
	if !strings.Contains(res.Stderr, "StartError") || !strings.Contains(res.Stderr, "microVM") {
		t.Errorf("stderr = %q, want the snapshot's StartError detail", res.Stderr)
	}
	if strings.Contains(res.Stderr, "no pod found") || strings.Contains(res.Stderr, "stream pod logs") {
		t.Errorf("stderr leaked a generic/raw message despite a captured snapshot: %q", res.Stderr)
	}
	if podLists < 2 {
		t.Errorf("expected the pod to be listed while polling and again at done, got %d lists", podLists)
	}
}

// TestPodStatusReason checks pod-level failures are surfaced when the container
// status carries no terminated state — chiefly Eviction, which the kubelet records
// on the pod (Status.Reason "Evicted") when a memory-backed /tmp exceeds its
// SizeLimit or the node is under memory pressure. An ordinary running/succeeded pod
// yields nothing.
func TestPodStatusReason(t *testing.T) {
	evicted := &corev1.Pod{Status: corev1.PodStatus{
		Phase:   corev1.PodFailed,
		Reason:  "Evicted",
		Message: "Pod ephemeral local storage usage exceeds the total limit of containers",
	}}
	if got := podStatusReason(evicted); !strings.Contains(got, "Evicted") {
		t.Errorf("podStatusReason = %q, want the Evicted reason", got)
	}

	running := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if got := podStatusReason(running); got != "" {
		t.Errorf("podStatusReason = %q, want empty for a running pod", got)
	}
}

// TestPodFailureDetail_EvictedPodLevel verifies an evicted pod whose container
// shows no terminated state still surfaces the eviction (via the pod status),
// rather than falling through to the events lookup.
func TestPodFailureDetail_EvictedPodLevel(t *testing.T) {
	r := &KubernetesRuntime{client: fake.NewSimpleClientset(), namespace: "forge"}
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase:             corev1.PodFailed,
			Reason:            "Evicted",
			Message:           "The node was low on resource: memory",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "runner"}}, // no terminated/waiting state
		},
	}
	if got := r.podFailureDetail(pod, "forge-exec-1"); !strings.Contains(got, "Evicted") {
		t.Errorf("podFailureDetail = %q, want the pod-level Evicted reason", got)
	}
}

// TestPodRemovedError checks the last-resort diagnostic for a pod that vanished
// before its logs could be read names eviction (the common kata cause), so a run
// never surfaces the raw "stream pod logs: pods ... not found" Kubernetes error.
func TestPodRemovedError(t *testing.T) {
	got := podRemovedError().Error()
	if !strings.Contains(got, "removed before its logs could be read") || !strings.Contains(got, "evicted") {
		t.Errorf("podRemovedError = %q, want a clean evicted/removed message", got)
	}
	if strings.Contains(got, "stream pod logs") || strings.Contains(got, "not found") {
		t.Errorf("podRemovedError leaked a raw stream error: %q", got)
	}
}

// TestParsePodMetricsMemoryMB checks PodMetrics payloads are summed across
// containers and converted to whole MiB, with ok=false for empty/garbage input.
func TestParsePodMetricsMemoryMB(t *testing.T) {
	// 184320Ki = 188743680 bytes = exactly 180 MiB.
	if mb, ok := parsePodMetricsMemoryMB([]byte(`{"containers":[{"name":"runner","usage":{"cpu":"5m","memory":"184320Ki"}}]}`)); !ok || mb != 180 {
		t.Errorf("single container = %d ok=%v, want 180 true", mb, ok)
	}
	// Multiple containers are summed: 100Mi + 50Mi = 150Mi.
	if mb, ok := parsePodMetricsMemoryMB([]byte(`{"containers":[{"usage":{"memory":"100Mi"}},{"usage":{"memory":"50Mi"}}]}`)); !ok || mb != 150 {
		t.Errorf("summed = %d ok=%v, want 150 true", mb, ok)
	}
	if _, ok := parsePodMetricsMemoryMB([]byte(`{}`)); ok {
		t.Error("empty payload should be ok=false")
	}
	if _, ok := parsePodMetricsMemoryMB([]byte(`not json`)); ok {
		t.Error("garbage should be ok=false")
	}
}

// TestWaitAndCollect_RecordsPeakMemory verifies the metrics sample taken while
// polling is recorded as the run's MemoryUsedMB, and that the right pod is sampled.
func TestWaitAndCollect_RecordsPeakMemory(t *testing.T) {
	const execID = "exec-mem"
	const jobName = "forge-" + execID

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "forge"},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abcde",
			Namespace: "forge",
			Labels:    map[string]string{"execution-id": execID},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "runner",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}

	r := &KubernetesRuntime{
		client:    fake.NewSimpleClientset(job, pod),
		namespace: "forge",
		podMemoryMB: func(_ context.Context, name string) (int64, bool) {
			if name != pod.Name {
				t.Errorf("sampled wrong pod %q, want %q", name, pod.Name)
			}
			return 200, true
		},
	}

	res, err := r.waitAndCollect(context.Background(), Execution{ExecutionID: execID, TimeoutSecs: 30}, jobName)
	if err != nil {
		t.Fatalf("waitAndCollect: %v", err)
	}
	if res.MemoryUsedMB == nil || *res.MemoryUsedMB != 200 {
		t.Errorf("MemoryUsedMB = %v, want 200 from the metrics sample", res.MemoryUsedMB)
	}
}

// TestWaitAndCollect_NoMetricsLeavesMemoryNil guards the nil-fetcher path (tests
// and clusters without metrics-server): no sample means MemoryUsedMB stays nil.
func TestWaitAndCollect_NoMetricsLeavesMemoryNil(t *testing.T) {
	const execID = "exec-nomem"
	const jobName = "forge-" + execID
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "forge"}, Status: batchv1.JobStatus{Succeeded: 1}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-x", Namespace: "forge", Labels: map[string]string{"execution-id": execID}}}
	r := &KubernetesRuntime{client: fake.NewSimpleClientset(job, pod), namespace: "forge"} // podMemoryMB nil

	res, err := r.waitAndCollect(context.Background(), Execution{ExecutionID: execID, TimeoutSecs: 30}, jobName)
	if err != nil {
		t.Fatalf("waitAndCollect: %v", err)
	}
	if res.MemoryUsedMB != nil {
		t.Errorf("MemoryUsedMB = %v, want nil with no metrics fetcher", *res.MemoryUsedMB)
	}
}

// TestResolveRuntimeClass checks the precedence a kata/kubernetes backend uses to
// pick its RuntimeClass: explicit per-backend config wins, then the legacy
// process-wide env var, then nil (the cluster's default runtime).
func TestResolveRuntimeClass(t *testing.T) {
	t.Setenv("K8S_RUNTIME_CLASS", "from-env")
	if got := resolveRuntimeClass("kata-qemu"); got == nil || *got != "kata-qemu" {
		t.Errorf("per-backend config should win, got %v", got)
	}
	if got := resolveRuntimeClass(""); got == nil || *got != "from-env" {
		t.Errorf("empty config should fall back to K8S_RUNTIME_CLASS, got %v", got)
	}
	t.Setenv("K8S_RUNTIME_CLASS", "")
	if got := resolveRuntimeClass(""); got != nil {
		t.Errorf("no config and no env should be nil (cluster default), got %q", *got)
	}
}

// TestBuildJob_RuntimeClassName guards that a kata/gvisor backend pins the pod's
// RuntimeClassName so the kubelet runs it under the sandboxed runtime, while a
// plain kubernetes backend leaves it nil (the default runc runtime).
func TestBuildJob_RuntimeClassName(t *testing.T) {
	exec := Execution{ExecutionID: "exec-1", Image: "alpine:3.19", Command: []string{"true"}, TimeoutSecs: 30}

	rc := "kata-qemu"
	kata := &KubernetesRuntime{namespace: "forge", runtimeClass: &rc}
	if got := kata.buildJob(exec, stdRunnerSpec()).Spec.Template.Spec.RuntimeClassName; got == nil || *got != "kata-qemu" {
		t.Errorf("RuntimeClassName = %v, want kata-qemu", got)
	}

	plain := &KubernetesRuntime{namespace: "forge"}
	if got := plain.buildJob(exec, stdRunnerSpec()).Spec.Template.Spec.RuntimeClassName; got != nil {
		t.Errorf("RuntimeClassName = %q, want nil for a plain k8s backend", *got)
	}
}

// TestBuildJob_AttachesVolumes checks a shared workspace volume becomes a PVC-backed
// pod volume mounted at its path, with the runner's working directory pinned to the
// mount that sets workdir. This is what lets a git step's checkout be visible (and
// the process start) inside the shared volume.
func TestBuildJob_AttachesVolumes(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	exec := Execution{
		ExecutionID: "exec-1", Image: "alpine:3.19", Command: []string{"true"}, TimeoutSecs: 30,
		Volumes: []VolumeMount{{WorkflowID: "run-1", Name: "workspace", MountPath: "/workspace", Workdir: true}},
	}
	pod := r.buildJob(exec, stdRunnerSpec()).Spec.Template.Spec

	want := volumeResourceName("run-1", "workspace")
	var pvcVolName string
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == want {
			pvcVolName = v.Name
		}
	}
	if pvcVolName == "" {
		t.Fatalf("no pod volume backed by PVC %q; volumes=%+v", want, pod.Volumes)
	}

	c := pod.Containers[0]
	if c.WorkingDir != "/workspace" {
		t.Errorf("WorkingDir = %q, want /workspace", c.WorkingDir)
	}
	mounted := false
	for _, m := range c.VolumeMounts {
		if m.Name == pvcVolName && m.MountPath == "/workspace" {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("PVC volume not mounted at /workspace; mounts=%+v", c.VolumeMounts)
	}
	// The always-present tmpfs /tmp must survive alongside the new mount.
	tmp := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/tmp" {
			tmp = true
		}
	}
	if !tmp {
		t.Error("/tmp mount was dropped when attaching a volume")
	}
}

// TestBuildJob_NoVolumes keeps the default (no shared storage): just the /tmp mount,
// and no working directory override.
func TestBuildJob_NoVolumes(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	exec := Execution{ExecutionID: "exec-1", Image: "alpine:3.19", Command: []string{"true"}, TimeoutSecs: 30}
	c := r.buildJob(exec, stdRunnerSpec()).Spec.Template.Spec.Containers[0]
	if c.WorkingDir != "" {
		t.Errorf("WorkingDir = %q, want empty when no workdir volume", c.WorkingDir)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/tmp" {
		t.Errorf("want only the /tmp mount, got %+v", c.VolumeMounts)
	}
}

// TestKubernetesCreateDeleteVolume exercises the PVC lifecycle against the fake
// clientset: create provisions a correctly-sized PVC, create is idempotent, and
// delete removes it (and is idempotent on a missing PVC).
func TestKubernetesCreateDeleteVolume(t *testing.T) {
	initVolumeConfig()
	// A resolvable StorageClass is required: with the fake clientset (no admission)
	// an empty class leaves the PVC class-less, which CreateVolume now rejects. This
	// mirrors production, where either an explicit class or the cluster default is
	// stamped onto the PVC.
	volumeStorageClass = "test-sc"
	t.Cleanup(func() { volumeStorageClass = "" })
	r := &KubernetesRuntime{client: fake.NewSimpleClientset(), namespace: "forge"}
	ctx := context.Background()
	name := volumeResourceName("run-1", "workspace")

	if err := r.CreateVolume(ctx, VolumeSpec{ResourceName: name, SizeMB: 512, Medium: mediumMemory}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	pvc, err := r.client.CoreV1().PersistentVolumeClaims("forge").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC not created: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "512Mi" {
		t.Errorf("PVC size = %s, want 512Mi", got.String())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("PVC access modes = %v, want [ReadWriteOnce]", pvc.Spec.AccessModes)
	}

	// Idempotent create (already exists) must not error.
	if err := r.CreateVolume(ctx, VolumeSpec{ResourceName: name, SizeMB: 512, Medium: mediumMemory}); err != nil {
		t.Fatalf("idempotent CreateVolume: %v", err)
	}

	if err := r.DeleteVolume(ctx, name); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := r.client.CoreV1().PersistentVolumeClaims("forge").Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("PVC still present after delete")
	}
	// Idempotent delete (already gone) must not error.
	if err := r.DeleteVolume(ctx, name); err != nil {
		t.Fatalf("idempotent DeleteVolume: %v", err)
	}
}

// TestKubernetesCreateVolume_NoStorageClass covers the misconfiguration that
// surfaces as "pod has unbound immediate PersistentVolumeClaims" several steps
// later: no configured class and no cluster default, so the PVC resolves to no
// StorageClass. CreateVolume must reject it up front and leave no orphan PVC.
func TestKubernetesCreateVolume_NoStorageClass(t *testing.T) {
	initVolumeConfig() // resets volumeStorageClass to ""
	r := &KubernetesRuntime{client: fake.NewSimpleClientset(), namespace: "forge"}
	ctx := context.Background()
	name := volumeResourceName("run-1", "workspace")

	err := r.CreateVolume(ctx, VolumeSpec{ResourceName: name, SizeMB: 512, Medium: mediumMemory})
	if err == nil {
		t.Fatal("CreateVolume: want error for missing StorageClass, got nil")
	}
	if !strings.Contains(err.Error(), "StorageClass") {
		t.Errorf("error %q does not mention StorageClass", err.Error())
	}
	if _, gerr := r.client.CoreV1().PersistentVolumeClaims("forge").Get(ctx, name, metav1.GetOptions{}); gerr == nil {
		t.Error("unbindable PVC was left behind; want it deleted")
	}
}

func pvcWithPhase(name string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "forge"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

func pvcEvent(name, reason string, typ string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name + "." + reason, Namespace: "forge"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: name, Namespace: "forge"},
		Reason:         reason,
		Message:        reason + " detail",
		Type:           typ,
		LastTimestamp:  metav1.Now(),
	}
}

func TestKubernetesVolumeStatus(t *testing.T) {
	name := "fv-abc-workspace"
	tests := []struct {
		desc       string
		objs       []runtime.Object
		wantState  string
		wantDetail string // substring; "" = don't care
	}{
		{
			desc:      "bound is ready",
			objs:      []runtime.Object{pvcWithPhase(name, corev1.ClaimBound)},
			wantState: volumeReadyReady,
		},
		{
			desc:      "lost is failed",
			objs:      []runtime.Object{pvcWithPhase(name, corev1.ClaimLost)},
			wantState: volumeReadyFailed,
		},
		{
			// WaitForFirstConsumer binds only when a pod mounts it — nothing to wait for
			// at create time, so report ready and let the mount trigger binding.
			desc:      "pending + WaitForFirstConsumer event is ready",
			objs:      []runtime.Object{pvcWithPhase(name, corev1.ClaimPending), pvcEvent(name, "WaitForFirstConsumer", corev1.EventTypeNormal)},
			wantState: volumeReadyReady,
		},
		{
			desc:       "pending while provisioning keeps waiting, surfaces warning",
			objs:       []runtime.Object{pvcWithPhase(name, corev1.ClaimPending), pvcEvent(name, "ProvisioningFailed", corev1.EventTypeWarning)},
			wantState:  volumeReadyProvisioning,
			wantDetail: "ProvisioningFailed",
		},
		{
			desc:      "missing claim is failed",
			objs:      nil,
			wantState: volumeReadyFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			r := &KubernetesRuntime{client: fake.NewSimpleClientset(tc.objs...), namespace: "forge"}
			state, detail, err := r.VolumeStatus(context.Background(), name)
			if err != nil {
				t.Fatalf("VolumeStatus: unexpected error: %v", err)
			}
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if tc.wantDetail != "" && !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail = %q, want substring %q", detail, tc.wantDetail)
			}
		})
	}
}

// TestRunnerStarted checks the container-start gate that begins the command timeout:
// a ContainerCreating (Waiting) runner has not started — so pod-startup latency
// (volume bind + image pull) is not charged against the command — while a Running or
// already-Terminated runner has. A nil pod and a pod without a runner container have
// not started.
func TestRunnerStarted(t *testing.T) {
	runner := func(s corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "runner", State: s}}}}
	}
	cases := []struct {
		desc string
		pod  *corev1.Pod
		want bool
	}{
		{"ContainerCreating has not started", runner(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}), false},
		{"Running has started", runner(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}), true},
		{"Terminated has started", runner(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}), true},
		{"nil pod has not started", nil, false},
		{"no runner container has not started", &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "other", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}, false},
	}
	for _, tc := range cases {
		if got := runnerStarted(tc.pod); got != tc.want {
			t.Errorf("%s: runnerStarted = %v, want %v", tc.desc, got, tc.want)
		}
	}
}

// TestWaitAndCollect_CommandTimeoutFromContainerStart verifies the command timeout is
// enforced by the poll loop from container start — not left to the Job's larger
// ActiveDeadlineSeconds — so a command that runs long past its budget while the pod
// stays Running (the job never reaching a terminal status) is reported as a timeout.
func TestWaitAndCollect_CommandTimeoutFromContainerStart(t *testing.T) {
	// Poll fast so the ~1s command timeout is observed promptly; a small grace keeps
	// the loop's safety deadline near the command timeout if this ever regresses.
	defer swapDuration(&k8sPollInterval, 5*time.Millisecond)()
	defer swapInt64(&podStartupGraceSecs, 1)()

	const execID = "exec-slow"
	const jobName = "forge-" + execID
	// A running pod whose job never succeeds/fails — only the command timeout ends it.
	runningJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "forge"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName + "-abcde", Namespace: "forge", Labels: map[string]string{"execution-id": execID}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "runner", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	r := &KubernetesRuntime{client: fake.NewSimpleClientset(runningJob, pod), namespace: "forge"}

	res, err := r.waitAndCollect(context.Background(), Execution{ExecutionID: execID, TimeoutSecs: 1}, jobName)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("waitAndCollect err = %v, want a \"timed out after 1s\" error", err)
	}
	if res.ExitCode == nil || *res.ExitCode == 0 {
		t.Errorf("ExitCode = %v, want a non-zero timeout exit", res.ExitCode)
	}
}

// swapDuration sets *p to v and returns a func that restores the old value, for
// defer-scoped overrides of package-level tunables in a test.
func swapDuration(p *time.Duration, v time.Duration) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

// swapInt64 is swapDuration for an int64 tunable.
func swapInt64(p *int64, v int64) func() {
	old := *p
	*p = v
	return func() { *p = old }
}
