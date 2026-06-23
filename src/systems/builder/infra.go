package main

import (
	"context"
	"fmt"
	"log/slog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Stateless app infra a service needs but that is not itself a routed service: forge's
// egress-proxy and its exec-namespace RBAC. (Backing stores — Postgres, Redis — are
// admin-supplied via connection URLs and never deployed by builder.) Infra objects are
// labelled infra-of=<parent> so ListManaged ignores them and they are torn down with
// their parent rather than reclaimed as orphans by the desired-state diff.

const (
	labelInfraOf         = "codearmory.io/infra-of"
	egressProxyComponent = "egress-proxy"
	egressProxyPort      = 3128

	// annotationExecNamespace records the exec namespace on forge's ServiceAccount so
	// teardown can reclaim the Role/RoleBinding/NetworkPolicy from wherever they were
	// created — which is K8S_NAMESPACE when overridden, not always the release namespace.
	annotationExecNamespace = "codearmory.io/exec-namespace"

	// defaultEgressAllowedDomains mirrors the chart's egress-proxy allowlist. An admin
	// overrides it with PROXY_ALLOWED_DOMAINS in forge's config.
	defaultEgressAllowedDomains = "registry.terraform.io,releases.hashicorp.com,github.com,*.github.com,raw.githubusercontent.com,objects.githubusercontent.com,registry.npmjs.org,pypi.org,files.pythonhosted.org,proxy.golang.org,sum.golang.org,storage.googleapis.com"
)

// execNamespace is where forge runs its sandbox Jobs (and thus where its Role lives).
// Defaults to the release namespace so no separate namespace need exist; an admin can
// point it elsewhere with K8S_NAMESPACE.
func (b *k8sBackend) execNamespace(spec workloadSpec) string {
	if ns := spec.Env["K8S_NAMESPACE"]; ns != "" {
		return ns
	}
	return b.namespace
}

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
	if def.Infra.ForgeExecRBAC {
		if err := b.ensureForgeRBAC(ctx, spec); err != nil {
			return fmt.Errorf("forge rbac: %w", err)
		}
	}
	if def.Infra.NetworkPolicy {
		if err := b.ensureForgeNetworkPolicy(ctx, spec); err != nil {
			return fmt.Errorf("forge network policy: %w", err)
		}
	}
	if def.Infra.EgressProxy {
		domains := defaultEgressAllowedDomains
		if v := spec.Env["PROXY_ALLOWED_DOMAINS"]; v != "" {
			domains = v
		}
		if err := b.ensureEgressProxy(ctx, spec.Service, domains); err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
	}
	return nil
}

// teardownInfra removes the infra a torn-down service owns. The exec-namespace
// objects (RBAC, NetworkPolicy) may live in an overridden K8S_NAMESPACE, recorded on
// the ServiceAccount at create time — resolve it so teardown doesn't orphan them.
func (b *k8sBackend) teardownInfra(ctx context.Context, service string) {
	def, ok := embeddedServiceDef(service)
	if !ok {
		return
	}
	if def.Infra.EgressProxy {
		b.deleteEgressProxy(ctx, service)
	}
	execNS := b.recordedExecNamespace(ctx, service)
	if def.Infra.NetworkPolicy {
		b.deleteForgeNetworkPolicy(ctx, service, execNS)
	}
	if def.Infra.ForgeExecRBAC {
		b.deleteForgeRBAC(ctx, service, execNS)
	}
}

// recordedExecNamespace reads the exec namespace recorded on the service's
// ServiceAccount (annotationExecNamespace), falling back to the release namespace
// when the SA or annotation is absent.
func (b *k8sBackend) recordedExecNamespace(ctx context.Context, service string) string {
	sa, err := b.client.CoreV1().ServiceAccounts(b.namespace).Get(ctx, b.name(service), metav1.GetOptions{})
	if err == nil {
		if ns := sa.Annotations[annotationExecNamespace]; ns != "" {
			return ns
		}
	}
	return b.namespace
}

func (b *k8sBackend) ensureEgressProxy(ctx context.Context, parent, allowedDomains string) error {
	name := b.prefix + "-" + egressProxyComponent
	labels := b.infraLabels(egressProxyComponent, parent)
	selector := b.infraSelector(egressProxyComponent)
	replicas := int32(1)
	runAsNonRoot := true
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
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot},
					Containers: []corev1.Container{{
						Name:  egressProxyComponent,
						Image: fmt.Sprintf("%s/%s:%s", b.registry, egressProxyComponent, b.tag),
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: egressProxyPort, Protocol: corev1.ProtocolTCP}},
						Env: []corev1.EnvVar{
							{Name: "PORT", Value: fmt.Sprintf("%d", egressProxyPort)},
							{Name: "PROXY_ALLOWED_DOMAINS", Value: allowedDomains},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPriv,
							ReadOnlyRootFilesystem:   &readOnly,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						ReadinessProbe: tcpProbe(5, 10),
						Resources:      defaultResources(),
					}},
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
	// The Service selects the same component label; teardown removes it by name.
	return b.applyService(ctx, egressProxyComponent, egressProxyPort)
}

func (b *k8sBackend) deleteEgressProxy(ctx context.Context, parent string) {
	name := b.prefix + "-" + egressProxyComponent
	if err := b.client.AppsV1().Deployments(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete egress-proxy deployment failed", "parent", parent, "error", err)
	}
	if err := b.client.CoreV1().Services(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete egress-proxy service failed", "parent", parent, "error", err)
	}
}

