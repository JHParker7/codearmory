package main

import (
	"context"
	"regexp"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// withServiceDef installs a def into the embedded catalog for one test, so the
// persistence paths can be exercised without pinning the tests to whichever shipped
// service happens to declare storage today.
func withServiceDef(t *testing.T, d serviceDef) {
	t.Helper()
	defs, err := loadEmbeddedDefs()
	if err != nil {
		t.Fatalf("load embedded defs: %v", err)
	}
	prev, had := defs[d.RegistryName]
	defs[d.RegistryName] = d
	t.Cleanup(func() {
		if had {
			defs[d.RegistryName] = prev
			return
		}
		delete(defs, d.RegistryName)
	})
}

func persistentDef(mode string) serviceDef {
	return serviceDef{
		RegistryName: "stateful_svc",
		K8sName:      "stateful-svc",
		ImageRepo:    "stateful",
		Port:         8099,
		Infra: svcInfra{Persistence: &svcPersistence{
			MountPath:    "/var/lib/stateful",
			Size:         "20Gi",
			StorageClass: "fast",
			AccessMode:   mode,
		}},
	}
}

func mountPath(c corev1.Container, name string) string {
	for _, m := range c.VolumeMounts {
		if m.Name == name {
			return m.MountPath
		}
	}
	return ""
}

func claimName(ps corev1.PodSpec, name string) string {
	for _, v := range ps.Volumes {
		if v.Name == name && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

func TestEnsurePVC_CreatedOnceAndNeverUpdated(t *testing.T) {
	httpClient = initHTTPClient()
	withServiceDef(t, persistentDef("ReadWriteOnce"))
	b := newTestBackend(t, &registerRecorder{})
	ctx := context.Background()

	if err := b.ensureInfra(ctx, workloadSpec{Service: "stateful_svc"}); err != nil {
		t.Fatalf("ensureInfra: %v", err)
	}
	api := b.client.CoreV1().PersistentVolumeClaims("codearmory")
	pvc, err := api.Get(ctx, "codearmory-stateful-svc-data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if pvc.Labels[labelInfraOf] != "stateful_svc" {
		t.Errorf("pvc infra-of = %q, want stateful_svc", pvc.Labels[labelInfraOf])
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes = %v, want [ReadWriteOnce]", got)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "20Gi" {
		t.Errorf("size = %s, want 20Gi", got.String())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("storage class = %v, want fast", pvc.Spec.StorageClassName)
	}

	// A def that later asks for a bigger volume must NOT rewrite the existing claim —
	// PVC specs are near-immutable and resizing is a deliberate admin action.
	grown := persistentDef("ReadWriteOnce")
	grown.Infra.Persistence.Size = "100Gi"
	grown.Infra.Persistence.StorageClass = "slow"
	withServiceDef(t, grown)
	if err := b.ensureInfra(ctx, workloadSpec{Service: "stateful_svc"}); err != nil {
		t.Fatalf("ensureInfra (second pass): %v", err)
	}
	pvc, _ = api.Get(ctx, "codearmory-stateful-svc-data", metav1.GetOptions{})
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "20Gi" {
		t.Errorf("size after reconcile = %s, want the original 20Gi", got.String())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("storage class after reconcile = %v, want the original fast", pvc.Spec.StorageClassName)
	}
}

func TestPersistentService_TeardownKeepsTheVolume(t *testing.T) {
	httpClient = initHTTPClient()
	withServiceDef(t, persistentDef("ReadWriteOnce"))
	b := newTestBackend(t, &registerRecorder{})
	b.prov.enabled = false
	ctx := context.Background()

	if err := b.ensureInfra(ctx, workloadSpec{Service: "stateful_svc"}); err != nil {
		t.Fatalf("ensureInfra: %v", err)
	}
	// The claim is infra, so the desired-state diff must not see it as a stray service.
	managed, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	for _, s := range managed {
		if s == "stateful-svc-data" {
			t.Error("ListManaged returned the PVC component")
		}
	}
	// Disabling the service tears the workload down but never the data.
	if err := b.RemoveService(ctx, "stateful_svc"); err != nil {
		t.Fatalf("RemoveService: %v", err)
	}
	if _, err := b.client.CoreV1().PersistentVolumeClaims("codearmory").Get(ctx, "codearmory-stateful-svc-data", metav1.GetOptions{}); err != nil {
		t.Errorf("PVC removed on teardown (%v) — disabling a service must not destroy its data", err)
	}
}

func TestBuildDeployment_MountsVolumesOnTemplateAndClonePaths(t *testing.T) {
	httpClient = initHTTPClient()
	withServiceDef(t, persistentDef("ReadWriteOnce"))
	ctx := context.Background()

	check := func(t *testing.T, dep *appsv1.Deployment) {
		t.Helper()
		c := dep.Spec.Template.Spec.Containers[0]
		if got := mountPath(c, persistVolumeName); got != "/var/lib/stateful" {
			t.Errorf("data mount = %q, want /var/lib/stateful", got)
		}
		// git and friends write lock/temp files; the read-only rootfs denies that without
		// a scratch mount.
		if got := mountPath(c, tmpVolumeName); got != "/tmp" {
			t.Errorf("tmp mount = %q, want /tmp", got)
		}
		if got := claimName(dep.Spec.Template.Spec, persistVolumeName); got != "codearmory-stateful-svc-data" {
			t.Errorf("claim = %q", got)
		}
		if sc := c.SecurityContext; sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Error("read-only rootfs should be kept — mounted volumes stay writable")
		}
	}

	t.Run("template path", func(t *testing.T) {
		b := newTestBackend(t, &registerRecorder{})
		dep, err := b.buildDeployment(ctx, workloadSpec{Service: "stateful_svc"})
		if err != nil {
			t.Fatalf("buildDeployment: %v", err)
		}
		check(t, dep)
	})

	// The clone path is the one that would silently miss the volumes if injection lived
	// inside templatePod.
	t.Run("clone path", func(t *testing.T) {
		ro := true
		base := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "codearmory-stateful-svc",
				Namespace: "codearmory",
				Labels:    map[string]string{labelComponent: "stateful_svc", labelManagedBy: "Helm"},
			},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:            "stateful",
					Image:           "ghcr.io/x/stateful:v1",
					SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: &ro},
				}},
			}}},
		}
		b := newTestBackend(t, &registerRecorder{}, base)
		dep, err := b.buildDeployment(ctx, workloadSpec{Service: "stateful_svc"})
		if err != nil {
			t.Fatalf("buildDeployment: %v", err)
		}
		check(t, dep)
		// Idempotent: a second render neither duplicates the volumes nor churns the spec.
		again, _ := b.buildDeployment(ctx, workloadSpec{Service: "stateful_svc"})
		if got := len(again.Spec.Template.Spec.Volumes); got != 2 {
			t.Errorf("volumes on re-render = %d, want 2", got)
		}
		if specHash(dep.Spec) != specHash(again.Spec) {
			t.Error("re-render is not byte-identical (would churn the workload)")
		}
	})
}

