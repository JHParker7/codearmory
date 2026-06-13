package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

const enginePrefix = "exp-"

var (
	chaosEngineGVR = schema.GroupVersionResource{Group: "litmuschaos.io", Version: "v1alpha1", Resource: "chaosengines"}
	chaosResultGVR = schema.GroupVersionResource{Group: "litmuschaos.io", Version: "v1alpha1", Resource: "chaosresults"}
)

// chaosModule is the only net-new in-cluster actuator/collector for chaos. It
// translates run/stop commands into litmuschaos ChaosEngine CRs and surfaces
// ChaosResult verdicts as events. The control plane holds no cluster creds.
type chaosModule struct {
	client     dynamic.Interface
	serviceAcc string          // chaosServiceAccount referenced by every engine
	allowedNS  map[string]bool // namespaces chaos may target; empty = allowlist disabled
	resync     time.Duration
}

func newChaosModule(client dynamic.Interface) *chaosModule {
	return &chaosModule{
		client:     client,
		serviceAcc: envOrDefault("CHAOS_SERVICE_ACCOUNT", "litmus-admin"),
		allowedNS:  namespaceSet(os.Getenv("CHAOS_ALLOWED_NAMESPACES")),
		resync:     30 * time.Second,
	}
}

func (m *chaosModule) Name() string { return "chaos" }

// namespaceSet parses a CSV namespace allowlist into a set.
func namespaceSet(csv string) map[string]bool {
	set := map[string]bool{}
	for _, ns := range splitCSV(csv) {
		set[ns] = true
	}
	return set
}

// nsAllowed reports whether chaos may run in ns. The outpost is the customer's
// trust boundary, so it refuses to act on namespaces the operator did not
// authorize via CHAOS_ALLOWED_NAMESPACES even if the control plane asks. An
// empty allowlist disables the check (back-compat); the Helm chart always sets
// it from chaos.targetNamespaces so the supported install is fenced by default.
func (m *chaosModule) nsAllowed(ns string) bool {
	if len(m.allowedNS) == 0 {
		return true
	}
	return m.allowedNS[ns]
}

func (m *chaosModule) HandleCommand(ctx context.Context, c Command) ([]Event, error) {
	switch c.Type {
	case "run-experiment":
		return m.runExperiment(ctx, c)
	case "stop":
		return m.stopExperiment(ctx, c)
	default:
		return nil, fmt.Errorf("chaos: unknown command type %q", c.Type)
	}
}

func (m *chaosModule) runExperiment(ctx context.Context, c Command) ([]Event, error) {
	experimentID := payloadString(c.Payload, "experiment_id")
	experimentType := payloadString(c.Payload, "experiment_type")
	engine := payloadString(c.Payload, "engine_name")
	ns := payloadString(c.Payload, "target_app_ns")
	label := payloadString(c.Payload, "target_app_label")
	kind := payloadString(c.Payload, "target_app_kind")
	params := payloadStringMap(c.Payload, "params")
	if experimentID == "" || experimentType == "" || engine == "" || ns == "" || label == "" {
		return nil, fmt.Errorf("chaos: run-experiment missing required fields")
	}
	if kind == "" {
		kind = "deployment"
	}
	if !m.nsAllowed(ns) {
		return nil, fmt.Errorf("chaos: namespace %q is not in the allowed set", ns)
	}

	obj := m.buildChaosEngine(engine, ns, label, kind, experimentType, experimentID, params)
	_, err := m.client.Resource(chaosEngineGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	// A redelivered command (at-least-once) finds the engine already created;
	// treat that as success and re-emit run-started so the run is idempotent.
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("chaos: create ChaosEngine %s/%s: %w", ns, engine, err)
	}
	slog.Info("chaos: ChaosEngine created", "engine", engine, "namespace", ns, "experiment_type", experimentType, "experiment_id", experimentID)

	// Acknowledge the run started; the verdict arrives later via the informer.
	return []Event{{
		Integration: "chaos",
		Type:        "run-started",
		Payload:     map[string]any{"experiment_id": experimentID, "engine_name": engine},
	}}, nil
}

func (m *chaosModule) stopExperiment(_ context.Context, c Command) ([]Event, error) {
	engine := payloadString(c.Payload, "engine_name")
	ns := payloadString(c.Payload, "target_app_ns")
	if engine == "" || ns == "" {
		return nil, fmt.Errorf("chaos: stop missing engine_name or target_app_ns")
	}
	if !m.nsAllowed(ns) {
		return nil, fmt.Errorf("chaos: namespace %q is not in the allowed set", ns)
	}
	policy := metav1.DeletePropagationForeground
	// Use a fresh background context for cleanup so a cancelled command still
	// tears the engine down (mirrors forge's deleteJob).
	err := m.client.Resource(chaosEngineGVR).Namespace(ns).Delete(context.Background(), engine, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("chaos: delete ChaosEngine %s/%s: %w", ns, engine, err)
	}
	slog.Info("chaos: ChaosEngine stopped", "engine", engine, "namespace", ns)
	return nil, nil
}

