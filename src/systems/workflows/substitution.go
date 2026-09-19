package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Step inputs/outputs templating.
//
// String values inside a step's With map may interpolate ${...} references that
// are resolved at execution time against the run's inputs and the outputs of
// already-completed steps. Supported forms:
//
//	${inputs.NAME}   or  ${NAME}     run-level input NAME (bare form kept for compat)
//	${steps.STEP.output}            the full string output of an earlier step
//	${steps.STEP.output.a.b}        field a.b of an earlier step's JSON output
//
// Only outputs of steps that completed in a *prior* group are visible, so a step
// can never reference its own output or a sibling running in the same parallel
// group.
//
// An unresolved reference in a STEP's With map fails that step (see
// substituteWithStrict): the literal ${...} would otherwise be handed to whatever
// service the step calls, which either rejects it with a message naming neither the
// step nor the reference, or — worse, for a service that is not strict — accepts it
// and produces something quietly wrong.
//
// Only references in a recognised namespace (${steps.*}, ${inputs.*}, ${matrix.*},
// ${map.*}, ${scatter.*}, ${run_id}, ${workflow_id}) can fail a step. A run
// script's shell expansion — ${f%/go.mod}, ${MODULES# }, a bare ${HOOK_REF} — is
// not addressed to this engine and passes through as before. A step that must emit
// a literal reference in one of OUR namespaces opts out with allow_unresolved.
//
// Everywhere else (approval messages, ticket titles, matrix value lists) an
// unresolved reference is still left untouched — those are display or list-shaped
// values where a stray literal is visible rather than silently consequential.

// substContext carries everything a step's With values can interpolate.
type substContext struct {
	inputs  map[string]string // run-level inputs, by name
	outputs map[string]string // earlier step outputs, by step name
	matrix  map[string]string // matrix bindings for this execution, by var name
	// mapVars are the map-region bindings for this iteration, by var name, exposed as
	// ${map.<var>}. Kept separate from matrix: a step inside a map region can still
	// have its own matrix, so the two namespaces must not collide.
	mapVars map[string]string
	// scatterPath is the workspace path a scatter leg is bound to, exposed as
	// ${scatter.path} so the leg's command targets its own partition.
	scatterPath string
	runID       string // this run's id, exposed as ${run_id} / ${run.id}
	// workflowID is the PIPELINE's id, exposed as ${workflow_id}. Distinct from runID:
	// it identifies the definition across every run of it, which is what links a
	// created ticket back to the pipeline rather than to one execution of it.
	workflowID string
	// depth is the run's sub-pipeline nesting depth, propagated to a workflows/trigger
	// step so the created sub-run is one level deeper (not itself a substitution
	// value, so it is excluded from empty()).
	depth int
	// stepName names the step being substituted, so a failure can say WHICH step's
	// reference did not resolve. Diagnostic only — excluded from empty().
	stepName string
	// known is every step name in the pipeline, used only to tell "no such step" apart
	// from "that step exists but is not an ancestor of this one" — the two have
	// completely different fixes (typo vs missing route). Nil outside the step
	// execution path, where the weaker reason is used instead. Excluded from empty().
	known map[string]bool
}

func (sc substContext) empty() bool {
	return len(sc.inputs) == 0 && len(sc.outputs) == 0 && len(sc.matrix) == 0 &&
		len(sc.mapVars) == 0 && sc.scatterPath == "" && sc.runID == ""
}

var refPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// substitute resolves every ${...} reference in s, leaving unresolved ones as-is.
func substitute(s string, sc substContext) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return refPattern.ReplaceAllStringFunc(s, func(tok string) string {
		expr := strings.TrimSpace(tok[2 : len(tok)-1])
		if v, ok := sc.resolve(expr); ok {
			return v
		}
		return tok
	})
}

// unresolvedRef is one ${...} reference that did not resolve, with the reason.
type unresolvedRef struct {
	token  string // the literal ${...} as written
	reason string
}

// unresolvedError fails a step whose With map contains references that did not
// resolve. It names the step and every offending reference, because the message a
// downstream service produces ("not a valid image reference") names neither.
type unresolvedError struct {
	step string
	refs []unresolvedRef
}

func (e *unresolvedError) Error() string {
	where := "step"
	if e.step != "" {
		where = "step " + e.step
	}
	if len(e.refs) == 1 {
		return fmt.Sprintf("%s: %s did not resolve; %s", where, e.refs[0].token, e.refs[0].reason)
	}
	parts := make([]string, 0, len(e.refs))
	for _, r := range e.refs {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.token, r.reason))
	}
	return fmt.Sprintf("%s: %d references did not resolve — %s", where, len(e.refs), strings.Join(parts, "; "))
}

// workflowRefPrefixes are the namespaces that make a ${...} unambiguously a
// workflow reference rather than some other system's syntax.
var workflowRefPrefixes = []string{"steps.", "inputs.", "matrix.", "map.", "loop.", "scatter."}

