package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Phase 2 — the reconciler turns desired state (a default-scope service marked
// enabled) into a running workload. An instance admin enables a non-core service
// and builder creates its Deployment + Service in the instance namespace, slotting
// into the names/ports the registry manifest already routes to; disabling tears it
// back down. This is single-namespace (one instance), not per-org.

// Labels: builder stamps its workloads so it can list and reclaim exactly what it
// owns, and gives them the standard component/instance/name labels so the existing
// Service selectors and conductor routing find them.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelComponent = "app.kubernetes.io/component"
	labelInstance  = "app.kubernetes.io/instance"
	labelName      = "app.kubernetes.io/name"
	managedByValue = "codearmory-builder"

	// annotationRotatedAt carries the current rotation bucket (now truncated to the
	// rotate interval) on the pod template. It changes only on an interval boundary,
	// so periodic reconciles within the same window leave the template byte-identical
	// (no churn); on the boundary the new value rolls the pods — restoring them to the
	// image to shed drift or a compromised process. See stampRotation.
	annotationRotatedAt = "codearmory.io/rotated-at"

	// annotationSpecHash carries a hash of the desired spec builder last applied to a
	// workload. A steady-state reconcile whose rendered spec matches the stored hash
	// skips the Update entirely, so identical passes don't churn resourceVersions or
	// generate needless apiserver/etcd writes. See applyDeployment.
	annotationSpecHash = "codearmory.io/spec-hash"

	// minReadySeconds requires a new pod to stay Ready before a rollout proceeds to the
	// next, so a crash-looping image can't churn the whole set unnoticed.
	minReadySeconds = 10
)

// specHash is a stable content hash of a rendered spec, used to skip no-op Updates.
func specHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// workloadSpec is the desired workload for one service. Image/Port are optional:
// when empty the backend clones the platform service's existing base Deployment
// (the Helm-rendered spec) and falls back to the image registry/tag + a known port
// only when there is nothing to clone.
type workloadSpec struct {
	Service        string
	Image          string            // explicit image (custom services); "" => clone or template
	Port           int32             // explicit port; 0 => clone or known-port catalog
	Env            map[string]string // config overrides applied on top of the base env
	DBUrl          string            // decrypted admin-supplied database URL ("" if none)
	Secrets        map[string]string // decrypted admin-supplied sensitive config (env-key → value)
	Replicas       int32             // desired replicas; 0 => backend default (then 1)
	RotateInterval time.Duration     // periodic rolling-restart cadence; 0 => backend default (then off)
}

// clusterBackend is the reconciler's view of the cluster. The k8s implementation
// talks to the apiserver; noopBackend is used when reconciliation is disabled and
// in tests.
type clusterBackend interface {
	EnsureService(ctx context.Context, spec workloadSpec) error
	RemoveService(ctx context.Context, service string) error
	ListManaged(ctx context.Context) ([]string, error)
}

// noopBackend is the inert default: it records nothing in the cluster. Used when
// BUILDER_RECONCILE is off so the service runs unchanged in dev/compose.
type noopBackend struct{}

func (noopBackend) EnsureService(context.Context, workloadSpec) error { return nil }
func (noopBackend) RemoveService(context.Context, string) error       { return nil }
func (noopBackend) ListManaged(context.Context) ([]string, error)     { return nil, nil }

type k8sBackend struct {
	client    kubernetes.Interface
	namespace string // instance namespace workloads are created in
	prefix    string // release fullname prefix, e.g. "codearmory" → codearmory-forge
	registry  string // image registry for the template fallback
	tag       string // image tag for the template fallback
	prov      provisioningConfig

	// secrets is the backend for reading/writing per-service secret bundles. Defaults
	// to a k8s-Secret store; the seam lets a Vault-backed store swap in later. Nil in
	// hand-built test backends — store() falls back to a k8s store over client/namespace.
	secrets secretStore

	// defaultReplicas is the replica floor applied to every managed workload that does
	// not set its own (0 => 1). rotateInterval is the default periodic rolling-restart
	// cadence (0 => off). Both are overridable per service via reserved config knobs.
	defaultReplicas int32
	rotateInterval  time.Duration
	// imagePullSecrets are attached to every managed workload (private registries).
	imagePullSecrets []string
	// nowFn is the clock for rotation bucketing; overridden in tests.
	nowFn func() time.Time
}

