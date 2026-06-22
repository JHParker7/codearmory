package main

import (
	"context"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// minReadySeconds requires a new pod to stay Ready before a rollout proceeds to the
	// next, so a crash-looping image can't churn the whole set unnoticed.
	minReadySeconds = 10
)

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

// knownServicePorts lets the template fallback pick a container port for a
// platform service when there is no base Deployment to clone the port from.
var knownServicePorts = map[string]int32{
	"forge":             8083,
	"workflows":         8085,
	"tickets":           8086,
	"hooks":             8087,
	"gitea_integration": 8088,
	"containers":        8089,
	"chaos":             8090,
	"argo":              8091,
	"outpost-gateway":   8092,
	"blueprints":        8093,
	"notifications":     8094,
}

type k8sBackend struct {
	client    kubernetes.Interface
	namespace string // instance namespace workloads are created in
	prefix    string // release fullname prefix, e.g. "codearmory" → codearmory-forge
	registry  string // image registry for the template fallback
	tag       string // image tag for the template fallback
	prov      provisioningConfig

	// defaultReplicas is the replica floor applied to every managed workload that does
	// not set its own (0 => 1). rotateInterval is the default periodic rolling-restart
	// cadence (0 => off). Both are overridable per service via reserved config knobs.
	defaultReplicas int32
	rotateInterval  time.Duration
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
	return &k8sBackend{client: client, namespace: namespace, prefix: prefix, registry: registry, tag: tag, nowFn: time.Now}, nil
}

func (b *k8sBackend) name(service string) string { return b.prefix + "-" + service }

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
		if err := b.provision(ctx, spec.Service, spec.DBUrl); err != nil {
			return fmt.Errorf("provision %s: %w", spec.Service, err)
		}
	}
	dep, err := b.buildDeployment(ctx, spec)
	if err != nil {
		return err
	}
	if err := b.applyDeployment(ctx, dep); err != nil {
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
	return nil
}

func (b *k8sBackend) RemoveService(ctx context.Context, service string) error {
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
	if b.provisioningOn() {
		b.deprovision(ctx, service)
	}
	return nil
}

// ListManaged returns the service names of every builder-managed Deployment in the
// namespace, so the reconciler can reclaim ones no longer desired.
func (b *k8sBackend) ListManaged(ctx context.Context) ([]string, error) {
	list, err := b.client.AppsV1().Deployments(b.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
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

// stampRotation writes the current interval bucket onto the pod template. Truncating
// to the interval means the value is identical for every reconcile within a window
// (so the update is a no-op and the pods are left alone) and flips exactly once per
// interval (rolling the pods back to the image). now() is bucketed in UTC so the
// boundaries are stable regardless of the cluster's timezone.
func (b *k8sBackend) stampRotation(pt *corev1.PodTemplateSpec, interval time.Duration) {
	now := time.Now
	if b.nowFn != nil {
		now = b.nowFn
	}
	if pt.Annotations == nil {
		pt.Annotations = map[string]string{}
	}
	pt.Annotations[annotationRotatedAt] = now().UTC().Truncate(interval).Format(time.RFC3339)
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
	if p, ok := knownServicePorts[service]; ok {
		return p
	}
	return 8080
}

// templatePod renders the minimal pod for a service with no base to clone: the
// image (explicit, or registry/service:tag) on its port, with the standard
// gatekeeper/db wiring sourced from the conventional per-service secret.
func (b *k8sBackend) templatePod(spec workloadSpec) corev1.PodTemplateSpec {
	image := spec.Image
	if image == "" {
		image = fmt.Sprintf("%s/%s:%s", b.registry, spec.Service, b.tag)
	}
	port := spec.Port
	if port == 0 {
		if p, ok := knownServicePorts[spec.Service]; ok {
			port = p
		} else {
			port = 8080
		}
	}
	secretName := b.name(spec.Service)
	runAsNonRoot := true
	allowPriv := false
	readOnly := true
	env := []corev1.EnvVar{
		{Name: "PORT", Value: fmt.Sprintf("%d", port)},
		{Name: "GATEKEEPER_URL", Value: fmt.Sprintf("http://%s-gatekeeper:8081", b.prefix)},
		{Name: "REGISTRY_URL", Value: fmt.Sprintf("http://%s-registry:8082", b.prefix)},
	}
	// Wire the conventional per-service secret keys as optional refs: whichever the
	// service's existing Secret actually carries get set, the rest are skipped. The
	// Secret is provisioned by the chart even for not-yet-deployed services, so a
	// templated workload still authenticates. Service-specific plaintext env (e.g.
	// forge's RUNTIME) comes from the admin's config (applied as overrides).
	for envName, key := range templateSecretEnv {
		env = append(env, corev1.EnvVar{Name: envName, ValueFrom: optionalSecretRef(secretName, key)})
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: b.labels(spec.Service)},
		Spec: corev1.PodSpec{
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
			}},
		},
	}
}

// templateSecretEnv maps env var names to the conventional per-service Secret keys
// the chart provisions. All are wired as optional, so a service only receives the
// keys its Secret actually holds.
var templateSecretEnv = map[string]string{
	"DATABASE_URL":           "database-url",
	"GATEKEEPER_SERVICE_KEY": "gatekeeper-service-key",
	"CONDUCTOR_FORWARD_KEY":  "conductor-forward-key",
	"REDIS_URL":              "redis-url",
	"REGISTRY_SERVICE_KEY":   "registry-service-key",
	"HOOKS_TRIGGER_KEY":      "hooks-trigger-key",
	"OUTPOST_INTERNAL_KEY":   "outpost-internal-key",
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
	for k, v := range env {
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
func (b *k8sBackend) applyDeployment(ctx context.Context, dep *appsv1.Deployment) error {
	api := b.client.AppsV1().Deployments(b.namespace)
	existing, err := api.Get(ctx, dep.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
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
	existing.Labels = mergeLabels(existing.Labels, dep.Labels)
	existing.Spec = dep.Spec
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
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