// looksLikeWorkflowRef reports whether expr is addressed to THIS engine, and is
// therefore something we may fail a step over.
//
// A step's With map is full of ${...} that belongs to other languages — a run
// script's shell parameter expansion (${f%/go.mod}, ${MODULES# }, ${HOOK_REF})
// being the common case, and the reason strictness cannot simply apply to every
// unresolved reference: that would fail nearly every scripted step in every
// pipeline. Only a recognised namespace counts. A BARE ${NAME} deliberately does
// not: it is indistinguishable from an ordinary shell variable, so it keeps the
// lenient behaviour even though the engine would have resolved it as a run input.
func looksLikeWorkflowRef(expr string) bool {
	for _, p := range workflowRefPrefixes {
		if strings.HasPrefix(expr, p) {
			return true
		}
	}
	return expr == "run_id" || expr == "run.id" || expr == "workflow_id" || expr == "workflow.id"
}

// substituteStrict is substitute, but it reports every reference addressed to this
// engine that did not resolve, instead of leaving the literal in place. References
// belonging to another syntax pass through untouched — see looksLikeWorkflowRef.
func substituteStrict(s string, sc substContext) (string, []unresolvedRef) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var bad []unresolvedRef
	out := refPattern.ReplaceAllStringFunc(s, func(tok string) string {
		expr := strings.TrimSpace(tok[2 : len(tok)-1])
		if v, ok := sc.resolve(expr); ok {
			return v
		}
		if looksLikeWorkflowRef(expr) {
			bad = append(bad, unresolvedRef{token: tok, reason: sc.explain(expr)})
		}
		return tok
	})
	return out, bad
}

// explain says why expr did not resolve, in the terms the pipeline author needs to
// fix it. The visibility rule already knows the answer — this just puts it in words.
func (sc substContext) explain(expr string) string {
	if rest, ok := strings.CutPrefix(expr, "steps."); ok {
		idx := strings.Index(rest, ".output")
		if idx < 0 {
			return fmt.Sprintf("%q is not a step-output reference — expected ${steps.NAME.output} or ${steps.NAME.output.FIELD}", expr)
		}
		name := rest[:idx]
		out, produced := sc.outputs[name]
		if !produced {
			switch {
			case sc.known == nil:
				return fmt.Sprintf("%s has not produced an output visible to this step", name)
			case !sc.known[name]:
				return fmt.Sprintf("no step named %q in this pipeline", name)
			default:
				return fmt.Sprintf("%s is not an ancestor of this step, so its output is not visible here — route this step after %s", name, name)
			}
		}
		field := strings.TrimPrefix(rest[idx+len(".output"):], ".")
		if !json.Valid([]byte(out)) {
			return fmt.Sprintf("%s's output is not JSON, so it has no field %q", name, field)
		}
		return fmt.Sprintf("%s's output has no field %q", name, field)
	}
	if key, ok := strings.CutPrefix(expr, "inputs."); ok {
		return fmt.Sprintf("no run input named %q — declare it in the pipeline's inputs or pass it at trigger time", key)
	}
	if key, ok := strings.CutPrefix(expr, "matrix."); ok {
		return fmt.Sprintf("no matrix variable %q bound here — this step declares no matrix, or its var has another name", key)
	}
	if key, ok := strings.CutPrefix(expr, "map."); ok {
		return fmt.Sprintf("no map variable %q bound here — this step is not inside a map region, or its var has another name", key)
	}
	if key, ok := strings.CutPrefix(expr, "loop."); ok {
		return fmt.Sprintf("no loop variable %q bound here — this step is not inside a loop, or the loop declares no var", key)
	}
	if key, ok := strings.CutPrefix(expr, "scatter."); ok {
		if key == "path" {
			return "this step is not a scatter leg, so it has no ${scatter.path}"
		}
		return fmt.Sprintf("%q is not a scatter reference — only ${scatter.path} exists", expr)
	}
	if expr == "run_id" || expr == "run.id" || expr == "workflow_id" || expr == "workflow.id" {
		return fmt.Sprintf("%s is not set in this context", expr)
	}
	return fmt.Sprintf("no run input named %q (bare ${NAME} resolves a run input); use ${steps.NAME.output} to reference a step", expr)
}

