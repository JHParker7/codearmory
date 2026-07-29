package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// App infra a service needs but that is not itself a routed service: a service's
// managed Redis cache, and the PVC a service that keeps durable state on disk is
// mounted. (Backing stores — Postgres — are admin-supplied via connection URLs and
// never deployed by builder.) Infra objects are labelled infra-of=<parent> so
// ListManaged ignores them and they are torn down with their parent rather than
// reclaimed as orphans by the desired-state diff — except the PVC, which is created
// but never deleted (see teardownInfra).

const (
	labelInfraOf = "codearmory.io/infra-of"

	// redisComponentSuffix names the managed-Redis component per parent service
	// (<k8sName>-redis), so the Deployment/Service and the in-cluster URL the service
	// connects to all agree. defaultRedisImage is the stateless store's image; an
	// operator overrides it (private mirror / pinned digest) with BUILDER_REDIS_IMAGE.
	redisComponentSuffix = "redis"
	redisPort            = 6379
	defaultRedisImage    = "redis:7-alpine"

	// pvcComponentSuffix names a service's durable volume (<k8sName>-data) and
	// defaultPVCSize is the capacity used when a def declares persistence with no size.
	pvcComponentSuffix = "data"
	defaultPVCSize     = "10Gi"
	// redisUser is the uid of the official image's redis user; runAsUser pins it so the
	// container runs non-root without the entrypoint's root-then-step-down dance.
	redisUser = int64(999)
)

func (b *k8sBackend) infraLabels(component, parent string) map[string]string {
	return map[string]string{
		labelName:      "codearmory",
		labelInstance:  b.prefix,
		labelComponent: component,
		labelManagedBy: managedByValue,
		labelInfraOf:   parent,
	}
}

func (b *k8sBackend) infraSelector(component string) map[string]string {
	return map[string]string{
		labelName:      "codearmory",
		labelInstance:  b.prefix,
		labelComponent: component,
	}
}

// ensureInfra creates the stateless app infra a service's def declares.
func (b *k8sBackend) ensureInfra(ctx context.Context, spec workloadSpec) error {
	def, ok := embeddedServiceDef(spec.Service)
	if !ok {
		return nil
	}
	// Managed Redis: deploy an in-cluster store only when the admin supplied no external
	// REDIS_URL. Supplying one opts out and tears any builder-managed Redis back down, so
	// switching to an external Redis on a later reconcile cleans up. The redis-url Secret
	// key is written by ensureServiceSecret; here we own only the workload.
	if def.Infra.ManagedRedis {
		if spec.Secrets["REDIS_URL"] == "" {
			if err := b.ensureManagedRedis(ctx, spec.Service); err != nil {
				return fmt.Errorf("managed redis: %w", err)
			}
		} else {
			b.deleteManagedRedis(ctx, spec.Service)
		}
	}
	// Durable storage: the claim must exist before the workload references it, or the
	// pod stays Pending on a missing volume until the next reconcile.
	if p := def.Infra.Persistence; p != nil {
		if err := b.ensurePVC(ctx, spec.Service, *p); err != nil {
			return fmt.Errorf("persistence: %w", err)
		}
	}
	// Register the service as a clone source in git_connector. Unlike the steps above,
	// a failure here is logged and dropped rather than returned: the link is not
	// something the pod needs in order to start, and git_connector is frequently not
	// reachable yet on the pass that first brings a git host up. The next reconcile
	// retries, and the endpoint is an upsert, so converging late costs nothing.
	if link := def.GitConnectorBackend; link != nil {
		if err := b.ensureGitConnectorBackend(ctx, spec.Service, *link); err != nil {
			slog.WarnContext(ctx, "git connector link deferred to next reconcile",
				"service", spec.Service, "error", err)
		}
	}
	return nil
}

// teardownInfra removes the infra a torn-down service owns. Note what it does NOT
// remove: a persistence PVC. Disabling a service must not destroy its data — same rule
// as the db providers' teardown, which never drops a database. Reclaiming the volume
// stays a deliberate admin action (and builder is not even granted delete on PVCs).
func (b *k8sBackend) teardownInfra(ctx context.Context, service string) {
	def, ok := embeddedServiceDef(service)
	if !ok {
		return
	}
	if def.Infra.ManagedRedis {
		b.deleteManagedRedis(ctx, service)
	}
}

// persistenceFor returns the durable-volume declaration for a service, or nil when it
// declares none (the common, stateless case).
func persistenceFor(service string) *svcPersistence {
	def, ok := embeddedServiceDef(service)
	if !ok {
		return nil
	}
	return def.Infra.Persistence
}

// pvcComponent / pvcName name a service's data volume (<k8sName>-data), on the same
// DNS-1123 k8sName the Deployment/Service use.
func (b *k8sBackend) pvcComponent(service string) string {
	return k8sNameFor(service) + "-" + pvcComponentSuffix
}

