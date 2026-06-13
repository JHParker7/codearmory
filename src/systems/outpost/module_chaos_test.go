package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBuildChaosEngineFieldTypes(t *testing.T) {
	m := &chaosModule{serviceAcc: "litmus-admin"}
	u := m.buildChaosEngine("exp-abc", "codearmory", "app.kubernetes.io/component=conductor", "deployment",
		"pod-delete", "abc", map[string]string{"TOTAL_CHAOS_DURATION": "30"})

	if u.GetAPIVersion() != "litmuschaos.io/v1alpha1" || u.GetKind() != "ChaosEngine" {
		t.Fatalf("wrong GVK: %s/%s", u.GetAPIVersion(), u.GetKind())
	}
	// monitoring MUST be a bool, not a string — the operator is type-strict.
	mon, found, err := unstructured.NestedBool(u.Object, "spec", "monitoring")
	if err != nil || !found || mon != true {
		t.Errorf("spec.monitoring not a true bool: found=%v err=%v val=%v", found, err, mon)
	}
	// applabel MUST be a single key=value string.
	label, _, _ := unstructured.NestedString(u.Object, "spec", "appinfo", "applabel")
	if label != "app.kubernetes.io/component=conductor" {
		t.Errorf("applabel = %q", label)
	}
	// env values MUST be strings.
	env, found, err := unstructured.NestedSlice(u.Object, "spec", "experiments")
	if err != nil || !found || len(env) != 1 {
		t.Fatalf("experiments malformed: %v", err)
	}
	exp0 := env[0].(map[string]any)
	if exp0["name"] != "pod-delete" {
		t.Errorf("experiment name = %v", exp0["name"])
	}
	envList := exp0["spec"].(map[string]any)["components"].(map[string]any)["env"].([]any)
	if len(envList) != 1 {
		t.Fatalf("env len = %d", len(envList))
	}
	kv := envList[0].(map[string]any)
	if _, ok := kv["value"].(string); !ok {
		t.Errorf("env value is not a string: %T", kv["value"])
	}
}

func TestResultToEvent(t *testing.T) {
	m := &chaosModule{}
	mk := func(engine, verdict, phase string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": engine + "-pod-delete"},
			"spec":     map[string]any{"engine": engine},
			"status": map[string]any{"experimentStatus": map[string]any{
				"verdict": verdict, "phase": phase, "failStep": "N/A",
				"probeSuccessPercentage": "100",
			}},
		}}
	}

	// Decisive Pass on our engine -> verdict event with recovered experiment_id.
	ev, ok := m.resultToEvent(mk("exp-xyz", "Pass", "Completed"))
	if !ok {
		t.Fatal("expected an event for a decisive verdict")
	}
	if ev.Type != "verdict" || ev.Payload["experiment_id"] != "xyz" || ev.Payload["verdict"] != "Pass" {
		t.Errorf("unexpected event payload: %+v", ev.Payload)
	}
	if ev.Payload["probe_success_percentage"] != "100" {
		t.Errorf("probe percentage = %v", ev.Payload["probe_success_percentage"])
	}

	// Awaited verdict while still running -> no event (avoid flooding).
	if _, ok := m.resultToEvent(mk("exp-xyz", "Awaited", "Running")); ok {
		t.Error("did not expect an event for an Awaited/Running result")
	}

	// Foreign engine (not framework-created) -> ignored.
	if _, ok := m.resultToEvent(mk("some-other-engine", "Pass", "Completed")); ok {
		t.Error("did not expect an event for a foreign engine")
	}
}

func TestResultToEvent_EngineFallback(t *testing.T) {
	m := &chaosModule{}
	// No spec.engine: the engine (and thus experiment_id) must be recovered from
	// the result name by stripping the experiment suffix via spec.experiment, NOT
	// the last hyphen — experiment ids are UUIDs full of hyphens.
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "exp-abc-123-def-pod-delete"},
		"spec":     map[string]any{"experiment": "pod-delete"},
		"status": map[string]any{"experimentStatus": map[string]any{
			"verdict": "Pass", "phase": "Completed",
		}},
	}}
	ev, ok := m.resultToEvent(u)
	if !ok {
		t.Fatal("expected an event")
	}
	if ev.Payload["experiment_id"] != "abc-123-def" {
		t.Errorf("experiment_id = %q, want abc-123-def", ev.Payload["experiment_id"])
	}
}
