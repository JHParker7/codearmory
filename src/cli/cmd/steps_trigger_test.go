package cmd

import (
	"encoding/json"
	"reflect"
	"testing"
)

// workflows/trigger runs another pipeline as a sub-run: `pipeline` names the target
// and `inputs` is a KEY=VALUE map passed to it. Both land flat on the with map.
func TestBuildStepWith_WorkflowsTrigger(t *testing.T) {
	with, err := buildStepWith("workflows/trigger", reader(map[string]string{
		withKeyPrefix + "pipeline": "deploy",
		withKeyPrefix + "inputs":   "ENV=staging REGION=us-east-1",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	if with["pipeline"] != "deploy" {
		t.Errorf("pipeline = %v, want deploy", with["pipeline"])
	}
	if !reflect.DeepEqual(with["inputs"], map[string]string{"ENV": "staging", "REGION": "us-east-1"}) {
		t.Errorf("inputs = %#v", with["inputs"])
	}
}

// The target pipeline is required; inputs are optional.
func TestBuildStepWith_WorkflowsTrigger_RequiresPipeline(t *testing.T) {
	if _, err := buildStepWith("workflows/trigger", reader(map[string]string{
		withKeyPrefix + "inputs": "ENV=staging",
	})); err == nil {
		t.Errorf("expected an error when pipeline is missing")
	}
	if _, err := buildStepWith("workflows/trigger", reader(map[string]string{
		withKeyPrefix + "pipeline": "deploy",
	})); err != nil {
		t.Errorf("pipeline alone should be sufficient, got %v", err)
	}
}

// A stored workflows/trigger step round-trips back to the flat form fields (pipeline
// as-is, inputs rendered to KEY=VALUE tokens) so the edit form pre-populates.
func TestWorkflowsTrigger_EditRoundTrip(t *testing.T) {
	with, err := buildStepWith("workflows/trigger", reader(map[string]string{
		withKeyPrefix + "pipeline": "deploy",
		withKeyPrefix + "inputs":   "ENV=prod",
	}))
	if err != nil {
		t.Fatalf("buildStepWith: %v", err)
	}
	// Simulate the API round-trip: a stored step comes back JSON-decoded, so its env
	// map is map[string]any (the shape formatEnvTokens reads) rather than the
	// map[string]string buildStepWith emits.
	raw, _ := json.Marshal(with)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("re-decode with: %v", err)
	}
	vals := stepEditValues(tuiStep{Action: "workflows/trigger", With: decoded})
	if vals[withKeyPrefix+"pipeline"] != "deploy" {
		t.Errorf("pipeline field = %q, want deploy", vals[withKeyPrefix+"pipeline"])
	}
	if vals[withKeyPrefix+"inputs"] != "ENV=prod" {
		t.Errorf("inputs field = %q, want ENV=prod", vals[withKeyPrefix+"inputs"])
	}
}