// resolve looks up a single reference expression (the text between ${ and }).
func (sc substContext) resolve(expr string) (string, bool) {
	if rest, ok := strings.CutPrefix(expr, "steps."); ok {
		// rest is NAME.output[.field...]; split on the first ".output".
		idx := strings.Index(rest, ".output")
		if idx < 0 {
			return "", false
		}
		name := rest[:idx]
		out, ok := sc.outputs[name]
		if !ok {
			return "", false
		}
		after := rest[idx+len(".output"):]
		if after == "" {
			return out, true // whole output
		}
		if !strings.HasPrefix(after, ".") {
			return "", false // e.g. ".outputs" — not a field accessor
		}
		return jsonField(out, strings.Split(after[1:], "."))
	}
	if key, ok := strings.CutPrefix(expr, "inputs."); ok {
		v, ok := sc.inputs[key]
		return v, ok
	}
	// ${matrix.<var>} resolves to this execution's matrix binding; ${matrix.value}
	// is a generic alias for the bound value regardless of the var name.
	if key, ok := strings.CutPrefix(expr, "matrix."); ok {
		if v, ok := sc.matrix[key]; ok {
			return v, true
		}
		if key == "value" && len(sc.matrix) == 1 {
			for _, v := range sc.matrix {
				return v, true
			}
		}
		return "", false
	}
	// ${map.<var>} resolves to this map iteration's binding — the region-scoped twin
	// of ${matrix.<var>}, kept in its own namespace so a step inside a region can
	// still carry a matrix of its own.
	if key, ok := strings.CutPrefix(expr, "map."); ok {
		if v, ok := sc.mapVars[key]; ok {
			return v, true
		}
		if key == "value" && len(sc.mapVars) == 1 {
			for _, v := range sc.mapVars {
				return v, true
			}
		}
		return "", false
	}
	// ${loop.<var>} resolves to this loop iteration's binding. A loop reuses the same
	// per-iteration bindings a map region does (a step is in at most one), so this is
	// an alias over the same namespace — kept distinct so a loop body reads ${loop.x}
	// and a map body reads ${map.x}, each naming what it actually is.
	if key, ok := strings.CutPrefix(expr, "loop."); ok {
		if v, ok := sc.mapVars[key]; ok {
			return v, true
		}
		return "", false
	}
	// ${scatter.path} resolves to the workspace path this scatter leg is bound to.
	if key, ok := strings.CutPrefix(expr, "scatter."); ok {
		if key == "path" {
			return sc.scatterPath, sc.scatterPath != ""
		}
		return "", false
	}
	// ${run_id} / ${run.id} expose the run's id — used to scope a shared workspace
	// volume to the run (workflow_id) and to name it deterministically across steps.
	if expr == "run_id" || expr == "run.id" {
		return sc.runID, sc.runID != ""
	}
	if expr == "workflow_id" || expr == "workflow.id" {
		return sc.workflowID, sc.workflowID != ""
	}
	// Bare ${NAME} resolves to a run input (backward compatible).
	v, ok := sc.inputs[expr]
	return v, ok
}

// jsonField parses raw as JSON and walks the dotted path, returning the value as
// a string (scalars verbatim, objects/arrays re-encoded as JSON).
func jsonField(raw string, path []string) (string, bool) {
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return "", false
	}
	cur := data
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[key]
		if !ok {
			return "", false
		}
	}
	return jsonScalar(cur), true
}

func jsonScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// substituteWith applies substitution to every string value in a With map,
// recursing into nested maps and slices. The map is returned unchanged when the
// context is empty.
func substituteWith(with map[string]any, sc substContext) map[string]any {
	if sc.empty() {
		return with
	}
	result := make(map[string]any, len(with))
	for k, v := range with {
		result[k] = substituteValue(v, sc)
	}
	return result
}

// substituteWithStrict is substituteWith for a step about to execute: it fails the
// step when any ${...} reference in its With map did not resolve, rather than
// handing the literal to the action. Every offending reference is reported at once,
// so a step wired to three missing outputs does not take three runs to fix.
func substituteWithStrict(with map[string]any, sc substContext) (map[string]any, error) {
	if sc.empty() {
		return with, nil
	}
	var bad []unresolvedRef
	result := substituteValueStrict(with, sc, &bad).(map[string]any)
	if len(bad) > 0 {
		return nil, &unresolvedError{step: sc.stepName, refs: bad}
	}
	return result, nil
}

// substituteValueStrict mirrors substituteValue, accumulating unresolved references
// into bad as it walks nested maps and slices.
func substituteValueStrict(v any, sc substContext, bad *[]unresolvedRef) any {
	switch sv := v.(type) {
	case string:
		out, refs := substituteStrict(sv, sc)
		*bad = append(*bad, refs...)
		return out
	case map[string]any:
		result := make(map[string]any, len(sv))
		for k, item := range sv {
			result[k] = substituteValueStrict(item, sc, bad)
		}
		return result
	case []any:
		result := make([]any, len(sv))
		for i, item := range sv {
			result[i] = substituteValueStrict(item, sc, bad)
		}
		return result
	default:
		return v
	}
}

func substituteValue(v any, sc substContext) any {
	switch sv := v.(type) {
	case string:
		return substitute(sv, sc)
	case map[string]any:
		return substituteWith(sv, sc)
	case []any:
		result := make([]any, len(sv))
		for i, item := range sv {
			result[i] = substituteValue(item, sc)
		}
		return result
	default:
		return v
	}
}
