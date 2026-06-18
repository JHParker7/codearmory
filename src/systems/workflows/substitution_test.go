package main

import "testing"

// ── step output references ──────────────────────────────────────────────────────

func TestSubstitute_StepOutputWhole(t *testing.T) {
	sc := substContext{outputs: map[string]string{"build": "v1.2.3"}}
	if got := substitute("tag-${steps.build.output}", sc); got != "tag-v1.2.3" {
		t.Errorf("got %q, want tag-v1.2.3", got)
	}
}

func TestSubstitute_StepOutputJSONField(t *testing.T) {
	sc := substContext{outputs: map[string]string{
		"create_ticket": `{"ticket_id":"T-42","title":"hi"}`,
	}}
	if got := substitute("${steps.create_ticket.output.ticket_id}", sc); got != "T-42" {
		t.Errorf("got %q, want T-42", got)
	}
}

func TestSubstitute_StepOutputNestedJSONField(t *testing.T) {
	sc := substContext{outputs: map[string]string{
		"run": `{"result":{"meta":{"sha":"abc123"}}}`,
	}}
	if got := substitute("${steps.run.output.result.meta.sha}", sc); got != "abc123" {
		t.Errorf("got %q, want abc123", got)
	}
}

func TestSubstitute_StepOutputNumberField(t *testing.T) {
	sc := substContext{outputs: map[string]string{"run": `{"count":7}`}}
	if got := substitute("${steps.run.output.count}", sc); got != "7" {
		t.Errorf("got %q, want 7 (json number stringified)", got)
	}
}

func TestSubstitute_UnknownStepLeftAsIs(t *testing.T) {
	sc := substContext{outputs: map[string]string{"build": "x"}}
	in := "${steps.missing.output}"
	if got := substitute(in, sc); got != in {
		t.Errorf("got %q, want the reference left untouched", got)
	}
}

func TestSubstitute_MissingJSONFieldLeftAsIs(t *testing.T) {
	sc := substContext{outputs: map[string]string{"build": `{"a":"b"}`}}
	in := "${steps.build.output.nope}"
	if got := substitute(in, sc); got != in {
		t.Errorf("got %q, want the reference left untouched on missing field", got)
	}
}

func TestSubstitute_FieldOnNonJSONOutputLeftAsIs(t *testing.T) {
	sc := substContext{outputs: map[string]string{"build": "not json"}}
	in := "${steps.build.output.field}"
	if got := substitute(in, sc); got != in {
		t.Errorf("got %q, want the reference left untouched when output isn't JSON", got)
	}
}

func TestSubstitute_InputsDotForm(t *testing.T) {
	sc := substContext{inputs: map[string]string{"ENV": "prod"}}
	if got := substitute("${inputs.ENV}", sc); got != "prod" {
		t.Errorf("got %q, want prod", got)
	}
}

func TestSubstitute_BareInputFormStillWorks(t *testing.T) {
	sc := substContext{inputs: map[string]string{"ENV": "prod"}}
	if got := substitute("${ENV}", sc); got != "prod" {
		t.Errorf("got %q, want prod (bare form backward compatible)", got)
	}
}

func TestSubstitute_InputsAndOutputsTogether(t *testing.T) {
	sc := substContext{
		inputs:  map[string]string{"REPO": "acme/app"},
		outputs: map[string]string{"build": `{"sha":"deadbeef"}`},
	}
	got := substitute("${inputs.REPO}@${steps.build.output.sha}", sc)
	if got != "acme/app@deadbeef" {
		t.Errorf("got %q, want acme/app@deadbeef", got)
	}
}

// ── substituteWith threading into nested With values ────────────────────────────

func TestSubstituteWith_ResolvesEnvFromStepOutput(t *testing.T) {
	with := map[string]any{
		"image": "ubuntu:22.04",
		"env": map[string]any{
			"BUILD_SHA": "${steps.build.output.sha}",
		},
	}
	sc := substContext{outputs: map[string]string{"build": `{"sha":"abc"}`}}
	out := substituteWith(with, sc)
	env, _ := out["env"].(map[string]any)
	if env["BUILD_SHA"] != "abc" {
		t.Errorf("env.BUILD_SHA = %v, want abc", env["BUILD_SHA"])
	}
}

func TestSubstituteWith_EmptyContextReturnsInput(t *testing.T) {
	with := map[string]any{"title": "${steps.x.output}"}
	out := substituteWith(with, substContext{})
	if out["title"] != "${steps.x.output}" {
		t.Errorf("title = %v, want untouched with an empty context", out["title"])
	}
}
