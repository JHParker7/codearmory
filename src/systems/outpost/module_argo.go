package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var argoAppGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// argoModule is the second integration, proving the framework generalises with
// no change to the outpost core or gateway. It drives Argo CD Applications
// in-cluster via the same dynamic client: a "sync" command sets the
// Application's `operation` (which Argo CD reconciles), and a watch surfaces
// health/sync status as app-state events. No separate Argo API credentials are
// needed — the outpost is still the sole in-cluster actuator.
type argoModule struct {
	client    dynamic.Interface
	namespace string // Argo CD installation namespace (where Applications live)
	resync    time.Duration
}

func newArgoModule(client dynamic.Interface) *argoModule {
	return &argoModule{
		client:    client,
		namespace: envOrDefault("ARGO_NAMESPACE", "argocd"),
		resync:    30 * time.Second,
	}
}

func (m *argoModule) Name() string { return "argo" }

func (m *argoModule) HandleCommand(ctx context.Context, c Command) ([]Event, error) {
	switch c.Type {
	case "sync":
		return m.sync(ctx, c)
	default:
		return nil, fmt.Errorf("argo: unknown command type %q", c.Type)
	}
}

func (m *argoModule) sync(ctx context.Context, c Command) ([]Event, error) {
	app := payloadString(c.Payload, "app_name")
	if app == "" {
		return nil, fmt.Errorf("argo: sync missing app_name")
	}
	syncID := payloadString(c.Payload, "sync_id")
	revision := payloadString(c.Payload, "revision")
	if revision == "" {
		revision = "HEAD"
	}
	// Setting the Application's top-level `operation` triggers Argo CD to run a
	// sync. We merge-patch so we don't disturb spec.
	operation := map[string]any{
		"operation": map[string]any{
			"initiatedBy": map[string]any{"username": "codearmory-outpost"},
			"sync": map[string]any{
				"revision": revision,
				"syncStrategy": map[string]any{
					"hook": map[string]any{},
				},
			},
		},
	}
	patch, err := json.Marshal(operation)
	if err != nil {
		return nil, err
	}
	_, err = m.client.Resource(argoAppGVR).Namespace(m.namespace).
		Patch(ctx, app, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("argo: trigger sync for %s/%s: %w", m.namespace, app, err)
	}
	slog.Info("argo: sync triggered", "app", app, "namespace", m.namespace, "revision", revision)
	return []Event{{
		Integration: "argo",
		Type:        "sync-started",
		// sync_id is what the consumer correlates the sync-started event to; without
		// it markSyncRunning can never flip the sync to running.
		Payload: map[string]any{"sync_id": syncID, "app_name": app, "revision": revision},
	}}, nil
}

// Start watches Argo CD Applications and emits app-state events on status change.
func (m *argoModule) Start(ctx context.Context, emit func(Event)) error {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(m.client, m.resync, m.namespace, nil)
	informer := factory.ForResource(argoAppGVR).Informer()
	handler := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		emit(m.appToEvent(u))
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handler,
		UpdateFunc: func(_, newObj any) { handler(newObj) },
	})
	if err != nil {
		return fmt.Errorf("argo: add application informer handler: %w", err)
	}
	go factory.Start(ctx.Done())
	slog.Info("argo: Application informer started", "namespace", m.namespace)
	return nil
}

func (m *argoModule) appToEvent(u *unstructured.Unstructured) Event {
	syncStatus, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	healthStatus, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	revision, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision")
	opPhase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
	return Event{
		Integration: "argo",
		Type:        "app-state",
		Payload: map[string]any{
			"app_name":        u.GetName(),
			"sync_status":     syncStatus,
			"health_status":   healthStatus,
			"revision":        revision,
			"operation_phase": opPhase,
		},
	}
}
