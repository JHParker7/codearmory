package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// Route conditions ("when") are expressions, evaluated once the source node of a
// route reaches a terminal state.
//
// Why a real expression language and not on_success/on_failure flags: gating on a
// step's OUTPUT ("only publish if a release was actually cut") is the case that
// motivated the graph model, and a status enum cannot express it.
//
// Why expr-lang/expr specifically:
//   - zero required dependencies, which matters for a module whose only
//     non-observability deps are uuid and gorm;
//   - not Turing-complete — no user-defined loops, recursion, I/O, or reflection,
//     so evaluating an untrusted pipeline definition is bounded work, not a sandbox
//     escape problem;
//   - expr.Compile with expr.Env does COMPILE-TIME type checking against exprEnv,
//     so `steps.buidl.status` is a 400 at authoring time with a column offset
//     rather than a silent false at 3am. That property is the whole reason for the
//     dependency.
//
// Route conditions are deliberately NOT the same language as the ${...} templating
// in substitution.go, and they must not be unified. ${...} is a string template
// over arbitrary user config and has to be TOLERANT — substitution.go leaves an
// unknown reference as a literal, because a With value may legitimately contain
// ${SOMETHING} meant for a downstream shell. A route condition has the exact
// opposite requirement: an unknown identifier MUST be an error, or a typo silently
// makes a branch dead. The identifier namespace is kept aligned so they read the
// same (${steps.X.output.a} <-> steps.X.json.a), but the engines stay separate.

// exprMaxNodes caps the compiled AST size. With no loops the node count is the
// only proxy for evaluation cost.
const exprMaxNodes = 256

// exprEvalTimeout bounds a single route evaluation. expr is not Turing-complete so
// this should be unreachable; it exists so a pathological expression degrades to a
// failed route rather than stalling the run's scheduler goroutine.
const exprEvalTimeout = 100 * time.Millisecond

// stepView is what a route condition can see about a node.
//
// Status is the node's terminal state; Output is its raw string output; JSON is
// the output parsed as an object, or nil when the output is not a JSON object, so
// a condition can read a field without a parse step.
type stepView struct {
	Status string         `expr:"status"`
	Output string         `expr:"output"`
	JSON   map[string]any `expr:"json"`
}

// runView exposes the run itself, mirroring ${run_id} in substitution.go.
type runView struct {
	ID string `expr:"id"`
}

// exprEnv is the environment a route condition is compiled and evaluated against.
// Its shape IS the contract: expr.Env type-checks against it, so anything absent
// here is a compile error rather than a runtime nil.
//
// Only nodes in a TERMINAL state ever appear in Steps — a condition can never
// observe a running node, which keeps evaluation deterministic and mirrors the
// scheduler's ancestor-scoped visibility rule.
//
// Deliberately absent: matrix/scatter bindings (a route connects nodes, and
// fan-out is internal to a node), and anything with an effect.
type exprEnv struct {
	Inputs map[string]string   `expr:"inputs"`
	Steps  map[string]stepView `expr:"steps"`
	Run    runView             `expr:"run"`
}

// compileWhen compiles a route condition. An empty condition has no program: the
// route is then taken iff its source node completed, which is the default that
// reproduces the pre-graph linear semantics.
//
// AsBool rejects a condition that is not a boolean (e.g. bare `steps.build.output`,
// a string) at compile time rather than coercing it.
func compileWhen(src string) (*vm.Program, error) {
	if src == "" {
		return nil, nil
	}
	return expr.Compile(src,
		expr.Env(exprEnv{}),
		expr.AsBool(),
		expr.MaxNodes(exprMaxNodes),
	)
}

// buildExprEnv assembles the environment for one evaluation from the terminal
// nodes' recorded states and outputs.
func buildExprEnv(inputs map[string]string, runID string, states map[string]nodeState, outputs map[string]string) exprEnv {
	steps := make(map[string]stepView, len(states))
	for name, st := range states {
		if !st.terminal() {
			continue
		}
		out := outputs[name]
		sv := stepView{Status: string(st), Output: out}
		// Best effort: a non-JSON output simply has no .json view rather than
		// failing the condition, so `steps.x.json.field` on a plain-text output is
		// a nil field access, not a parse error.
		var obj map[string]any
		if out != "" && json.Unmarshal([]byte(out), &obj) == nil {
			sv.JSON = obj
		}
		steps[name] = sv
	}
	return exprEnv{Inputs: inputs, Steps: steps, Run: runView{ID: runID}}
}

// evalWhen runs a compiled condition. A nil program means "no condition".
//
// Evaluation happens on a goroutine bounded by exprEvalTimeout so that a
// pathological expression cannot stall the scheduler. expr terminates on its own,
// so the goroutine cannot leak indefinitely.
func evalWhen(prog *vm.Program, env exprEnv) (bool, error) {
	if prog == nil {
		return true, nil
	}
	type res struct {
		v   any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := expr.Run(prog, env)
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return false, r.err
		}
		b, ok := r.v.(bool)
		if !ok {
			// AsBool should make this unreachable; treat it as an error rather
			// than silently coercing.
			return false, fmt.Errorf("condition returned %T, want bool", r.v)
		}
		return b, nil
	case <-time.After(exprEvalTimeout):
		return false, errors.New("condition timed out")
	}
}
