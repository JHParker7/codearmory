package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
)

func init() {
	// The validation and reaper tests read the configured ceilings, which are
	// otherwise zero in a test binary that never runs main.
	initLeaseConfig()
}

// ── Request validation ───────────────────────────────────────────────────────

func TestValidateLeaseRequest_ClampsTimeouts(t *testing.T) {
	req := createLeaseRequest{Image: "alpine:3.19", MaxLifetime: 999999, IdleTimeout: 999999}
	if err := validateLeaseRequest(&req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MaxLifetime != leaseMaxLifetimeSecs {
		t.Errorf("MaxLifetime = %d, want clamped to %d", req.MaxLifetime, leaseMaxLifetimeSecs)
	}
	if req.IdleTimeout != leaseIdleTimeoutSecs {
		t.Errorf("IdleTimeout = %d, want clamped to %d", req.IdleTimeout, leaseIdleTimeoutSecs)
	}
}

func TestValidateLeaseRequest_DefaultsZeroTimeouts(t *testing.T) {
	req := createLeaseRequest{Image: "alpine:3.19"}
	if err := validateLeaseRequest(&req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MaxLifetime <= 0 || req.IdleTimeout <= 0 {
		t.Errorf("zero timeouts should default, got max=%d idle=%d", req.MaxLifetime, req.IdleTimeout)
	}
}

// An idle timeout above the hard lifetime could never fire, which would read to a
// caller as an idle timeout that silently does not work.
func TestValidateLeaseRequest_IdleNeverExceedsLifetime(t *testing.T) {
	req := createLeaseRequest{Image: "alpine:3.19", MaxLifetime: 60, IdleTimeout: 600}
	if err := validateLeaseRequest(&req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.IdleTimeout > req.MaxLifetime {
		t.Errorf("IdleTimeout %d exceeds MaxLifetime %d", req.IdleTimeout, req.MaxLifetime)
	}
}

func TestValidateLeaseRequest_RequiresImage(t *testing.T) {
	req := createLeaseRequest{}
	if err := validateLeaseRequest(&req); err == nil {
		t.Fatal("expected an error for a lease with no image")
	}
}

// A lease pins one working directory, so an explicit checkout path could not be
// honoured. Refusing it beats cloning somewhere the caller did not ask for.
func TestValidateLeaseRequest_RejectsCheckoutPath(t *testing.T) {
	req := createLeaseRequest{
		Image:      "alpine:3.19",
		SecretRefs: map[string]string{"GIT_CLONE_URL": "git:https://example.test/acme/widgets.git"},
		Checkout:   &CheckoutSpec{Path: "sub/dir"},
	}
	err := validateLeaseRequest(&req)
	if err == nil || !strings.Contains(err.Error(), "checkout.path") {
		t.Fatalf("expected a checkout.path rejection, got %v", err)
	}
}

func TestValidateLeaseRequest_RejectsTwoWorkdirMounts(t *testing.T) {
	req := createLeaseRequest{
		Image: "alpine:3.19",
		Volumes: []VolumeMount{
			{WorkflowID: "wf", Name: "a", MountPath: "/a", Workdir: true},
			{WorkflowID: "wf", Name: "b", MountPath: "/b", Workdir: true},
		},
	}
	if err := validateLeaseRequest(&req); err == nil {
		t.Fatal("expected two workdir mounts to be rejected")
	}
}

// ── Start command ────────────────────────────────────────────────────────────

func TestLeaseStartCommand_ClonesThenIdles(t *testing.T) {
	lease := Lease{
		LeaseID:         "l1",
		MaxLifetimeSecs: 1800,
		SecretRefs:      map[string]string{"GIT_CLONE_URL": "git:https://example.test/acme/widgets.git"},
		Checkout:        &CheckoutSpec{},
	}
	cmd := lease.startCommand()
	if len(cmd) != 3 || cmd[1] != "-c" {
		t.Fatalf("start command should be a shell form, got %v", cmd)
	}
	script := cmd[2]
	if !strings.Contains(script, "git clone") {
		t.Error("a lease with a checkout should clone at boot")
	}
	if !strings.Contains(script, leaseReadyMarker) {
		t.Error("the start script must print the ready marker so a failed clone is not mistaken for a ready lease")
	}
	// The clone has to complete before the marker, or the marker stops meaning
	// "ready" and an exec can race the checkout.
	if strings.Index(script, "git clone") > strings.Index(script, leaseReadyMarker) {
		t.Error("the ready marker must come after the clone")
	}
	if !strings.Contains(script, "exec sleep 1800") {
		t.Errorf("the idle wait should be bounded by the lease lifetime, got %q", script)
	}
}

// Without a checkout the sandbox still has to announce readiness, or nothing can
// tell a booted lease from one whose shell never started.
func TestLeaseStartCommand_MarkerWithoutCheckout(t *testing.T) {
	lease := Lease{LeaseID: "l1", MaxLifetimeSecs: 60}
	script := lease.startCommand()[2]
	if !strings.Contains(script, leaseReadyMarker) {
		t.Error("expected the ready marker even with no checkout")
	}
	if strings.Contains(script, "git clone") {
		t.Error("a lease with no checkout should not clone")
	}
}

// ── Working directory ────────────────────────────────────────────────────────

func TestLeaseWorkDir(t *testing.T) {
	if got := (Lease{}).workDir(); got != leaseWorkDir {
		t.Errorf("default workDir = %q, want %q", got, leaseWorkDir)
	}
	withVol := Lease{Volumes: []VolumeMount{
		{WorkflowID: "wf", Name: "cache", MountPath: "/cache"},
		{WorkflowID: "wf", Name: "src", MountPath: "/src", Workdir: true},
	}}
	if got := withVol.workDir(); got != "/src" {
		t.Errorf("workDir = %q, want the workdir mount /src", got)
	}
}

// ── Deadlines ────────────────────────────────────────────────────────────────

// The idle clock must run from the last USE, not from creation — otherwise a lease
// in continuous use is reaped mid-command once it passes its idle timeout.
func TestLeaseIdleDeadline_MeasuredFromLastUse(t *testing.T) {
	created := time.Now().UTC().Add(-30 * time.Minute)
	used := time.Now().UTC().Add(-1 * time.Minute)
	lease := Lease{CreatedAt: created, StartedAt: &created, LastUsedAt: &used, IdleTimeoutSecs: 300}
	if got := lease.idleDeadline(); !got.After(time.Now().UTC()) {
		t.Errorf("a lease used a minute ago should not be idle-expired; deadline %v", got)
	}
}

// A lease that was created and never used still has to be collectable, or an
// abandoned create leaks a sandbox until its hard lifetime.
func TestLeaseIdleDeadline_UnusedLeaseStillExpires(t *testing.T) {
	created := time.Now().UTC().Add(-30 * time.Minute)
	lease := Lease{CreatedAt: created, StartedAt: &created, IdleTimeoutSecs: 300}
	if got := lease.idleDeadline(); got.After(time.Now().UTC()) {
		t.Errorf("an unused lease should be idle-expired by now; deadline %v", got)
	}
}

func TestLeaseExpiryReason(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-10 * time.Second)

	cases := []struct {
		name  string
		lease Lease
		busy  bool
		want  string
	}{
		{
			name:  "live lease is kept",
			lease: Lease{Status: leaseReady, CreatedAt: recent, StartedAt: &recent, LastUsedAt: &recent, IdleTimeoutSecs: 300, MaxLifetimeSecs: 3600},
			want:  "",
		},
		{
			name:  "idle past its timeout",
			lease: Lease{Status: leaseReady, CreatedAt: now.Add(-20 * time.Minute), StartedAt: ptr(now.Add(-20 * time.Minute)), LastUsedAt: ptr(now.Add(-10 * time.Minute)), IdleTimeoutSecs: 300, MaxLifetimeSecs: 3600},
			want:  "idle",
		},
		{
			name:  "past its maximum lifetime",
			lease: Lease{Status: leaseReady, CreatedAt: now.Add(-2 * time.Hour), StartedAt: &recent, LastUsedAt: &recent, IdleTimeoutSecs: 300, MaxLifetimeSecs: 3600},
			want:  "exceeded its maximum lifetime",
		},
		{
			// IDLENESS IS MEASURED FROM DISPATCH, NOT COMPLETION: LastUsedAt is bumped
			// when a command is submitted and never again, so a long-running command
			// leaves its lease looking untouched for as long as it takes. Measured
			// against a coding agent held in one lease — last used at +4 seconds,
			// reaped "idle" at +5:17, mid-edit — after which the caller's next
			// command got a 409 and a stage that had done its work reported failure.
			name:  "working, however long since the command was dispatched",
			lease: Lease{Status: leaseReady, CreatedAt: now.Add(-20 * time.Minute), StartedAt: ptr(now.Add(-20 * time.Minute)), LastUsedAt: ptr(now.Add(-10 * time.Minute)), IdleTimeoutSecs: 300, MaxLifetimeSecs: 3600},
			busy:  true,
			want:  "",
		},
		{
			// ONLY IDLENESS IS FORGIVEN. A command that never finishes must not hold a
			// sandbox forever — that is precisely what the maximum lifetime is for.
			name:  "working, but past its maximum lifetime",
			lease: Lease{Status: leaseReady, CreatedAt: now.Add(-2 * time.Hour), StartedAt: &recent, LastUsedAt: &recent, IdleTimeoutSecs: 300, MaxLifetimeSecs: 3600},
			busy:  true,
			want:  "exceeded its maximum lifetime",
		},
		{
			name:  "stuck starting after a restart",
			lease: Lease{Status: leaseStarting, CreatedAt: now.Add(-time.Duration(leaseStartTimeoutSecs+60) * time.Second), IdleTimeoutSecs: 99999, MaxLifetimeSecs: 99999},
			want:  "never finished starting",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaseExpiryReason(tc.lease, now, tc.busy); got != tc.want {
				t.Errorf("leaseExpiryReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// ── Pod construction ─────────────────────────────────────────────────────────

func leaseFixture() Lease {
	return Lease{LeaseID: "l1", Image: "alpine:3.19", RunnerClass: "standard", MaxLifetimeSecs: 1800}
}

// The whole safety argument for leases is that a held sandbox is no laxer than a
// one-shot one. If this drifts, a lease becomes a way to get a weaker sandbox.
func TestBuildLeasePod_MatchesJobSecurityPosture(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	pod := r.buildLeasePod(leaseFixture(), stdRunnerSpec()).Spec

	if pod.SecurityContext == nil {
		t.Fatal("pod security context is nil")
	}
	if got := pod.SecurityContext.RunAsUser; got == nil || *got != sandboxUID {
		t.Errorf("pod RunAsUser = %v, want %d", got, sandboxUID)
	}
	if got := pod.SecurityContext.RunAsNonRoot; got == nil || !*got {
		t.Error("pod RunAsNonRoot should be true")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("a lease pod must not automount the service account token")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %v, want Never", pod.RestartPolicy)
	}
	c := pod.Containers[0]
	if c.SecurityContext == nil {
		t.Fatal("container security context is nil")
	}
	if got := c.SecurityContext.ReadOnlyRootFilesystem; got == nil || !*got {
		t.Error("container rootfs should be read-only")
	}
	if got := c.SecurityContext.AllowPrivilegeEscalation; got == nil || *got {
		t.Error("privilege escalation should be denied")
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) == 0 {
		t.Error("capabilities should be dropped")
	}
}

// A read-only rootfs leaves a checkout nowhere to land unless the lease mounts its
// own writable workspace.
func TestBuildLeasePod_MountsWritableWorkspace(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	pod := r.buildLeasePod(leaseFixture(), stdRunnerSpec()).Spec

	c := pod.Containers[0]
	if c.WorkingDir != leaseWorkDir {
		t.Errorf("WorkingDir = %q, want %q", c.WorkingDir, leaseWorkDir)
	}
	found := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == leaseWorkDir {
			found = true
			if m.ReadOnly {
				t.Error("the workspace mount must be writable")
			}
		}
	}
	if !found {
		t.Fatalf("no volume mounted at %s", leaseWorkDir)
	}
	// Memory-backed would charge the source tree and build output against the
	// runner class's memory limit, so a build would OOM with headroom to spare.
	for _, v := range pod.Volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Fatal("workspace should be an emptyDir")
			}
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				t.Error("the workspace emptyDir must not be memory-backed")
			}
		}
	}
}

// A shared volume that asks to be the working directory replaces the default, so a
// lease can keep its tree on storage that outlives the sandbox.
func TestBuildLeasePod_WorkdirVolumeReplacesDefault(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	lease := leaseFixture()
	lease.Volumes = []VolumeMount{{WorkflowID: "wf", Name: "src", MountPath: "/src", Workdir: true}}
	pod := r.buildLeasePod(lease, stdRunnerSpec()).Spec

	if got := pod.Containers[0].WorkingDir; got != "/src" {
		t.Errorf("WorkingDir = %q, want /src", got)
	}
	for _, v := range pod.Volumes {
		if v.Name == "workspace" {
			t.Error("the default workspace emptyDir should not be added when a volume claims the working directory")
		}
	}
}

func TestBuildLeasePod_LabelledForOrphanSweep(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	pod := r.buildLeasePod(leaseFixture(), stdRunnerSpec())
	if got := pod.Labels[leasePodLabel]; got != "l1" {
		t.Errorf("%s label = %q, want l1 — the orphan sweep selects on it", leasePodLabel, got)
	}
	if pod.Name != (Lease{LeaseID: "l1"}).resourceName() {
		t.Errorf("pod name %q does not match the derived resource name", pod.Name)
	}
}

// Kubernetes bounding the lease independently is what collects sandboxes when
// forge itself dies holding them.
func TestBuildLeasePod_HasActiveDeadline(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	pod := r.buildLeasePod(leaseFixture(), stdRunnerSpec()).Spec
	if pod.ActiveDeadlineSeconds == nil {
		t.Fatal("a lease pod needs its own deadline as a backstop to the reaper")
	}
	if *pod.ActiveDeadlineSeconds <= 1800 {
		t.Errorf("ActiveDeadlineSeconds = %d, want the lifetime plus a startup grace", *pod.ActiveDeadlineSeconds)
	}
}

func TestBuildLeasePod_AppliesRunnerClassLimits(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	spec := RunnerClass{Name: "big", MemoryMB: 2048, CPUMillicores: 2000, PidsLimit: 256, TmpfsMB: 512, Enabled: true}
	c := r.buildLeasePod(leaseFixture(), spec).Spec.Containers[0]

	if got := c.Resources.Limits.Memory().String(); got != "2Gi" {
		t.Errorf("memory limit = %s, want 2Gi", got)
	}
	if got := c.Resources.Limits.Cpu().MilliValue(); got != 2000 {
		t.Errorf("cpu limit = %dm, want 2000m", got)
	}
}

// ── Exit-status unwrapping ───────────────────────────────────────────────────

type fakeExitErr struct{ code int }

func (e fakeExitErr) Error() string   { return "command failed" }
func (e fakeExitErr) ExitStatus() int { return e.code }

type wrappedErr struct{ inner error }

func (w wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrappedErr) Unwrap() error { return w.inner }

// A non-zero exit arrives from remotecommand as an error. Reading the status out of
// it is what keeps "the command failed" distinct from "forge could not run the
// command" — classifyResult treats those two very differently.
func TestAsExitStatus(t *testing.T) {
	var out interface{ ExitStatus() int }

	if !asExitStatus(fakeExitErr{code: 3}, &out) || out.ExitStatus() != 3 {
		t.Error("expected a direct exit status to be read")
	}
	if !asExitStatus(wrappedErr{inner: fakeExitErr{code: 7}}, &out) || out.ExitStatus() != 7 {
		t.Error("expected a wrapped exit status to be read")
	}
	if asExitStatus(wrappedErr{inner: errPlain{}}, &out) {
		t.Error("a transport error must NOT be reported as an exit status, or a platform failure would be recorded as a failed command")
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "connection reset" }

func TestFirstNonEmptyLine(t *testing.T) {
	if got := firstNonEmptyLine("", "  \n", "real reason\nmore"); got != "real reason" {
		t.Errorf("firstNonEmptyLine = %q, want %q", got, "real reason")
	}
	if got := firstNonEmptyLine("", ""); got == "" {
		t.Error("expected a fallback rather than an empty diagnostic")
	}
}

// Git refuses a repository whose top-level directory belongs to another user,
// and a lease's working directory is exactly that: a volume the kubelet creates
// as root, used by commands running as the sandbox UID. Found on a live ticket —
// the clone succeeded and the agent's first `git checkout -b` failed with
// "detected dubious ownership", which reads as an agent bug rather than a
// sandbox one.
func TestBuildLeasePod_DeclaresTheWorkspaceSafeForGit(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	lease := leaseFixture()
	lease.Checkout = &CheckoutSpec{}
	env := map[string]string{}
	for _, e := range r.buildLeasePod(lease, stdRunnerSpec()).Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "safe.directory" {
		t.Fatalf("no safe.directory declaration; every git command in the lease would fail: %v", env)
	}
	if got := env["GIT_CONFIG_VALUE_0"]; got != lease.workDir() {
		t.Errorf("safe.directory = %q, want the lease's working directory %q", got, lease.workDir())
	}
}

// A volume that claims the working directory moves it, and the declaration has
// to follow — otherwise it names a path the repository is not in.
func TestBuildLeasePod_SafeDirectoryFollowsAWorkdirVolume(t *testing.T) {
	r := &KubernetesRuntime{namespace: "forge"}
	lease := leaseFixture()
	lease.Volumes = []VolumeMount{{WorkflowID: "wf", Name: "src", MountPath: "/src", Workdir: true}}
	for _, e := range r.buildLeasePod(lease, stdRunnerSpec()).Spec.Containers[0].Env {
		if e.Name == "GIT_CONFIG_VALUE_0" && e.Value != "/src" {
			t.Errorf("safe.directory = %q, want /src", e.Value)
		}
	}
}

// A lease that has just FINISHED a long command must not be instantly reapable.
//
// last_used_at was stamped only at dispatch, so an eight-minute command left the
// lease looking eight minutes idle the moment it returned. Measured: an agent
// command ran 8m16s, succeeded, and the reaper saw idle_secs=496 against a 300s
// limit in the same instant — so the very next submission, the verification of the
// work that had just succeeded, was refused because the sandbox was gone. The
// stage failed on work it had actually done.
//
// Idle has to mean "nothing has been running or finishing here recently".
func TestLeaseIsNotIdleImmediatelyAfterALongCommand(t *testing.T) {
	requireForgeDB(t)

	now := time.Now().UTC()
	dispatched := now.Add(-8 * time.Minute) // when the command started
	leaseID := uuid.New().String()

	lease := Lease{
		LeaseID: leaseID, UserID: "user-x", Image: "alpine:3.19", RunnerClass: "standard",
		Status: leaseReady, CreatedAt: dispatched, StartedAt: &dispatched, LastUsedAt: &dispatched,
		IdleTimeoutSecs: 300, MaxLifetimeSecs: 3600,
	}
	if err := connect().Create(&lease).Error; err != nil {
		t.Fatalf("create lease: %v", err)
	}
	t.Cleanup(func() {
		connect().Exec(`DELETE FROM leases WHERE lease_id = ?`, leaseID) //nolint:errcheck
	})

	// The bug, stated as a precondition: judged on dispatch time alone it is idle.
	if got := leaseExpiryReason(lease, now, false); got != "idle" {
		t.Fatalf("precondition: leaseExpiryReason() = %q, want %q — the scenario no longer reproduces", got, "idle")
	}

	// What the worker now does when the command finishes.
	if err := touchLease(context.Background(), leaseID); err != nil {
		t.Fatalf("touchLease: %v", err)
	}

	var after Lease
	if err := connect().Where("lease_id = ?", leaseID).First(&after).Error; err != nil {
		t.Fatalf("re-read lease: %v", err)
	}
	if got := leaseExpiryReason(after, time.Now().UTC(), false); got != "" {
		t.Errorf("leaseExpiryReason() = %q, want it kept: the next command has nowhere to run", got)
	}
}
