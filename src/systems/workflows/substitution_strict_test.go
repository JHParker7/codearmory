package main

import (
	"strings"
	"testing"
)

// The strict path is what a step's With map goes through before it executes. Its
// whole job is to turn a reference that did not resolve into a failure that names
// the step, the reference and the reason — instead of handing the literal ${...}
// to the action, which is how the original bug produced forge's
// 'destinations[0] "…/git:${steps.commit.output.SHA}": not a valid image reference'.

func TestSubstituteWithStrict_ResolvedValuesPassThrough(t *testing.T) {
	sc := substContext{
		inputs:  map[string]string{"env": "prod"},
		outputs: map[string]string{"commit": `{"SHA":"abc123"}`},
		known:   map[string]bool{"commit": true},
	}
	out, err := substituteWithStrict(map[string]any{
		"image":  "registry/app:${steps.commit.output.SHA}",
		"target": "${inputs.env}",
	}, sc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["image"] != "registry/app:abc123" {
		t.Errorf("image = %q, want registry/app:abc123", out["image"])
	}
	if out["target"] != "prod" {
		t.Errorf("target = %q, want prod", out["target"])
	}
}

// The exact shape from the ticket: the reference sits inside a slice nested under a
// map (forge/build-image's build.destinations), so the walker must reach it.
func TestSubstituteWithStrict_FailsOnUnresolvedInsideNestedSlice(t *testing.T) {
	sc := substContext{
		stepName: "image",
		inputs:   map[string]string{"x": "1"},
		outputs:  map[string]string{},
		known:    map[string]bool{"commit": true, "image": true},
	}
	_, err := substituteWithStrict(map[string]any{
		"build": map[string]any{
			"destinations": []any{"192.168.53.171:3000/jp01/git:${steps.commit.output.SHA}"},
		},
	}, sc)
	if err == nil {
		t.Fatal("want an error — an unresolved reference must fail the step, not reach the action")
	}
	msg := err.Error()
	for _, want := range []string{"step image", "${steps.commit.output.SHA}", "not an ancestor"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// A typo and a missing route both leave the output invisible, but need different
// fixes — so they must not produce the same sentence.
func TestSubstituteWithStrict_DistinguishesTypoFromMissingRoute(t *testing.T) {
	base := substContext{stepName: "image", inputs: map[string]string{"x": "1"}, outputs: map[string]string{}}

	notAncestor := base
	notAncestor.known = map[string]bool{"commit": true}
	_, err := substituteWithStrict(map[string]any{"v": "${steps.commit.output}"}, notAncestor)
	if err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Errorf("existing-but-unreachable step: got %v, want a 'not an ancestor' reason", err)
	}

	typo := base
	typo.known = map[string]bool{"commit": true}
	_, err = substituteWithStrict(map[string]any{"v": "${steps.commmit.output}"}, typo)
	if err == nil || !strings.Contains(err.Error(), `no step named "commmit"`) {
		t.Errorf("misspelled step: got %v, want a 'no step named' reason", err)
	}
}

// Without the known set (call sites outside the step path) the weaker reason is
// used rather than wrongly claiming the step does not exist.
func TestSubstituteWithStrict_WithoutKnownSetAvoidsClaimingNoSuchStep(t *testing.T) {
	sc := substContext{stepName: "image", inputs: map[string]string{"x": "1"}, outputs: map[string]string{}}
	_, err := substituteWithStrict(map[string]any{"v": "${steps.commit.output}"}, sc)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "no step named") {
		t.Errorf("must not assert the step is absent when the name set is unknown: %q", err.Error())
	}
}

func TestSubstituteWithStrict_MissingJSONFieldNamesTheField(t *testing.T) {
	sc := substContext{
		stepName: "deploy",
		outputs:  map[string]string{"commit": `{"SHA":"abc"}`},
		known:    map[string]bool{"commit": true},
	}
	_, err := substituteWithStrict(map[string]any{"v": "${steps.commit.output.BRANCH}"}, sc)
	if err == nil || !strings.Contains(err.Error(), `no field "BRANCH"`) {
		t.Errorf("got %v, want the missing field named", err)
	}
}

func TestSubstituteWithStrict_NonJSONOutputSaysSo(t *testing.T) {
	sc := substContext{
		stepName: "deploy",
		outputs:  map[string]string{"commit": "abc123"},
		known:    map[string]bool{"commit": true},
	}
	_, err := substituteWithStrict(map[string]any{"v": "${steps.commit.output.SHA}"}, sc)
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("got %v, want the non-JSON output explained", err)
	}
}

