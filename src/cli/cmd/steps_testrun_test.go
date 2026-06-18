package cmd

import (
	"reflect"
	"testing"
)

// ── stepWithRefs ────────────────────────────────────────────────────────────────

func TestStepWithRefs_FindsUniqueRefsAcrossNesting(t *testing.T) {
	with := map[string]any{
		"title": "Build ${steps.build.output} for ${inputs.ENV}",
		"env": map[string]any{
			"TAG": "${inputs.ENV}", // duplicate ref, must dedupe
			"SHA": "${steps.build.output.sha}",
		},
		"args": []any{"--ref", "${BARE}"},
	}
	got := stepWithRefs(with)
	want := []string{"BARE", "inputs.ENV", "steps.build.output", "steps.build.output.sha"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refs = %v, want %v", got, want)
	}
}

func TestStepWithRefs_NoneWhenNoReferences(t *testing.T) {
	with := map[string]any{"image": "ubuntu:22.04", "run": "go test ./..."}
	if got := stepWithRefs(with); len(got) != 0 {
		t.Errorf("refs = %v, want empty", got)
	}
}

// ── resolveStepWith ─────────────────────────────────────────────────────────────

func TestResolveStepWith_BakesValuesIncludingNested(t *testing.T) {
	with := map[string]any{
		"title": "Build ${steps.build.output}",
		"env":   map[string]any{"SHA": "${inputs.SHA}"},
		"args":  []any{"--tag", "${TAG}"},
	}
	vals := map[string]string{
		"steps.build.output": "OK (42 tests)",
		"inputs.SHA":         "abc123",
		"TAG":                "v1",
	}
	out := resolveStepWith(with, vals)
	if out["title"] != "Build OK (42 tests)" {
		t.Errorf("title = %v, want baked-in output", out["title"])
	}
	env, _ := out["env"].(map[string]any)
	if env["SHA"] != "abc123" {
		t.Errorf("env.SHA = %v, want abc123", env["SHA"])
	}
	args, _ := out["args"].([]any)
	if len(args) != 2 || args[1] != "v1" {
		t.Errorf("args = %v, want [--tag v1]", out["args"])
	}
}

func TestResolveStepWith_UnsuppliedRefLeftAsIs(t *testing.T) {
	with := map[string]any{"title": "${inputs.MISSING}"}
	out := resolveStepWith(with, map[string]string{})
	if out["title"] != "${inputs.MISSING}" {
		t.Errorf("title = %v, want the reference untouched when no value supplied", out["title"])
	}
}

func TestResolveStepWith_DoesNotMutateInput(t *testing.T) {
	with := map[string]any{"title": "${X}"}
	_ = resolveStepWith(with, map[string]string{"X": "y"})
	if with["title"] != "${X}" {
		t.Errorf("source with was mutated: %v", with["title"])
	}
}

// ── sanitizeName ────────────────────────────────────────────────────────────────

func TestSanitizeName_KeepsSafeCharsAndCaps(t *testing.T) {
	if got := sanitizeName("deploy prod/api!"); got != "deploy-prod-api-" {
		t.Errorf("sanitizeName = %q, want deploy-prod-api-", got)
	}
	long := sanitizeName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") // 34 a's
	if len(long) != 24 {
		t.Errorf("sanitizeName length = %d, want capped at 24", len(long))
	}
}