// newK8sBackend builds a backend from in-cluster config, falling back to a
// kubeconfig (KUBECONFIG or ~/.kube/config) for out-of-cluster use (e.g. local
// verification against a remote cluster).
func newK8sBackend(namespace, prefix, registry, tag string) (*k8sBackend, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = clientcmd.RecommendedHomeFile
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kube config: %w", err)
		}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		namespace = "default"
	}
	if prefix == "" {
		prefix = "codearmory"
	}
	return &k8sBackend{
		client:    client,
		namespace: namespace,
		prefix:    prefix,
		registry:  registry,
		tag:       tag,
		nowFn:     time.Now,
		secrets:   &k8sSecretStore{client: client, namespace: namespace},
	}, nil
}

// name is the cluster object name (Deployment/Service/Secret/PDB) for a service.
// It uses the embedded def's K8sName so a registry name containing '_' (e.g.
// "gitea_integration") maps to a valid DNS-1123 object name ("gitea-integration").
// Labels/selectors keep the registry name (see labels/selector), so reconciliation
// and identity stay keyed on the registry name.
func (b *k8sBackend) name(service string) string {
	k8s := service
	if d, ok := embeddedServiceDef(service); ok {
		k8s = d.K8sName
	}
	return b.prefix + "-" + k8s
}

// store returns the configured secret backend, falling back to a k8s store over the
// backend's own client/namespace when unset (hand-built test backends).
func (b *k8sBackend) store() secretStore {
	if b.secrets != nil {
		return b.secrets
	}
	return &k8sSecretStore{client: b.client, namespace: b.namespace}
}

func (b *k8sBackend) labels(service string) map[string]string {
	return map[string]string{
		labelName:      "codearmory",
		labelInstance:  b.prefix,
		labelComponent: service,
		labelManagedBy: managedByValue,
	}
}

// selector matches the component/instance/name labels the platform Service uses,
// so a builder-deployed workload is reachable on the same routing.
func (b *k8sBackend) selector(service string) map[string]string {
	return map[string]string{
		labelName:      "codearmory",
		labelInstance:  b.prefix,
		labelComponent: service,
	}
}

func (b *k8sBackend) EnsureService(ctx context.Context, spec workloadSpec) error {
	// Provision the gatekeeper identity + Secret first so the workload it deploys
	// can authenticate and connect — letting a service come online with no Helm change.
	if b.provisioningOn() {
		if err := b.provision(ctx, spec.Service, spec.DBUrl, spec.Secrets); err != nil {
			return fmt.Errorf("provision %s: %w", spec.Service, err)
		}
	}
	// Stateless app infra (egress-proxy, forge RBAC) before the workload, so the pod has
	// what it needs once it starts. Level-triggered: a transient failure retries next pass.
	if err := b.ensureInfra(ctx, spec); err != nil {
		return fmt.Errorf("ensure infra %s: %w", spec.Service, err)
	}
	dep, err := b.buildDeployment(ctx, spec)
	if err != nil {
		return err
	}
	// A per-service replicas override is an exact target; the backend default is a
	// floor that an admin/HPA scale-up may exceed.
	if err := b.applyDeployment(ctx, dep, spec.Replicas <= 0); err != nil {
		return fmt.Errorf("apply deployment %s: %w", dep.Name, err)
	}
	port := spec.Port
	if port == 0 {
		port = b.containerPort(ctx, spec.Service)
	}
	if err := b.applyService(ctx, spec.Service, port); err != nil {
		return fmt.Errorf("apply service %s: %w", b.name(spec.Service), err)
	}
	if err := b.applyPDB(ctx, spec.Service, b.replicasFor(spec)); err != nil {
		return fmt.Errorf("apply poddisruptionbudget %s: %w", b.name(spec.Service), err)
	}
	// Register with the registry LAST, so conductor only learns the route once the
	// Service/Deployment exist. Custom services with no embedded def are registered
	// out-of-band, so they are skipped here.
	if registerServicesOn() {
		if def, ok := embeddedServiceDef(spec.Service); ok {
			if def.RegistryAccount {
				if err := ensureRegistryAccount(ctx, def.RegistryName, derivePrivateKey(def.RegistryName, "registry-service-key")); err != nil {
					return fmt.Errorf("ensure registry account %s: %w", def.RegistryName, err)
				}
			}
			if err := ensureRegistryService(ctx, def, b.prefix); err != nil {
				return fmt.Errorf("register %s: %w", def.RegistryName, err)
			}
		}
	}
	return nil
}