func TestPersistence_SingleWriterRolloutIsRecreateAndPinnedToOnePod(t *testing.T) {
	httpClient = initHTTPClient()
	withServiceDef(t, persistentDef("ReadWriteOnce"))
	b := newTestBackend(t, &registerRecorder{})
	b.defaultReplicas = 3
	b.rotateInterval = time.Hour
	ctx := context.Background()

	spec := workloadSpec{Service: "stateful_svc", Replicas: 3, RotateInterval: time.Hour}
	dep, err := b.buildDeployment(ctx, spec)
	if err != nil {
		t.Fatalf("buildDeployment: %v", err)
	}
	// Surging a replacement would deadlock: the new pod cannot attach a volume the old
	// pod still holds.
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("strategy = %s, want Recreate", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1 (extra pods would never schedule)", dep.Spec.Replicas)
	}
	if b.replicaFloor(spec) {
		t.Error("single-writer replica count must be exact, not a floor")
	}
	// The periodic rolling restart hits the same deadlock, so it is off.
	if got := b.rotateFor(spec); got != 0 {
		t.Errorf("rotate interval = %s, want 0 for a single-writer volume", got)
	}
	if _, stamped := dep.Spec.Template.Annotations[annotationRotatedAt]; stamped {
		t.Error("rotation annotation stamped on a single-writer workload")
	}
	// One replica means no PDB (a PDB over a single pod wedges every drain).
	if err := b.applyPDB(ctx, "stateful_svc", b.replicasFor(spec)); err != nil {
		t.Fatalf("applyPDB: %v", err)
	}
	if _, err := b.client.PolicyV1().PodDisruptionBudgets("codearmory").Get(ctx, "codearmory-stateful-svc", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("PDB present for a single-replica workload (err=%v)", err)
	}
}

func TestPersistence_ReadWriteManyKeepsTheScalableShape(t *testing.T) {
	httpClient = initHTTPClient()
	withServiceDef(t, persistentDef("ReadWriteMany"))
	b := newTestBackend(t, &registerRecorder{})
	ctx := context.Background()

	spec := workloadSpec{Service: "stateful_svc", Replicas: 3, RotateInterval: time.Hour}
	dep, err := b.buildDeployment(ctx, spec)
	if err != nil {
		t.Fatalf("buildDeployment: %v", err)
	}
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("strategy = %s, want RollingUpdate for RWX", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want the requested 3", dep.Spec.Replicas)
	}
	if got := b.rotateFor(spec); got != time.Hour {
		t.Errorf("rotate interval = %s, want 1h", got)
	}
	if err := b.ensureInfra(ctx, workloadSpec{Service: "stateful_svc"}); err != nil {
		t.Fatalf("ensureInfra: %v", err)
	}
	pvc, err := b.client.CoreV1().PersistentVolumeClaims("codearmory").Get(ctx, "codearmory-stateful-svc-data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteMany {
		t.Errorf("access modes = %v, want [ReadWriteMany]", got)
	}
}

