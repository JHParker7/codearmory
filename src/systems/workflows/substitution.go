package main

import (
	"encoding/json"
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
// group. Unresolved references are left untouched (the literal ${...} survives).

// substContext carries everything a step's With values can interpolate.
type substContext struct {
	inputs  map[string]string // run-level inputs, by name
	outputs map[string]string // earlier step outputs, by step name
	matrix  map[string]string // matrix bindings for this execution, by var name
	// scatterPath is the workspace path a scatter leg is bound to, exposed as
	// ${scatter.path} so the leg's command targets its own partition.
	scatterPath string
	runID       string // this run's id, exposed as ${run_id} / ${run.id}
	// depth is the run's sub-pipeline nesting depth, propagated to a workflows/trigger
	// step so the created sub-run is one level deeper (not itself a substitution
	// value, so it is excluded from empty()).
	depth int
}

func (sc substContext) empty() bool {
	return len(sc.inputs) == 0 && len(sc.outputs) == 0 && len(sc.matrix) == 0 && sc.scatterPath == "" && sc.runID == ""
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