func (b *k8sBackend) RemoveService(ctx context.Context, service string) error {
	// Deregister from the registry FIRST so conductor stops routing before the workload
	// disappears (avoids routing to a deleting pod). Best-effort: a failure is logged and
	// corrected on the next reconcile rather than blocking teardown.
	if registerServicesOn() {
		if err := removeRegistryService(ctx, service); err != nil {
			slog.WarnContext(ctx, "deregister service from registry failed", "service", service, "error", err)
		}
	}
	name := b.name(service)
	policy := metav1.DeletePropagationForeground
	opts := metav1.DeleteOptions{PropagationPolicy: &policy}
	if err := b.client.AppsV1().Deployments(b.namespace).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete deployment %s: %w", name, err)
	}
	if err := b.client.CoreV1().Services(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	if err := b.client.PolicyV1().PodDisruptionBudgets(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete poddisruptionbudget %s: %w", name, err)
	}
	b.teardownInfra(ctx, service)
	if b.provisioningOn() {
		b.deprovision(ctx, service)
	}
	return nil
}

// ListManaged returns the service names of every builder-managed Deployment in the
// namespace, so the reconciler can reclaim ones no longer desired.
func (b *k8sBackend) ListManaged(ctx context.Context) ([]string, error) {
	// Exclude infra workloads (labelled infra-of): they belong to a parent service and
	// are torn down with it, not reconciled as top-level services in the desired diff.
	list, err := b.client.AppsV1().Deployments(b.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue + ",!" + labelInfraOf,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, d := range list.Items {
		if svc := d.Labels[labelComponent]; svc != "" {
			out = append(out, svc)
		}
	}
	return out, nil
}

// buildDeployment renders the desired Deployment: clone the platform base when one
// exists (preserving env/secrets/probes), else a minimal template from image+port.
func (b *k8sBackend) buildDeployment(ctx context.Context, spec workloadSpec) (*appsv1.Deployment, error) {
	var podTemplate corev1.PodTemplateSpec
	if base := b.findBaseDeployment(ctx, spec.Service); base != nil && spec.Image == "" {
		podTemplate = *base.Spec.Template.DeepCopy()
	} else {
		podTemplate = b.templatePod(spec)
	}
	applyEnvOverrides(&podTemplate, spec.Env)

	name := b.name(spec.Service)
	podTemplate.Labels = mergeLabels(podTemplate.Labels, b.labels(spec.Service))

	// Stamp the rotation bucket so a periodic rolling restart fires once per interval
	// (not on every reconcile) — restoring the pods to the image to shed drift.
	if rotate := b.rotateFor(spec); rotate > 0 {
		b.stampRotation(&podTemplate, rotate)
	}

	replicas := b.replicasFor(spec)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: b.labels(spec.Service)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: b.selector(spec.Service)},
			Template: podTemplate,
			// maxUnavailable:0/maxSurge:1 brings a replacement Ready before retiring an
			// old pod, so a deploy or periodic rotation never drops below the replica
			// floor — the "always N healthy" guarantee during voluntary churn.
			Strategy:        rollingStrategy(),
			MinReadySeconds: minReadySeconds,
		},
	}, nil
}

// rollingStrategy keeps the ready count at the replica floor throughout a rollout:
// surge one new pod, never take an old one down until its replacement is Ready.
func rollingStrategy() appsv1.DeploymentStrategy {
	maxUnavailable := intstr.FromInt(0)
	maxSurge := intstr.FromInt(1)
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}

