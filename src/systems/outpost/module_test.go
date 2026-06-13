package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPayloadString(t *testing.T) {
	p := map[string]any{"a": "x", "n": 1}
	if got := payloadString(p, "a"); got != "x" {
		t.Errorf("payloadString a = %q", got)
	}
	if got := payloadString(p, "n"); got != "" {
		t.Errorf("non-string should yield empty, got %q", got)
	}
	if got := payloadString(nil, "a"); got != "" {
		t.Errorf("nil payload should yield empty, got %q", got)
	}
}

func TestPayloadStringMap(t *testing.T) {
	p := map[string]any{"params": map[string]any{"K": "V", "n": 3}}
	m := payloadStringMap(p, "params")
	if m["K"] != "V" {
		t.Errorf("expected K=V, got %v", m)
	}
	if _, ok := m["n"]; ok {
		t.Errorf("non-string value should be dropped: %v", m)
	}
	if got := payloadStringMap(p, "missing"); len(got) != 0 {
		t.Errorf("missing key should yield empty map, got %v", got)
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" chaos , argo ,, ")
	if len(got) != 2 || got[0] != "chaos" || got[1] != "argo" {
		t.Errorf("splitCSV = %v, want [chaos argo]", got)
	}
	if len(splitCSV("")) != 0 {
		t.Error("empty string should yield no entries")
	}
}

func TestArgoAppToEvent(t *testing.T) {
	m := &argoModule{}
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "guestbook"},
		"status": map[string]any{
			"sync":           map[string]any{"status": "Synced", "revision": "abc123"},
			"health":         map[string]any{"status": "Healthy"},
			"operationState": map[string]any{"phase": "Succeeded"},
		},
	}}
	ev := m.appToEvent(u)
	if ev.Integration != "argo" || ev.Type != "app-state" {
		t.Fatalf("wrong envelope: %+v", ev)
	}
	if ev.Payload["app_name"] != "guestbook" || ev.Payload["sync_status"] != "Synced" ||
		ev.Payload["health_status"] != "Healthy" || ev.Payload["operation_phase"] != "Succeeded" ||
		ev.Payload["revision"] != "abc123" {
		t.Errorf("unexpected payload: %+v", ev.Payload)
	}
}

func TestCommandErrorPayloadCarriesExperimentID(t *testing.T) {
	c := Command{Type: "run-experiment", Payload: map[string]any{"experiment_id": "exp-9"}}
	p := commandErrorPayload(c, errSample)
	if p["experiment_id"] != "exp-9" || p["command_type"] != "run-experiment" || p["error"] == nil {
		t.Errorf("unexpected error payload: %+v", p)
	}
}

var errSample = sampleErr("boom")

type sampleErr string

func (e sampleErr) Error() string { return string(e) }
