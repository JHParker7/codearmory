package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var deploymentGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// deployModule redeploys workloads in the target cluster on command. It is the
// actuator that closes the CI loop without the control plane ever holding cluster
// credentials: the control plane enqueues a command, and this — running inside the
// target cluster — patches the Deployment.
//
// Two command types:
//   - "rollout": a rolling restart (sets the same restart annotation `kubectl rollout
//     restart` uses), so the pods re-pull and re-create. Use for a mutable tag (:ci).
//   - "set-image": point a container at a new image, which also triggers a rollout.
//     Use to deploy a specific freshly-built tag.
//
// Both are imperative and return immediately with an event; the rollout itself is
// asynchronous in Kubernetes, as it always is.
type deployModule struct {
	client       dynamic.Interface
	defaultNS    string
	restartAnnot string
}

func newDeployModule(client dynamic.Interface) *deployModule {
	return &deployModule{
		client: client,
		// The namespace a command targets when it names none — the outpost's own
		// namespace by default, which is where the workloads it manages usually live.
		defaultNS: envOrDefault("DEPLOY_NAMESPACE", envOrDefault("POD_NAMESPACE", "default")),
		// The same annotation `kubectl rollout restart` stamps, so a restart triggered
		// here is indistinguishable from a manual one and tools read it the same way.
		restartAnnot: "kubectl.kubernetes.io/restartedAt",
	}
}

func (m *deployModule) Name() string { return "deploy" }

func (m *deployModule) HandleCommand(ctx context.Context, c Command) ([]Event, error) {
	switch c.Type {
	case "rollout":
		return m.rollout(ctx, c)
	case "set-image":
		return m.setImage(ctx, c)
	default:
		return nil, fmt.Errorf("deploy: unknown command type %q", c.Type)
	}
}

func (m *deployModule) namespace(c Command) string {
	if ns := payloadString(c.Payload, "namespace"); ns != "" {
		return ns
	}
	return m.defaultNS
}

// rollout does a rolling restart by stamping the restart annotation on the pod
// template — the same mechanism as `kubectl rollout restart`.
func (m *deployModule) rollout(ctx context.Context, c Command) ([]Event, error) {
	name := payloadString(c.Payload, "deployment")
	if name == "" {
		return nil, fmt.Errorf("deploy: rollout missing deployment")
	}
	ns := m.namespace(c)
	stamp := time.Now().UTC().Format(time.RFC3339)
	// JSON merge patch: merges this one annotation into the pod template without
	// disturbing the rest of the spec or other annotations.
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{m.restartAnnot: stamp},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if _, err := m.client.Resource(deploymentGVR).Namespace(ns).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("deploy: rollout %s/%s: %w", ns, name, err)
	}
	slog.Info("deploy: rollout restarted", "namespace", ns, "deployment", name)
	return []Event{{
		Integration: "deploy",
		Type:        "rollout-started",
		Payload:     map[string]any{"deployment": name, "namespace": ns, "restarted_at": stamp},
	}}, nil
}

// setImage points a container at a new image. A STRATEGIC merge patch is required so
// the container list merges by name — a plain merge patch would replace the whole
// list and drop every other container.
func (m *deployModule) setImage(ctx context.Context, c Command) ([]Event, error) {
	name := payloadString(c.Payload, "deployment")
	image := payloadString(c.Payload, "image")
	if name == "" || image == "" {
		return nil, fmt.Errorf("deploy: set-image requires deployment and image")
	}
	ns := m.namespace(c)
	// ignore_missing lets a caller (e.g. a CI redeploy fanning out over every changed
	// service) target a deployment that may not exist in this cluster — a changed
	// service with no control-plane Deployment (an agent, a sidecar) — and get a
	// "skipped" event instead of a hard failure that would fail the whole run.
	ignoreMissing := payloadBool(c.Payload, "ignore_missing")

	// Fetch the deployment up front: it resolves the sole container when the caller
	// gave none, and lets a NotFound be handled as skip-or-error in one place.
	dep, err := m.client.Resource(deploymentGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && ignoreMissing {
			slog.Info("deploy: set-image skipped (deployment absent)", "namespace", ns, "deployment", name, "image", image)
			return []Event{{
				Integration: "deploy",
				Type:        "image-skipped",
				Payload:     map[string]any{"deployment": name, "namespace": ns, "image": image, "reason": "deployment not found"},
			}}, nil
		}
		return nil, fmt.Errorf("deploy: get %s/%s: %w", ns, name, err)
	}

	container := payloadString(c.Payload, "container")
	if container == "" {
		// Default to the sole container when unambiguous; otherwise the caller must
		// say which one, since a strategic merge is keyed on the container name.
		resolved, rerr := soleContainerOf(dep.Object, ns, name)
		if rerr != nil {
			return nil, rerr
		}
		container = resolved
	}

	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{{"name": container, "image": image}},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if _, err := m.client.Resource(deploymentGVR).Namespace(ns).
		Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("deploy: set-image %s/%s container=%s: %w", ns, name, container, err)
	}
	slog.Info("deploy: image updated", "namespace", ns, "deployment", name, "container", container, "image", image)
	return []Event{{
		Integration: "deploy",
		Type:        "image-updated",
		Payload:     map[string]any{"deployment": name, "namespace": ns, "container": container, "image": image},
	}}, nil
}

// soleContainerOf returns the deployment's container name when it has exactly one, so
// set-image can omit `container` for the common single-container case. It errors on
// zero or many, because a strategic merge without a name is ambiguous. It operates on
// an already-fetched Deployment object so set-image does a single Get.
func soleContainerOf(obj map[string]any, ns, name string) (string, error) {
	containers, found, err := unstructuredContainers(obj)
	if err != nil || !found {
		return "", fmt.Errorf("deploy: %s/%s has no readable containers", ns, name)
	}
	if len(containers) != 1 {
		return "", fmt.Errorf("deploy: %s/%s has %d containers; specify `container`", ns, name, len(containers))
	}
	first, _ := containers[0].(map[string]any)
	cname, _ := first["name"].(string)
	if cname == "" {
		return "", fmt.Errorf("deploy: %s/%s container has no name", ns, name)
	}
	return cname, nil
}

// unstructuredContainers pulls spec.template.spec.containers out of an unstructured
// Deployment. Split out so it can be unit-tested without a cluster.
func unstructuredContainers(obj map[string]any) ([]any, bool, error) {
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	tmpl, ok := spec["template"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	pspec, ok := tmpl["spec"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	c, ok := pspec["containers"].([]any)
	return c, ok, nil
}

// Start is a no-op: deploy is a fire-and-forget actuator. The rollout it triggers is
// asynchronous in Kubernetes anyway, and a caller that wants completion watches the
// Deployment's status. A rollout-status watch could be added here later without
// touching the command path.
func (m *deployModule) Start(ctx context.Context, emit func(Event)) error { return nil }
