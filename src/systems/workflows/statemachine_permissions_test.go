package main

import "testing"

// A pipeline's state-machine view must round-trip a step's declared permissions.
//
// This is not a cosmetic gap. Every GET of a pipeline returns a COMPUTED state_machine,
// and handleUpdateWorkflow expands that document over the request. So a client that read
// a pipeline and wrote it back had its steps replaced by ones the document could
// describe — and the document could not describe `permissions`. The run role was then
// re-minted without them, and the next run failed with a 403 naming a permission the
// pipeline still visibly declared. Observed on git_factory-cd's retarget step.
func TestStateMachineRoundTripsStepPermissions(t *testing.T) {
	perms := []PermissionSpec{{Service: "builder", Action: "setOrgServiceImage", Resource: "builder/orgs/default"}}
	steps := []WorkflowStep{{
		Step:        Step{Name: "retarget", Action: ActionHTTP, Permissions: perms},
		}}

	doc := modelToSM("p", "", steps, nil, nil, nil, nil)
	st, ok := doc.States["retarget"]
	if !ok {
		t.Fatalf("state %q missing from the document; states: %v", "retarget", keysOf(doc.States))
	}
	if len(st.Permissions) != 1 {
		t.Fatalf("model -> state machine dropped permissions: got %v", st.Permissions)
	}

	// ...and back, which is the direction handleUpdateWorkflow actually takes.
	back, _, _, err := smToModel(doc)
	if err != nil {
		t.Fatalf("smToModel: %v", err)
	}
	var found bool
	for _, ref := range back {
		if ref.Name != "retarget" {
			continue
		}
		found = true
		if len(ref.Permissions) != 1 {
			t.Fatalf("state machine -> model dropped permissions: got %v — the run role would be re-minted without them", ref.Permissions)
		}
		got := ref.Permissions[0]
		if got.Service != "builder" || got.Action != "setOrgServiceImage" || got.Resource != "builder/orgs/default" {
			t.Errorf("permission mangled in round-trip: %+v", got)
		}
	}
	if !found {
		t.Fatal("retarget step missing after round-trip")
	}
}

// An explicit `steps` array must win over a state_machine sent in the same request.
// The doc comment on createWorkflowRequest.StateMachine has always claimed this
// ("Explicit top-level fields still win over the document's"), but it held only for
// name/description/inputs/outputs — steps were overwritten, which is exactly how a
// read-edit-write round-trip lost its edits and returned 200 anyway.
func TestExplicitStepsBeatStateMachine(t *testing.T) {
	doc := &smDoc{Name: "p", States: map[string]*smState{
		"fromdoc": {Run: "forge/run", End: true},
	}}
	req := &createWorkflowRequest{
		Name:         "p",
		StateMachine: doc,
		Steps:        []WorkflowStepRef{{Name: "explicit", Action: "forge/run"}},
	}
	if msg := applyStateMachine(req); msg != "" {
		t.Fatalf("applyStateMachine: %s", msg)
	}
	if len(req.Steps) != 1 || req.Steps[0].Name != "explicit" {
		t.Fatalf("explicit steps were overwritten by the document: %+v", req.Steps)
	}

	// With no explicit steps the document is still the source of truth, so authoring
	// purely as a state machine keeps working.
	req2 := &createWorkflowRequest{Name: "p", StateMachine: doc}
	if msg := applyStateMachine(req2); msg != "" {
		t.Fatalf("applyStateMachine: %s", msg)
	}
	if len(req2.Steps) != 1 || req2.Steps[0].Name != "fromdoc" {
		t.Fatalf("document was not expanded when steps were absent: %+v", req2.Steps)
	}
}

func keysOf(m map[string]*smState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