// replicasFor resolves the replica count: per-service override, else the backend
// default, else 1 (the historical behaviour).
func (b *k8sBackend) replicasFor(spec workloadSpec) int32 {
	if spec.Replicas > 0 {
		return spec.Replicas
	}
	if b.defaultReplicas > 0 {
		return b.defaultReplicas
	}
	return 1
}

// rotateFor resolves the periodic-restart cadence: per-service override, else the
// backend default, else 0 (disabled).
func (b *k8sBackend) rotateFor(spec workloadSpec) time.Duration {
	if spec.RotateInterval > 0 {
		return spec.RotateInterval
	}
	return b.rotateInterval
}

// stampRotation writes the current interval bucket onto the pod template. The value
// is identical for every reconcile within a window (so the update is a no-op and the
// pods are left alone) and flips exactly once per interval (rolling the pods back to
// the image). Buckets are anchored to the UTC day so the boundaries are stable across
// timezones and land on intuitive wall-clock times. See rotationBucket.
func (b *k8sBackend) stampRotation(pt *corev1.PodTemplateSpec, interval time.Duration) {
	now := time.Now
	if b.nowFn != nil {
		now = b.nowFn
	}
	if pt.Annotations == nil {
		pt.Annotations = map[string]string{}
	}
	pt.Annotations[annotationRotatedAt] = rotationBucket(now().UTC(), interval).Format(time.RFC3339)
}

// rotationBucket returns the start of the current interval bucket anchored to the UTC
// day. Anchoring to midnight (rather than time.Truncate, which buckets relative to the
// Unix epoch) keeps the boundaries on intuitive wall-clock times for intervals that
// don't evenly divide a day — e.g. a 7h cadence rolls at 00:00, 07:00, 14:00, 21:00
// each day instead of drifting. For intervals that do divide a day (30m, 1h, …) it is
// identical to epoch-relative truncation.
func rotationBucket(now time.Time, interval time.Duration) time.Time {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	elapsed := now.Sub(dayStart)
	return dayStart.Add((elapsed / interval) * interval)
}

// findBaseDeployment returns a platform (non-builder) Deployment for the service to
// clone, or nil. It ignores builder-managed Deployments so a clone never feeds back
// on itself.
func (b *k8sBackend) findBaseDeployment(ctx context.Context, service string) *appsv1.Deployment {
	list, err := b.client.AppsV1().Deployments(b.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelComponent + "=" + service,
	})
	if err != nil {
		return nil
	}
	for i := range list.Items {
		if list.Items[i].Labels[labelManagedBy] != managedByValue {
			return &list.Items[i]
		}
	}
	return nil
}

func (b *k8sBackend) containerPort(ctx context.Context, service string) int32 {
	if base := b.findBaseDeployment(ctx, service); base != nil {
		for _, c := range base.Spec.Template.Spec.Containers {
			for _, p := range c.Ports {
				if p.ContainerPort != 0 {
					return p.ContainerPort
				}
			}
		}
	}
	if d, ok := embeddedServiceDef(service); ok && d.Port != 0 {
		return d.Port
	}
	return 8080
}

