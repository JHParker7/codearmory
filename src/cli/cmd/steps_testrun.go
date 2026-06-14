package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Standalone step testing.
//
// A step can be exercised on its own — before it's added to a pipeline — by
// running it through a throwaway single-step pipeline and then deleting both. This
// lives entirely on the CLI side: no dedicated backend endpoint is needed, and the
// step runs through the exact same path (permissions, substitution, execution) it
// would in a real pipeline.
//
// The dynamic ${...} references a step uses are surfaced as test inputs: the user
// supplies a value for each, the values are baked into a throwaway copy of the
// step's With map, and the copy is run with no run-level inputs.

var stepRefPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// stepWithRefs returns the unique ${...} reference expressions used across a
// step's With values, sorted for a stable field order. These are the inputs a step
// consumes — run inputs (${inputs.X}/${X}) and prior-step outputs
// (${steps.X.output}) — that the test runner asks the user to fill in.
func stepWithRefs(with map[string]any) []string {
	seen := map[string]bool{}
	var refs []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for _, m := range stepRefPattern.FindAllStringSubmatch(t, -1) {
				expr := strings.TrimSpace(m[1])
				if expr != "" && !seen[expr] {
					seen[expr] = true
					refs = append(refs, expr)
				}
			}
		case map[string]any:
			for _, item := range t {
				walk(item)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(with)
	sort.Strings(refs)
	return refs
}

// resolveStepWith deep-copies a With map, replacing every ${expr} with vals[expr]
// (references without a supplied value are left untouched). It bakes the test
// values into a throwaway step so the run needs no run-level inputs.
func resolveStepWith(with map[string]any, vals map[string]string) map[string]any {
	out := make(map[string]any, len(with))
	for k, v := range with {
		out[k] = resolveStepValue(v, vals)
	}
	return out
}

func resolveStepValue(v any, vals map[string]string) any {
	switch t := v.(type) {
	case string:
		return stepRefPattern.ReplaceAllStringFunc(t, func(tok string) string {
			expr := strings.TrimSpace(tok[2 : len(tok)-1])
			if val, ok := vals[expr]; ok {
				return val
			}
			return tok
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = resolveStepValue(item, vals)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = resolveStepValue(item, vals)
		}
		return out
	default:
		return v
	}
}

// stepTestOutcome is the result of a standalone step run.
type stepTestOutcome struct {
	status  string // completed | failed | cancelled | timed out
	output  string // the step's output (response body)
	failure string // error detail when the run did not complete
}

const (
	stepTestPollInterval = time.Second
	stepTestMaxWait      = 120 * time.Second
)

// runStepTest creates a throwaway step (with test values baked in), wraps it in a
// throwaway single-step pipeline, runs it, polls to a terminal state, then deletes
// both. The throwaway resources are always cleaned up, even on error.
func runStepTest(name, action string, with map[string]any, timeout int64) (stepTestOutcome, error) {
	suffix := testSuffix()
	tag := "_test_" + sanitizeName(name) + "_" + suffix

	stepBody, _ := json.Marshal(map[string]any{
		"name":        tag,
		"description": "throwaway step created by 'test step'",
		"action":      action,
		"with":        with,
		"timeout":     timeout,
	})
	stepResp, err := doRequest("POST", "/workflows/steps", stepBody)
	if err != nil {
		return stepTestOutcome{}, fmt.Errorf("create throwaway step: %w", err)
	}
	var step struct {
		StepID string `json:"step_id"`
	}
	json.Unmarshal(stepResp, &step) //nolint:errcheck
	if step.StepID == "" {
		return stepTestOutcome{}, fmt.Errorf("create throwaway step: no step_id in response")
	}
	// Cleanup order (LIFO): delete the pipeline first (frees its run role), then
	// the step. Both are best-effort — a leaked _test_ resource is not fatal.
	defer doRequest("DELETE", "/workflows/steps/"+step.StepID, nil) //nolint:errcheck

	pipeBody, _ := json.Marshal(map[string]any{
		"name":        tag,
		"description": "throwaway pipeline created by 'test step'",
		"steps":       []map[string]any{{"step_id": step.StepID}},
	})
	pipeResp, err := doRequest("POST", "/workflows/pipelines", pipeBody)
	if err != nil {
		return stepTestOutcome{}, fmt.Errorf("create throwaway pipeline: %w", err)
	}
	var pipe struct {
		WorkflowID string `json:"workflow_id"`
	}
	json.Unmarshal(pipeResp, &pipe) //nolint:errcheck
	if pipe.WorkflowID == "" {
		return stepTestOutcome{}, fmt.Errorf("create throwaway pipeline: no workflow_id in response")
	}
	defer doRequest("DELETE", "/workflows/pipelines/"+pipe.WorkflowID, nil) //nolint:errcheck

	runResp, err := doRequest("POST", "/workflows/pipelines/"+pipe.WorkflowID+"/runs", []byte("{}"))
	if err != nil {
		return stepTestOutcome{}, fmt.Errorf("trigger test run: %w", err)
	}
	var run struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(runResp, &run) //nolint:errcheck
	if run.RunID == "" {
		return stepTestOutcome{}, fmt.Errorf("trigger test run: no run_id in response")
	}

	return pollStepTest(run.RunID)
}

// pollStepTest polls a run until it reaches a terminal state or the wait budget is
// exhausted, returning the first step's output.
func pollStepTest(runID string) (stepTestOutcome, error) {
	deadline := time.Now().Add(stepTestMaxWait)
	for {
		data, err := doRequest("GET", "/workflows/runs/"+runID, nil)
		if err != nil {
			return stepTestOutcome{}, fmt.Errorf("poll run: %w", err)
		}
		var run struct {
			Status   string `json:"status"`
			StepRuns []struct {
				Status string  `json:"status"`
				Output *string `json:"output"`
			} `json:"step_runs"`
		}
		json.Unmarshal(data, &run) //nolint:errcheck

		switch run.Status {
		case "completed", "failed", "cancelled":
			out := stepTestOutcome{status: run.Status}
			if len(run.StepRuns) > 0 && run.StepRuns[0].Output != nil {
				out.output = *run.StepRuns[0].Output
				if run.Status != "completed" {
					out.failure = *run.StepRuns[0].Output
				}
			}
			return out, nil
		}
		if time.Now().After(deadline) {
			return stepTestOutcome{status: "timed out", failure: "run did not finish within the test window"}, nil
		}
		time.Sleep(stepTestPollInterval)
	}
}

// testSuffix returns a short random hex suffix so throwaway names never collide.
func testSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "tmp"
	}
	return hex.EncodeToString(b)
}

// sanitizeName trims a step name to a short, identifier-friendly fragment for the
// throwaway resource name.
func sanitizeName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, name)
	if len(name) > 24 {
		name = name[:24]
	}
	return name
}