func (b *k8sBackend) pvcName(service string) string {
	return b.prefix + "-" + b.pvcComponent(service)
}

// ensurePVC creates the service's data volume if it is absent, and otherwise leaves it
// completely alone. This is deliberately create-only: a PVC's spec is near-immutable
// (storage class and access mode cannot change at all, and capacity only grows, only on
// an expandable class), so a reconcile that tried to "converge" it would fail on every
// pass. Growing the volume stays a deliberate admin action — edit the claim, or the
// def and hand-apply. Labelled infra-of=<parent> like the managed Redis so ListManaged
// skips it; unlike the Redis, teardown never deletes it.
func (b *k8sBackend) ensurePVC(ctx context.Context, parent string, p svcPersistence) error {
	name := b.pvcName(parent)
	api := b.client.CoreV1().PersistentVolumeClaims(b.namespace)
	if _, err := api.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil // already exists — never updated
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	size := strings.TrimSpace(p.Size)
	if size == "" {
		size = defaultPVCSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("parse size %q for %s: %w", size, parent, err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: b.namespace,
			Labels:    b.infraLabels(b.pvcComponent(parent), parent),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{p.accessMode()},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: qty}},
		},
	}
	if sc := strings.TrimSpace(p.StorageClass); sc != "" {
		pvc.Spec.StorageClassName = &sc
	}
	if _, err := api.Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	slog.InfoContext(ctx, "created persistent volume claim", "service", parent, "pvc", name, "size", size, "accessMode", p.accessMode())
	return nil
}

// redisComponent is the managed-Redis component name for a service: its DNS-1123
// k8sName plus "-redis", so the object names and the connection URL stay valid even
// for a service whose registry name has an underscore.
func (b *k8sBackend) redisComponent(service string) string {
	return k8sNameFor(service) + "-" + redisComponentSuffix
}

// managedRedisURL is the in-cluster URL the parent service connects to — the ClusterIP
// Service ensureManagedRedis creates (codearmory-<k8sName>-redis:6379).
func (b *k8sBackend) managedRedisURL(service string) string {
	return fmt.Sprintf("redis://%s-%s:%d", b.prefix, b.redisComponent(service), redisPort)
}

func redisImage() string {
	if v := strings.TrimSpace(os.Getenv("BUILDER_REDIS_IMAGE")); v != "" {
		return v
	}
	return defaultRedisImage
}

// ensureManagedRedis deploys a single-replica, persistence-off Redis for a service that
// declares managedRedis and was given no external REDIS_URL. The store is ephemeral (an
// emptyDir wiped on restart/rotation) — appropriate for a cache, not durable state. It is
// labelled infra-of=<parent> so ListManaged skips it and teardown reclaims it with the
// parent. Fixed single replica: applied exactly, not as a floor.
func (b *k8sBackend) ensureManagedRedis(ctx context.Context, parent string) error {
	component := b.redisComponent(parent)
	name := b.prefix + "-" + component
	labels := b.infraLabels(component, parent)
	selector := b.infraSelector(component)
	replicas := int32(1)
	runAsNonRoot := true
	uid := redisUser
	allowPriv := false
	readOnly := true

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot, RunAsUser: &uid, FSGroup: &uid},
					Containers: []corev1.Container{{
						Name:  redisComponentSuffix,
						Image: redisImage(),
						// Persistence fully off: no RDB snapshots, no AOF — a pure in-memory
						// cache. The emptyDir at /data only satisfies redis's working dir.
						Command: []string{"redis-server", "--save", "", "--appendonly", "no"},
						// Named "http" so the shared applyService (TargetPort "http") selects it.
						Ports:        []corev1.ContainerPort{{Name: "http", ContainerPort: redisPort, Protocol: corev1.ProtocolTCP}},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPriv,
							ReadOnlyRootFilesystem:   &readOnly,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						ReadinessProbe: tcpProbe(5, 10),
						Resources:      defaultResources(),
					}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			},
		},
	}
	for _, n := range b.imagePullSecrets {
		dep.Spec.Template.Spec.ImagePullSecrets = append(dep.Spec.Template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: n})
	}
	// Fixed single replica — apply the count exactly (not as a floor).
	if err := b.applyDeployment(ctx, dep, false); err != nil {
		return err
	}
	// The Service name (b.name(component)) and selector match the Deployment; teardown
	// removes both by name.
	return b.applyService(ctx, component, redisPort)
}

func (b *k8sBackend) deleteManagedRedis(ctx context.Context, parent string) {
	name := b.prefix + "-" + b.redisComponent(parent)
	if err := b.client.AppsV1().Deployments(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete managed redis deployment failed", "parent", parent, "error", err)
	}
	if err := b.client.CoreV1().Services(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete managed redis service failed", "parent", parent, "error", err)
	}
}