// ensureForgeRBAC grants forge the rights to create sandbox Jobs in its exec namespace:
// a ServiceAccount in the release namespace (the forge pod runs there) bound to a Role
// in the exec namespace. When the exec namespace is the release namespace (the default)
// these all live together.
func (b *k8sBackend) ensureForgeRBAC(ctx context.Context, spec workloadSpec) error {
	name := b.name(spec.Service) // codearmory-forge — also the pod's serviceAccountName
	execNS := b.execNamespace(spec)
	labels := b.infraLabels(spec.Service, spec.Service)

	// Record the exec namespace on the SA so teardown reclaims the Role/RoleBinding/
	// NetworkPolicy from the same namespace even when K8S_NAMESPACE is overridden.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   b.namespace,
		Labels:      labels,
		Annotations: map[string]string{annotationExecNamespace: execNS},
	}}
	if _, err := b.client.CoreV1().ServiceAccounts(b.namespace).Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := b.client.CoreV1().ServiceAccounts(b.namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("serviceaccount: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("get serviceaccount: %w", err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: execNS, Labels: labels},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create", "get", "list", "watch", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"pods", "pods/log"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		},
	}
	if err := b.upsertRole(ctx, role); err != nil {
		return err
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: execNS, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: b.namespace}},
	}
	return b.upsertRoleBinding(ctx, rb)
}

func (b *k8sBackend) deleteForgeRBAC(ctx context.Context, service, execNS string) {
	name := b.name(service)
	if err := b.client.RbacV1().RoleBindings(execNS).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete forge rolebinding failed", "error", err)
	}
	if err := b.client.RbacV1().Roles(execNS).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete forge role failed", "error", err)
	}
	if err := b.client.CoreV1().ServiceAccounts(b.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete forge serviceaccount failed", "error", err)
	}
}

// forgeNetworkPolicyName is the NetworkPolicy that isolates forge's sandbox Job pods.
// Named distinctly from the RBAC objects (which share b.name(service)) so the two never
// collide in the exec namespace.
func (b *k8sBackend) forgeNetworkPolicyName(service string) string {
	return b.name(service) + "-exec-egress"
}

// ensureForgeNetworkPolicy locks down egress from forge's sandbox Job pods: in
// Kubernetes runtime mode the Jobs run untrusted user code, so — mirroring the Docker
// forge-exec internal network — they may only reach DNS and the egress-proxy, which
// enforces the domain allowlist. Without it the FORGE_EGRESS_PROXY env is advisory and
// a malicious process could connect out directly, bypassing PROXY_ALLOWED_DOMAINS.
// (Enforcement requires a NetworkPolicy-aware CNI; on clusters without one it is inert,
// the same as before.)
func (b *k8sBackend) ensureForgeNetworkPolicy(ctx context.Context, spec workloadSpec) error {
	execNS := b.execNamespace(spec)
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)
	proxyPort := intstr.FromInt(egressProxyPort)
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.forgeNetworkPolicyName(spec.Service),
			Namespace: execNS,
			Labels:    b.infraLabels(spec.Service, spec.Service),
		},
		Spec: networkingv1.NetworkPolicySpec{
			// forge labels its sandbox Job pods app=forge (see forge's k8s runtime).
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "forge"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// DNS resolution (so the proxy's hostname resolves).
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &dnsPort},
					{Protocol: &tcp, Port: &dnsPort},
				}},
				// The egress-proxy in the release namespace — the only allowed egress path.
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": b.namespace}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{labelComponent: egressProxyComponent}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &proxyPort}},
				},
			},
		},
	}
	return b.upsertNetworkPolicy(ctx, np)
}

func (b *k8sBackend) deleteForgeNetworkPolicy(ctx context.Context, service, execNS string) {
	if err := b.client.NetworkingV1().NetworkPolicies(execNS).Delete(ctx, b.forgeNetworkPolicyName(service), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.WarnContext(ctx, "delete forge networkpolicy failed", "error", err)
	}
}

func (b *k8sBackend) upsertNetworkPolicy(ctx context.Context, np *networkingv1.NetworkPolicy) error {
	api := b.client.NetworkingV1().NetworkPolicies(np.Namespace)
	existing, err := api.Get(ctx, np.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, np, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[labelManagedBy] != managedByValue {
		return fmt.Errorf("refusing to overwrite NetworkPolicy %q not managed by builder", np.Name)
	}
	existing.Spec = np.Spec
	existing.Labels = mergeLabels(existing.Labels, np.Labels)
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (b *k8sBackend) upsertRole(ctx context.Context, role *rbacv1.Role) error {
	api := b.client.RbacV1().Roles(role.Namespace)
	existing, err := api.Get(ctx, role.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, role, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[labelManagedBy] != managedByValue {
		return fmt.Errorf("refusing to overwrite Role %q not managed by builder", role.Name)
	}
	existing.Rules = role.Rules
	existing.Labels = mergeLabels(existing.Labels, role.Labels)
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (b *k8sBackend) upsertRoleBinding(ctx context.Context, rb *rbacv1.RoleBinding) error {
	api := b.client.RbacV1().RoleBindings(rb.Namespace)
	existing, err := api.Get(ctx, rb.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, rb, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[labelManagedBy] != managedByValue {
		return fmt.Errorf("refusing to overwrite RoleBinding %q not managed by builder", rb.Name)
	}
	// RoleRef is immutable; only the subjects/labels are reconciled.
	existing.Subjects = rb.Subjects
	existing.Labels = mergeLabels(existing.Labels, rb.Labels)
	_, err = api.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}