// git_factory is the first shipped def to declare persistence. The invariant worth
// pinning is that its storage-root env and its mount path cannot drift apart — the
// service would happily write repositories to the container's ephemeral filesystem.
func TestGitFactoryDef_StorageRootMatchesTheMount(t *testing.T) {
	def, ok := embeddedServiceDef("codearmory_git_factory")
	if !ok {
		t.Fatal("no embedded def for codearmory_git_factory")
	}
	p := def.Infra.Persistence
	if p == nil {
		t.Fatal("git_factory declares no persistence — repos would live on an ephemeral disk")
	}
	if got := def.EnvExtras["GIT_STORAGE_ROOT"]; got != p.MountPath {
		t.Errorf("GIT_STORAGE_ROOT = %q but the volume is mounted at %q", got, p.MountPath)
	}
	// v1 is single-node by design (ARCHITECTURE §5 step 1), so the volume is RWO and
	// the workload must be pinned to one pod.
	if !p.singleWriter() {
		t.Error("git_factory is single-node in v1; RWX needs the step-2 routing table first")
	}
	b := &k8sBackend{prefix: "codearmory", namespace: "codearmory", defaultReplicas: 3}
	if got := b.replicasFor(workloadSpec{Service: "codearmory_git_factory", Replicas: 3}); got != 1 {
		t.Errorf("replicas = %d, want 1", got)
	}
}

// Every name k8s validates as a DNS-1123 label must come from k8sName, never the
// registry name: the apiserver rejects the whole Deployment over a container named
// "codearmory_git_factory".
func TestTemplatePod_ContainerNameIsDNS1123(t *testing.T) {
	b := &k8sBackend{prefix: "codearmory", namespace: "codearmory", registry: "ghcr.io/x", tag: "v1"}
	dns1123 := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	defs, err := loadEmbeddedDefs()
	if err != nil {
		t.Fatalf("load defs: %v", err)
	}
	for name := range defs {
		got := b.templatePod(workloadSpec{Service: name}).Spec.Containers[0].Name
		if !dns1123.MatchString(got) {
			t.Errorf("%s: container name %q is not a DNS-1123 label", name, got)
		}
	}
}

// A service that declares no persistence keeps exactly its previous shape: no volumes,
// no strategy change, no replica clamp.
func TestNoPersistence_ShapeUnchanged(t *testing.T) {
	httpClient = initHTTPClient()
	b := newTestBackend(t, &registerRecorder{})
	ctx := context.Background()

	spec := workloadSpec{Service: "blueprints", Replicas: 2}
	dep, err := b.buildDeployment(ctx, spec)
	if err != nil {
		t.Fatalf("buildDeployment: %v", err)
	}
	if len(dep.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("volumes = %v, want none", dep.Spec.Template.Spec.Volumes)
	}
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("strategy = %s, want RollingUpdate", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2", dep.Spec.Replicas)
	}
	if err := b.ensureInfra(ctx, spec); err != nil {
		t.Fatalf("ensureInfra: %v", err)
	}
	list, err := b.client.CoreV1().PersistentVolumeClaims("codearmory").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pvcs: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("created %d PVCs for a stateless service", len(list.Items))
	}
}

// A "shared" key is only shared if both holders have the same bytes. Builder derives
// them, but a peer deployed by the Helm chart carries a chart-generated key instead —
// so the owning service's existing value must win. Getting this wrong is silent: the
// emitter signs, the receiver 401s, and nothing reports a misconfiguration.
func TestSharedKeyOwner(t *testing.T) {
	cases := map[string]string{
		"hooks-trigger-key":      "hooks",     // builder-catalog service
		"conductor-forward-key":  "conductor", // core service
		"encryption-key":         "",          // not a service — derive
		"gatekeeper-service-key": "gatekeeper",
		"registry-service-key":   "registry",
		"nodashes":               "",
		"":                       "",
	}
	for key, want := range cases {
		if got := sharedKeyOwner(key); got != want {
			t.Errorf("sharedKeyOwner(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestProvision_SharedKeyAdoptsTheOwnersValue(t *testing.T) {
	httpClient = initHTTPClient()
	enableDerivation(t)
	ctx := context.Background()

	// hooks already exists with a key builder did not derive (as the Helm chart leaves it).
	const chartKey = "chart-generated-not-derived"
	hooksSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "codearmory-hooks", Namespace: "codearmory"},
		Data:       map[string][]byte{"hooks-trigger-key": []byte(chartKey)},
	}
	b := newTestBackend(t, &registerRecorder{}, hooksSecret)

	// A service whose def declares the same shared key must adopt hooks' value.
	withServiceDef(t, serviceDef{
		RegistryName: "emitter", K8sName: "emitter", ImageRepo: "emitter", Port: 9000,
		DerivedSecrets: []derivedSecret{{EnvVar: "HOOKS_TRIGGER_KEY", Kind: "shared", Name: "hooks-trigger-key"}},
	})
	if err := b.provision(ctx, "emitter", "", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	sec, err := b.client.CoreV1().Secrets("codearmory").Get(ctx, "codearmory-emitter", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got := string(sec.Data["hooks-trigger-key"]); got != chartKey {
		t.Errorf("hooks-trigger-key = %q, want the owner's value %q — a derived value would 401 at hooks", got, chartKey)
	}
}