// templatePod fully expresses the pod for a service builder deploys. Once the chart is
// core-only there is no base Deployment to clone, so this carries everything: the image
// on its port, the full env (gatekeeper URL + the embedded def's static/inter-service
// env, derived-secret refs, registry-account ref, and admin-secret refs), the security
// context, probes, resources, image pull secrets, and (for forge) its ServiceAccount.
func (b *k8sBackend) templatePod(spec workloadSpec) corev1.PodTemplateSpec {
	def, hasDef := embeddedServiceDef(spec.Service)

	image := spec.Image
	if image == "" {
		repo := spec.Service
		if hasDef {
			repo = def.ImageRepo
		}
		image = fmt.Sprintf("%s/%s:%s", b.registry, repo, b.tag)
	}
	port := spec.Port
	if port == 0 {
		switch {
		case hasDef && def.Port != 0:
			port = def.Port
		default:
			port = 8080
		}
	}
	secretName := b.name(spec.Service)

	// Assemble env into a map and emit it sorted, so repeated reconciles produce a
	// byte-identical pod template and never churn the workload.
	envByName := map[string]corev1.EnvVar{}
	setVal := func(name, value string) { envByName[name] = corev1.EnvVar{Name: name, Value: value} }
	setRef := func(name, secretKey string) {
		envByName[name] = corev1.EnvVar{Name: name, ValueFrom: optionalSecretRef(secretName, secretKey)}
	}
	setVal("PORT", fmt.Sprintf("%d", port))
	setVal("GATEKEEPER_URL", fmt.Sprintf("http://%s-gatekeeper:8081", b.prefix))
	// Always-available identity/connection keys, wired as optional secret refs (absent
	// keys are simply unset).
	setRef("DATABASE_URL", "database-url")
	setRef("GATEKEEPER_SERVICE_KEY", "gatekeeper-service-key")
	setRef("CONDUCTOR_FORWARD_KEY", "conductor-forward-key")
	if hasDef {
		for k, v := range def.EnvExtras {
			v = strings.ReplaceAll(v, "${PREFIX}", b.prefix)
			v = strings.ReplaceAll(v, "${NAMESPACE}", b.namespace)
			setVal(k, v)
		}
		for _, ds := range def.DerivedSecrets {
			setRef(ds.EnvVar, ds.Name)
		}
		if def.RegistryAccount {
			setRef("REGISTRY_SERVICE_KEY", "registry-service-key")
		}
		for _, key := range def.SecretConfig {
			setRef(key, secretKeyForEnv(key))
		}
	}
	names := make([]string, 0, len(envByName))
	for n := range envByName {
		names = append(names, n)
	}
	sort.Strings(names)
	env := make([]corev1.EnvVar, 0, len(names))
	for _, n := range names {
		env = append(env, envByName[n])
	}

	runAsNonRoot := true
	allowPriv := false
	readOnly := true
	podSpec := corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot},
		Containers: []corev1.Container{{
			Name:  spec.Service,
			Image: image,
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
			Env:   env,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &allowPriv,
				ReadOnlyRootFilesystem:   &readOnly,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			ReadinessProbe: tcpProbe(5, 10),
			LivenessProbe:  tcpProbe(10, 15),
			Resources:      defaultResources(),
		}},
	}
	for _, n := range b.imagePullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: n})
	}
	// forge needs its ServiceAccount so it can create sandbox Jobs in the exec namespace.
	if hasDef && def.Infra.ForgeExecRBAC {
		automount := true
		podSpec.ServiceAccountName = b.name(spec.Service)
		podSpec.AutomountServiceAccountToken = &automount
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: b.labels(spec.Service)},
		Spec:       podSpec,
	}
}

// secretKeyForEnv maps an env var name to its conventional Secret key
// (DATABASE_URL → database-url, GITEA_ADMIN_TOKEN → gitea-admin-token).
func secretKeyForEnv(envVar string) string {
	return strings.ToLower(strings.ReplaceAll(envVar, "_", "-"))
}

func tcpProbe(initialDelay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http")}},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
	}
}

func defaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

func optionalSecretRef(name, key string) *corev1.EnvVarSource {
	optional := true
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
		Optional:             &optional,
	}}
}

// applyEnvOverrides upserts each override as an env var on the first container.
// Config keys are env var names so an admin can tune a service (e.g. ALLOWED_IMAGES)
// without redefining its whole spec.
func applyEnvOverrides(pt *corev1.PodTemplateSpec, env map[string]string) {
	if len(env) == 0 || len(pt.Spec.Containers) == 0 {
		return
	}
	c := &pt.Spec.Containers[0]
	// Apply in sorted key order so newly-appended overrides land deterministically and
	// repeated reconciles produce a byte-identical pod template (no churn).
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := env[k]
		set := false
		for i := range c.Env {
			if c.Env[i].Name == k {
				c.Env[i] = corev1.EnvVar{Name: k, Value: v}
				set = true
				break
			}
		}
		if !set {
			c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: v})
		}
	}
}

func mergeLabels(into, add map[string]string) map[string]string {
	if into == nil {
		into = map[string]string{}
	}
	for k, v := range add {
		into[k] = v
	}
	return into
}

