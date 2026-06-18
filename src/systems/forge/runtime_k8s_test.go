package main

import "testing"

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
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 42 {
		t.Errorf("ActiveDeadlineSeconds = %v, want 42", job.Spec.ActiveDeadlineSeconds)
	}
	if amt := pod.AutomountServiceAccountToken; amt == nil || *amt {
		t.Error("AutomountServiceAccountToken should be false (no cluster credentials in the sandbox)")
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