// buildChaosEngine constructs the unstructured ChaosEngine. Field types mirror
// the operator's expectations exactly: spec.monitoring is a bool, applabel is a
// single key=value string, and every env value is a string.
func (m *chaosModule) buildChaosEngine(engine, ns, label, kind, experimentType, experimentID string, params map[string]string) *unstructured.Unstructured {
	env := make([]any, 0, len(params))
	for k, v := range params {
		env = append(env, map[string]any{"name": k, "value": v})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "litmuschaos.io/v1alpha1",
		"kind":       "ChaosEngine",
		"metadata": map[string]any{
			"name":      engine,
			"namespace": ns,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "codearmory-outpost",
				"codearmory.io/experiment-id":  experimentID,
			},
		},
		"spec": map[string]any{
			"appinfo": map[string]any{
				"appns":    ns,
				"applabel": label,
				"appkind":  kind,
			},
			"engineState":         "active",
			"chaosServiceAccount": m.serviceAcc,
			"monitoring":          true,
			"jobCleanUpPolicy":    "retain",
			"experiments": []any{
				map[string]any{
					"name": experimentType,
					"spec": map[string]any{
						"components": map[string]any{
							"env": env,
						},
					},
				},
			},
		},
	}}
}

// Start runs a ChaosResult informer cluster-wide and emits verdict events for
// engines this framework created (name prefix "exp-"). It reads
// status.experimentStatus defensively and never blocks.
func (m *chaosModule) Start(ctx context.Context, emit func(Event)) error {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(m.client, m.resync)
	informer := factory.ForResource(chaosResultGVR).Informer()
	handler := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		if ev, ok := m.resultToEvent(u); ok {
			emit(ev)
		}
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handler,
		UpdateFunc: func(_, newObj any) { handler(newObj) },
	})
	if err != nil {
		return fmt.Errorf("chaos: add result informer handler: %w", err)
	}
	go factory.Start(ctx.Done())
	slog.Info("chaos: ChaosResult informer started")
	return nil
}

// resultToEvent extracts a verdict event from a ChaosResult, recovering the
// experiment id from the engine name. It only emits when there is a decisive
// verdict or the experiment has completed, to avoid flooding "Awaited" updates.
func (m *chaosModule) resultToEvent(u *unstructured.Unstructured) (Event, bool) {
	engine, _, _ := unstructured.NestedString(u.Object, "spec", "engine")
	if engine == "" {
		// Fall back to the result name, which Litmus forms as "<engine>-<experiment>".
		// Strip the experiment suffix using spec.experiment — NOT the last hyphen,
		// since the engine name embeds a UUID experiment_id full of hyphens (trimming
		// the last segment would corrupt the recovered id and misattribute verdicts).
		experiment, _, _ := unstructured.NestedString(u.Object, "spec", "experiment")
		name := u.GetName()
		if experiment != "" {
			engine = strings.TrimSuffix(name, "-"+experiment)
		} else {
			engine = name
		}
	}
	if !strings.HasPrefix(engine, enginePrefix) {
		return Event{}, false
	}
	experimentID := strings.TrimPrefix(engine, enginePrefix)

	verdict, _, _ := unstructured.NestedString(u.Object, "status", "experimentStatus", "verdict")
	phase, _, _ := unstructured.NestedString(u.Object, "status", "experimentStatus", "phase")
	failStep, _, _ := unstructured.NestedString(u.Object, "status", "experimentStatus", "failStep")
	probe := nestedToString(u.Object, "status", "experimentStatus", "probeSuccessPercentage")

	decisive := verdict != "" && !strings.EqualFold(verdict, "Awaited")
	completed := strings.EqualFold(phase, "Completed") || strings.EqualFold(phase, "Stopped")
	if !decisive && !completed {
		return Event{}, false
	}
	return Event{
		Integration: "chaos",
		Type:        "verdict",
		Payload: map[string]any{
			"experiment_id":            experimentID,
			"verdict":                  verdict,
			"phase":                    phase,
			"fail_step":                failStep,
			"probe_success_percentage": probe,
		},
	}, true
}

// nestedToString reads a possibly numeric/string nested field as a string.
func nestedToString(obj map[string]any, fields ...string) string {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || err != nil || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return fmt.Sprintf("%d", t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