// applyDeployment creates the Deployment, or updates the existing one's spec in
// place (preserving identity/resourceVersion) so reconfigure rolls the pods.
//
// replicaFloor marks the desired replica count as a floor rather than an exact
// target: when true, an existing Deployment already scaled ABOVE the floor (by an
// admin or an HPA) keeps its higher count, so reconcile never fights an upward
// scale — it only ever raises a workload back up to the floor. When false (an
// explicit per-service replicas override, or fixed-size infra), the count is
// applied exactly. A reconcile whose rendered spec matches the stored spec-hash
// annotation is a no-op and skips the Update.
func (b *k8sBackend) applyDeployment(ctx context.Context, dep *appsv1.Deployment, replicaFloor bool) error {
	api := b.client.AppsV1().Deployments(b.namespace)
	existing, err := api.Get(ctx, dep.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		stampSpecHash(&dep.ObjectMeta, dep.Spec)
		_, err = api.Create(ctx, dep, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	// Only overwrite a Deployment builder owns. A same-named resource rendered by
	// the Helm chart (managed-by=Helm) must never be hijacked — replacing its spec
	// would drop the chart's probes/resources/affinity and re-stamp it as ours.
	if existing.Labels[labelManagedBy] != managedByValue {
		return fmt.Errorf("refusing to overwrite Deployment %q owned by %q, not %s", dep.Name, existing.Labels[labelManagedBy], managedByValue)
	}
	// Preserve a higher externally-set replica count when the desired count is only a
	// floor, so an admin/HPA scale-up survives the next reconcile.
	if replicaFloor && existing.Spec.Replicas != nil && dep.Spec.Replicas != nil && *existing.Spec.Replicas > *dep.Spec.Replicas {
		dep.Spec.Replicas = existing.Spec.Replicas
	}
	hash := specHash(dep.Spec)
	if existing.Annotations[annotationSpecHash] == hash {
		return nil // unchanged since the last apply — skip the no-op Update
	}
	existing.Labels = mergeLabels(existing.Labels, dep.Labels)
	stampSpecHash(&existing.ObjectMeta, dep.Spec)
	existing.Spec = dep.Spec
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// stampSpecHash records the spec-hash annotation on an object's metadata.
func stampSpecHash(meta *metav1.ObjectMeta, spec any) {
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[annotationSpecHash] = specHash(spec)
}

func (b *k8sBackend) applyService(ctx context.Context, service string, port int32) error {
	name := b.name(service)
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: b.labels(service)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: b.selector(service),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       port,
				TargetPort: intstr.FromString("http"),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	api := b.client.CoreV1().Services(b.namespace)
	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	// As with applyDeployment: never overwrite a chart-owned Service's
	// selector/ports — that would redirect or break the platform's own Service.
	if existing.Labels[labelManagedBy] != managedByValue {
		return fmt.Errorf("refusing to overwrite Service %q owned by %q, not %s", name, existing.Labels[labelManagedBy], managedByValue)
	}
	existing.Labels = mergeLabels(existing.Labels, desired.Labels)
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// applyPDB keeps a PodDisruptionBudget in sync with the replica count so node drains
// and other involuntary evictions can't take the workload below its floor. minAvailable
// is replicas-1: a drain may evict one pod at a time while the rest stay up. A single
// replica gets NO PDB (minAvailable on one pod would wedge every drain), and any stale
// PDB from a prior higher count is removed.
func (b *k8sBackend) applyPDB(ctx context.Context, service string, replicas int32) error {
	name := b.name(service)
	api := b.client.PolicyV1().PodDisruptionBudgets(b.namespace)

	if replicas < 2 {
		if err := api.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	minAvail := intstr.FromInt(int(replicas - 1))
	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: b.labels(service)},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector:     &metav1.LabelSelector{MatchLabels: b.selector(service)},
		},
	}
	existing, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[labelManagedBy] != managedByValue {
		return fmt.Errorf("refusing to overwrite PodDisruptionBudget %q owned by %q, not %s", name, existing.Labels[labelManagedBy], managedByValue)
	}
	existing.Labels = mergeLabels(existing.Labels, desired.Labels)
	existing.Spec = desired.Spec
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}
