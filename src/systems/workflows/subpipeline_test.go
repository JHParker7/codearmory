package main

import "testing"

// applyDeclaredInputs applies declared defaults, keeps provided/undeclared values,
// and enforces required inputs.
func TestApplyDeclaredInputs(t *testing.T) {
	defs := []WorkflowInputDef{
		{Name: "region", Default: "us-east"},
		{Name: "tag", Required: true},
		{Name: "env", Default: "dev", Required: true}, // required but satisfied by default
	}

	t.Run("defaults and provided", func(t *testing.T) {
		got, msg := applyDeclaredInputs(defs, map[string]string{"tag": "v1", "extra": "kept"})
		if msg != "" {
			t.Fatalf("unexpected error: %s", msg)
		}
		want := map[string]string{"region": "us-east", "tag": "v1", "env": "dev", "extra": "kept"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("input %s = %q, want %q", k, got[k], v)
			}
		}
	})

	t.Run("provided overrides default", func(t *testing.T) {
		got, _ := applyDeclaredInputs(defs, map[string]string{"region": "eu-west", "tag": "v2"})
		if got["region"] != "eu-west" {
			t.Errorf("region = %q, want eu-west (provided wins over default)", got["region"])
		}
	})

	t.Run("missing required fails", func(t *testing.T) {
		_, msg := applyDeclaredInputs(defs, map[string]string{"region": "eu-west"})
		if msg == "" {
			t.Fatal("expected an error for missing required input 'tag'")
		}
	})

	t.Run("no declarations keeps free-form inputs", func(t *testing.T) {
		got, msg := applyDeclaredInputs(nil, map[string]string{"anything": "goes"})
		if msg != "" || got["anything"] != "goes" {
			t.Fatalf("free-form input not preserved: got=%v msg=%q", got, msg)
		}
	})
}

// resolveWorkflowOutputs resolves each declared output template against the run's
// step outputs at completion.
func TestResolveWorkflowOutputs(t *testing.T) {
	sc := substContext{
		inputs:  map[string]string{"who": "world"},
		outputs: map[string]string{"build": `{"IMAGE":"acme/app:1.0"}`},
	}
	defs := []WorkflowOutputDef{
		{Name: "image", Value: "${steps.build.output.IMAGE}"},
		{Name: "greeting", Value: "hi ${inputs.who}"},
		{Name: "", Value: "ignored — no name"},
	}
	got := resolveWorkflowOutputs(defs, sc)
	if got["image"] != "acme/app:1.0" {
		t.Errorf("image = %q, want acme/app:1.0", got["image"])
	}
	if got["greeting"] != "hi world" {
		t.Errorf("greeting = %q, want 'hi world'", got["greeting"])
	}
	if _, ok := got[""]; ok {
		t.Error("an unnamed output should be dropped")
	}
	if resolveWorkflowOutputs(nil, sc) != nil {
		t.Error("no declared outputs should resolve to nil")
	}
}

func TestValidateWorkflowIO(t *testing.T) {
	cases := []struct {
		name    string
		inputs  []WorkflowInputDef
		outputs []WorkflowOutputDef
		wantErr bool
	}{
		{"valid", []WorkflowInputDef{{Name: "a"}}, []WorkflowOutputDef{{Name: "o", Value: "${steps.x.output}"}}, false},
		{"empty is valid", nil, nil, false},
		{"dup input", []WorkflowInputDef{{Name: "a"}, {Name: "a"}}, nil, true},
		{"unnamed input", []WorkflowInputDef{{Name: " "}}, nil, true},
		{"dup output", nil, []WorkflowOutputDef{{Name: "o", Value: "x"}, {Name: "o", Value: "y"}}, true},
		{"output without value", nil, []WorkflowOutputDef{{Name: "o"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateWorkflowIO(tc.inputs, tc.outputs)
			if (msg != "") != tc.wantErr {
				t.Errorf("validateWorkflowIO msg=%q, wantErr=%v", msg, tc.wantErr)
			}
		})
	}
}

// A workflows/trigger step's run role must be able to both create the sub-run
// (triggerRun) and poll it to completion (getRun) on the item-scoped run space —
// otherwise the trigger step 403-hangs polling the sub-run.
func TestCollectWorkflowPermissions_TriggerGrantsTriggerAndPoll(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		ActionWorkflowsTrigger: {
			Name:               ActionWorkflowsTrigger,
			RequiredPermission: &PermissionSpec{Service: "workflows", Action: "triggerRun", Resource: "workflows/runs"},
			Async:              &AsyncConfig{PollPath: "/runs/{id}"},
		},
	})
	perms := collectWorkflowPermissions([]WorkflowStep{{Step: Step{Action: ActionWorkflowsTrigger}}}, nil)
	var hasTrigger, hasPoll bool
	for _, p := range perms {
		if p.Service == "workflows" && p.Action == "triggerRun" && p.Resource == "workflows/runs/*" {
			hasTrigger = true
		}
		if p.Service == "workflows" && p.Action == "getRun" && p.Resource == "workflows/runs/*" {
			hasPoll = true
		}
	}
	if !hasTrigger || !hasPoll {
		t.Fatalf("perms = %+v, want triggerRun + getRun on workflows/runs/*", perms)
	}
}

// An inline step contributes its action's permission to the run role just like a
// stored-step reference — the enriched WorkflowStep carries the same Action.
func TestCollectWorkflowPermissions_InlineStepContributesPerms(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run": {
			Name:               "forge/run",
			RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"},
		},
	})
	// An inline step is an enriched WorkflowStep with Action set and no StepID — the
	// same shape collectWorkflowPermissions sees for any step.
	perms := collectWorkflowPermissions([]WorkflowStep{{Step: Step{Name: "build", Action: "forge/run"}}}, nil)
	var has bool
	for _, p := range perms {
		if p.Service == "forge" && p.Action == "createExecution" && p.Resource == "forge/executions" {
			has = true
		}
	}
	if !has {
		t.Fatalf("perms = %+v, want forge:createExecution on an inline forge/run step", perms)
	}
}