// Strictness must not fail a step over ${...} that belongs to another syntax. These
// are the real shell expansions in infra/ci/codearmory-ci.yaml — if any of them
// tripped the check, the dogfood CI pipeline would fail on its next run.
func TestSubstituteWithStrict_ShellExpansionIsNotAWorkflowReference(t *testing.T) {
	sc := substContext{
		stepName: "discover",
		inputs:   map[string]string{"x": "1"},
		outputs:  map[string]string{},
		known:    map[string]bool{"discover": true},
	}
	script := `d=${f%/go.mod}; MODULES="${MODULES# }"; IMAGES="${IMAGES# }"; ` +
		`m=${m#systems/}; d=${d#systems/}; f=${f%/Dockerfile}; e="${EXTRAS# }"; ref=${HOOK_REF}`
	out, err := substituteWithStrict(map[string]any{"run": script}, sc)
	if err != nil {
		t.Fatalf("shell expansion must not fail the step: %v", err)
	}
	if out["run"] != script {
		t.Errorf("script was rewritten:\n got %q\nwant %q", out["run"], script)
	}
}

// The two must coexist in one value: the shell part survives, the workflow part
// still fails.
func TestSubstituteWithStrict_MixedShellAndWorkflowRefs(t *testing.T) {
	sc := substContext{
		stepName: "image",
		inputs:   map[string]string{"x": "1"},
		outputs:  map[string]string{},
		known:    map[string]bool{"commit": true},
	}
	_, err := substituteWithStrict(map[string]any{
		"run": `tag=${steps.commit.output.SHA}; dir=${f%/go.mod}`,
	}, sc)
	if err == nil {
		t.Fatal("want the workflow reference to fail the step")
	}
	if !strings.Contains(err.Error(), "${steps.commit.output.SHA}") {
		t.Errorf("message %q should name the workflow reference", err.Error())
	}
	if strings.Contains(err.Error(), "go.mod") {
		t.Errorf("message %q must not blame the shell expansion", err.Error())
	}
}

func TestSubstituteWithStrict_MissingRunInput(t *testing.T) {
	sc := substContext{stepName: "deploy", inputs: map[string]string{"env": "prod"}}
	_, err := substituteWithStrict(map[string]any{"v": "${inputs.region}"}, sc)
	if err == nil || !strings.Contains(err.Error(), `no run input named "region"`) {
		t.Errorf("got %v, want the missing input named", err)
	}
}

// A step wired to several missing outputs should be fixable in one pass, not one
// reference per run.
func TestSubstituteWithStrict_ReportsEveryBadReferenceAtOnce(t *testing.T) {
	sc := substContext{
		stepName: "deploy",
		inputs:   map[string]string{"x": "1"},
		outputs:  map[string]string{},
		known:    map[string]bool{"build": true, "test": true},
	}
	_, err := substituteWithStrict(map[string]any{
		"a": "${steps.build.output}",
		"b": "${steps.test.output}",
	}, sc)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "${steps.build.output}") || !strings.Contains(msg, "${steps.test.output}") {
		t.Errorf("message %q should name both unresolved references", msg)
	}
	if !strings.Contains(msg, "2 references") {
		t.Errorf("message %q should count the unresolved references", msg)
	}
}

// The opt-out path: a step that legitimately emits a literal ${...} keeps the old
// lenient behaviour. executeStep selects this via Step.AllowUnresolved.
func TestSubstituteWith_LenientPathStillPassesLiteralThrough(t *testing.T) {
	sc := substContext{inputs: map[string]string{"x": "1"}, outputs: map[string]string{}}
	out := substituteWith(map[string]any{"script": "echo ${steps.later.output}"}, sc)
	if out["script"] != "echo ${steps.later.output}" {
		t.Errorf("got %q, want the literal preserved for an opted-out step", out["script"])
	}
}

// An empty context means "nothing to interpolate against" (e.g. a step run outside
// a graph), so the map is handed back untouched rather than failing on every ref.
func TestSubstituteWithStrict_EmptyContextReturnsInputUnchanged(t *testing.T) {
	with := map[string]any{"v": "${steps.build.output}"}
	out, err := substituteWithStrict(with, substContext{})
	if err != nil {
		t.Fatalf("empty context must not fail: %v", err)
	}
	if out["v"] != "${steps.build.output}" {
		t.Errorf("got %q, want the map returned unchanged", out["v"])
	}
}

func TestSubstituteWithStrict_MatrixAndMapReferences(t *testing.T) {
	sc := substContext{stepName: "build", inputs: map[string]string{"x": "1"}}
	_, err := substituteWithStrict(map[string]any{"v": "${matrix.region}"}, sc)
	if err == nil || !strings.Contains(err.Error(), "no matrix variable") {
		t.Errorf("matrix: got %v, want an unbound-matrix-variable reason", err)
	}
	_, err = substituteWithStrict(map[string]any{"v": "${map.svc}"}, sc)
	if err == nil || !strings.Contains(err.Error(), "no map variable") {
		t.Errorf("map: got %v, want an unbound-map-variable reason", err)
	}
}
