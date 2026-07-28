package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ── Pure substitution / matrix helpers ─────────────────────────────────────────

func TestSubstitute_MatrixBindings(t *testing.T) {
	sc := substContext{matrix: map[string]string{"region": "us-east-1"}}
	if got := substitute("deploy ${matrix.region}", sc); got != "deploy us-east-1" {
		t.Errorf("matrix.region: got %q", got)
	}
	// ${matrix.value} is a generic alias when there is exactly one binding.
	if got := substitute("v=${matrix.value}", sc); got != "v=us-east-1" {
		t.Errorf("matrix.value alias: got %q", got)
	}
	// Unknown binding is left untouched.
	if got := substitute("${matrix.missing}", sc); got != "${matrix.missing}" {
		t.Errorf("unknown matrix key should pass through, got %q", got)
	}
	// value alias does not resolve when there are multiple bindings (ambiguous).
	multi := substContext{matrix: map[string]string{"a": "1", "b": "2"}}
	if got := substitute("${matrix.value}", multi); got != "${matrix.value}" {
		t.Errorf("ambiguous matrix.value should pass through, got %q", got)
	}
}

func TestParseMatrixList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`["a","b","c"]`, []string{"a", "b", "c"}},
		{`[1,2,3]`, []string{"1", "2", "3"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{"  ", nil},
		{`[]`, []string{}},
		{"solo", []string{"solo"}},
		// Linux-style list output: whitespace separates values just like commas.
		{"a b c", []string{"a", "b", "c"}},                           // space-separated (echo a b c)
		{"a.txt\nb.txt\nc.txt", []string{"a.txt", "b.txt", "c.txt"}}, // newline-separated (ls)
		{"a.txt\nb.txt\n", []string{"a.txt", "b.txt"}},               // trailing newline dropped
		{"a\tb  c", []string{"a", "b", "c"}},                         // tabs and runs of spaces collapse
		{"a, b\nc d", []string{"a", "b", "c", "d"}},                  // mixed comma + whitespace
	}
	for _, c := range cases {
		got := parseMatrixList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseMatrixList(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseMatrixList(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestResolveMatrixValues_LiteralAndFrom(t *testing.T) {
	sc := substContext{inputs: map[string]string{"regions": `["eu","us"]`, "tier": "prod"}}

	// Literal values, each resolved against the context.
	vals, err := resolveMatrixValues(&MatrixConfig{Var: "t", Values: []string{"${inputs.tier}", "stage"}}, sc)
	if err != nil {
		t.Fatalf("literal: %v", err)
	}
	if len(vals) != 2 || vals[0] != "prod" || vals[1] != "stage" {
		t.Errorf("literal values = %v", vals)
	}

	// values_from resolves a reference to a JSON array.
	vals, err = resolveMatrixValues(&MatrixConfig{Var: "r", ValuesFrom: "${inputs.regions}"}, sc)
	if err != nil {
		t.Fatalf("values_from: %v", err)
	}
	if len(vals) != 2 || vals[0] != "eu" || vals[1] != "us" {
		t.Errorf("values_from = %v", vals)
	}
}

func TestResolveMatrixValues_CapExceeded(t *testing.T) {
	big := make([]string, maxMatrixValues+1)
	for i := range big {
		big[i] = "v"
	}
	if _, err := resolveMatrixValues(&MatrixConfig{Var: "x", Values: big}, substContext{}); err == nil {
		t.Fatal("expected error when matrix exceeds the cap")
	}
}

func TestBuildGroupTasks(t *testing.T) {
	// Matrix step → one task per value plus an aggregate name.
	mstep := WorkflowStep{Step: Step{Name: "deploy", Action: "http"}, Matrix: &MatrixConfig{Var: "env", Values: []string{"a", "b"}}}
	tasks, agg, err := buildGroupTasks(stepGroup{steps: []WorkflowStep{mstep}, indices: []int{2}}, substContext{})
	if err != nil {
		t.Fatalf("matrix buildGroupTasks: %v", err)
	}
	if agg != "deploy" || len(tasks) != 2 {
		t.Fatalf("matrix tasks=%d agg=%q", len(tasks), agg)
	}
	if tasks[0].matrix["env"] != "a" || tasks[1].matrix["env"] != "b" {
		t.Errorf("matrix bindings wrong: %+v", tasks)
	}
	if tasks[0].stepIndex != 2 || tasks[1].stepIndex != 2 {
		t.Errorf("matrix tasks should share the step index")
	}

	// Parallel group → one task per member, no aggregate.
	p1 := WorkflowStep{Step: Step{Name: "x", Action: "http"}}
	p2 := WorkflowStep{Step: Step{Name: "y", Action: "http"}}
	tasks, agg, err = buildGroupTasks(stepGroup{steps: []WorkflowStep{p1, p2}, indices: []int{0, 1}}, substContext{})
	if err != nil || agg != "" || len(tasks) != 2 {
		t.Fatalf("parallel tasks=%d agg=%q err=%v", len(tasks), agg, err)
	}
}

func TestGroupConcurrency(t *testing.T) {
	mk := func(m *MatrixConfig) stepGroup {
		return stepGroup{steps: []WorkflowStep{{Step: Step{Name: "s", Action: "http"}, Matrix: m}}, indices: []int{0}}
	}
	cases := []struct {
		name  string
		group stepGroup
		want  int
	}{
		{"matrix throttled below cap", mk(&MatrixConfig{Var: "v", Values: []string{"a"}, MaxConcurrent: 3}), 3},
		{"matrix cap of 1", mk(&MatrixConfig{Var: "v", Values: []string{"a"}, MaxConcurrent: 1}), 1},
		{"matrix sequential pins to 1", mk(&MatrixConfig{Var: "v", Values: []string{"a", "b"}, Sequential: true}), 1},
		{"matrix sequential overrides max_concurrent", mk(&MatrixConfig{Var: "v", Values: []string{"a"}, MaxConcurrent: 5, Sequential: true}), 1},
		{"matrix above cap uses ceiling", mk(&MatrixConfig{Var: "v", Values: []string{"a"}, MaxConcurrent: 99}), maxParallelSteps},
		{"matrix unset defaults to 3", mk(&MatrixConfig{Var: "v", Values: []string{"a"}}), defaultFanoutConcurrency},
		{"parallel group uses ceiling", stepGroup{steps: []WorkflowStep{{Step: Step{Name: "a"}}, {Step: Step{Name: "b"}}}, indices: []int{0, 1}}, maxParallelSteps},
		{"sequential step uses ceiling", stepGroup{steps: []WorkflowStep{{Step: Step{Name: "a"}}}, indices: []int{0}}, maxParallelSteps},
	}
	for _, c := range cases {
		if got := groupConcurrency(c.group); got != c.want {
			t.Errorf("%s: groupConcurrency = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestRunTaskGroup_MatrixMaxConcurrent drives a real matrix fan-out through the
// worker's semaphore and asserts that no more than MaxConcurrent executions are
// ever in flight at once — the throttle a resource-heavy matrix relies on.
func TestRunTaskGroup_MatrixMaxConcurrent(t *testing.T) {
	if !testDBReady {
		t.Skip("test DB not ready")
	}
	const maxConcurrent = 2
	var mu sync.Mutex
	var inFlight, peak int
	fakeService(t, "forgeconc", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond) // hold the slot so overlap is observable
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	})

	// Six matrix values, throttled to two at a time: peak must never exceed two.
	mstep := WorkflowStep{
		Step:   Step{Name: "deploy", Action: ActionHTTP, With: map[string]any{"service": "forgeconc", "path": "/run", "method": "GET"}},
		Matrix: &MatrixConfig{Var: "v", Values: []string{"1", "2", "3", "4", "5", "6"}, MaxConcurrent: maxConcurrent},
	}
	group := stepGroup{steps: []WorkflowStep{mstep}, indices: []int{0}}
	tasks, _, err := buildGroupTasks(group, substContext{})
	if err != nil {
		t.Fatalf("buildGroupTasks: %v", err)
	}
	runID := uuid.New().String()
	if err := (WorkflowRun{RunID: runID, WorkflowID: uuid.New().String(), Status: StatusRunning}).Add(context.Background()); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	results, status := (&WorkerPool{}).runTaskGroup(context.Background(), newTokenStore("", ""), runID, "wf", tasks, nil, nil, nil, 0, groupConcurrency(group), nil)
	if status != StatusCompleted {
		t.Fatalf("group status = %s, want completed", status)
	}
	if len(results) != 6 {
		t.Fatalf("results = %d, want 6", len(results))
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > maxConcurrent {
		t.Errorf("peak concurrency = %d, want <= %d", peak, maxConcurrent)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, expected the throttle to still allow parallelism", peak)
	}
}

func TestAggregateTaskOutputs_PreservesOrder(t *testing.T) {
	// Results may arrive out of order; aggregation keys on idx.
	results := []taskResult{
		{output: "first", idx: 0},
		{output: "second", idx: 1},
		{output: "third", idx: 2},
	}
	if got := aggregateTaskOutputs([]taskResult{results[2], results[0], results[1]}); got != `["first","second","third"]` {
		t.Errorf("aggregateTaskOutputs = %s", got)
	}
}

// ── Validation ─────────────────────────────────────────────────────────────────

func TestValidateMatrix(t *testing.T) {
	cases := []struct {
		name string
		m    MatrixConfig
		ok   bool
	}{
		{"missing var", MatrixConfig{Values: []string{"a"}}, false},
		{"bad var", MatrixConfig{Var: "a b", Values: []string{"a"}}, false},
		{"both sources", MatrixConfig{Var: "v", Values: []string{"a"}, ValuesFrom: "${x}"}, false},
		{"no source", MatrixConfig{Var: "v"}, false},
		{"literal ok", MatrixConfig{Var: "v", Values: []string{"a"}}, true},
		{"from ok", MatrixConfig{Var: "v", ValuesFrom: "${inputs.x}"}, true},
		{"negative max_concurrent", MatrixConfig{Var: "v", Values: []string{"a"}, MaxConcurrent: -1}, false},
		{"positive max_concurrent ok", MatrixConfig{Var: "v", Values: []string{"a"}, MaxConcurrent: 3}, true},
	}
	for _, c := range cases {
		msg := validateMatrix(&c.m)
		if (msg == "") != c.ok {
			t.Errorf("%s: validateMatrix msg=%q ok=%v", c.name, msg, c.ok)
		}
	}
}

// An inline step carries its whole definition on the ref (no step_id). It is
// validated like a stored step, must have a name, cannot double as a reference or a
// gate, and cannot use the approval action.
func TestValidateStepRefShape_Inline(t *testing.T) {
	cases := []struct {
		name string
		ref  WorkflowStepRef
		ok   bool
	}{
		{"valid inline", WorkflowStepRef{Action: "forge/run", Name: "build", With: map[string]any{"image": "alpine"}}, true},
		{"inline with matrix", WorkflowStepRef{Action: "forge/run", Name: "build", Matrix: &MatrixConfig{Var: "v", Values: []string{"a"}}}, true},
		{"step_id and action", WorkflowStepRef{StepID: uuid.New().String(), Action: "forge/run", Name: "x"}, false},
		{"neither step_id nor action", WorkflowStepRef{}, false},
		{"inline missing name", WorkflowStepRef{Action: "forge/run"}, false},
		{"inline approval action rejected", WorkflowStepRef{Action: ActionApproval, Name: "gate"}, false},
		{"inline and gate", WorkflowStepRef{Action: "forge/run", Name: "x", Approval: &ApprovalGate{Message: "m"}}, false},
		{"inline http missing service", WorkflowStepRef{Action: ActionHTTP, Name: "h", With: map[string]any{"path": "/x"}}, false},
		{"inline http ok", WorkflowStepRef{Action: ActionHTTP, Name: "h", With: map[string]any{"service": "s", "path": "/x"}}, true},
	}
	for _, c := range cases {
		if (validateStepRefShape(0, c.ref) == "") != c.ok {
			t.Errorf("%s: validateStepRefShape(...) ok mismatch, want ok=%v", c.name, c.ok)
		}
	}
}

// ── Worker integration: matrix fan-out ─────────────────────────────────────────

func seedApprovalStep(t *testing.T, user, org, message string, approvers []string) Step {
	t.Helper()
	now := time.Now().UTC()
	with := map[string]any{}
	if message != "" {
		with["message"] = message
	}
	if len(approvers) > 0 {
		with["approvers"] = approvers
	}
	s := Step{
		StepID: uuid.New().String(), Name: "approve-" + uuid.New().String(), Action: ActionApproval,
		With: with, Timeout: 30, CreatedBy: user, OrgID: org, Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Add(context.Background()); err != nil {
		t.Fatalf("seedApprovalStep: %v", err)
	}
	t.Cleanup(func() { connect().Exec(`DELETE FROM steps WHERE step_id = ?`, s.StepID) }) //nolint:errcheck
	return s
}

// recordingService registers a stub service that records each request path.
func recordingService(t *testing.T, name string) *[]string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	fakeService(t, name, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	return &paths
}

func TestExecuteRun_MatrixFansOut(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	paths := recordingService(t, "msvc")
	step := seedHTTPStep(t, "tu", "to", "msvc", "/hit/${matrix.target}")
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": step.StepID, "matrix": map[string]any{"var": "target", "values": []string{"a", "b", "c"}}},
	})

	got := runOnce(t, wf)
	if got.Status != StatusCompleted {
		t.Fatalf("matrix run status = %q, want completed", got.Status)
	}
	// One step run per matrix value.
	srs, _ := getStepRuns(context.Background(), got.RunID)
	if len(srs) != 3 {
		t.Fatalf("expected 3 step runs, got %d", len(srs))
	}
	// Each value produced its own substituted request.
	sort.Strings(*paths)
	want := []string{"/hit/a", "/hit/b", "/hit/c"}
	if len(*paths) != 3 || (*paths)[0] != want[0] || (*paths)[1] != want[1] || (*paths)[2] != want[2] {
		t.Errorf("matrix request paths = %v, want %v", *paths, want)
	}
}

func TestExecuteRun_MatrixValuesFromInput(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	paths := recordingService(t, "fsvc")
	step := seedHTTPStep(t, "tu", "to", "fsvc", "/r/${matrix.region}")
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": step.StepID, "matrix": map[string]any{"var": "region", "values_from": "${inputs.regions}"}},
	})

	// Drive the value list from a run input holding a JSON array.
	run := WorkflowRun{RunID: uuid.New().String(), WorkflowID: wf.WorkflowID, TriggeredBy: "tu", OrgID: "to", Status: "pending", Inputs: map[string]string{"regions": `["eu","us"]`}, CreatedAt: time.Now().UTC()}
	run.Add(context.Background())                                                               //nolint:errcheck
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, run.RunID)       //nolint:errcheck
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_runs WHERE run_id=?`, run.RunID) }) //nolint:errcheck
	newWorkerPool().executeRun(context.Background(), run.RunID, wf.WorkflowID, "", "", "tu", run.Inputs, 0, "")

	got, _ := getRun(context.Background(), run.RunID)
	if got.Status != StatusCompleted {
		t.Fatalf("values_from run status = %q, want completed", got.Status)
	}
	if len(*paths) != 2 {
		t.Fatalf("expected 2 requests, got %d (%v)", len(*paths), *paths)
	}
}

// ── Worker integration: approval pause / resume / reject ───────────────────────

// resumeRun simulates a worker re-dequeuing a re-queued run: mark it running and
// execute it again. Returns the run after the second pass.
func resumeRun(t *testing.T, runID, workflowID string) WorkflowRun {
	t.Helper()
	connect().Exec(`UPDATE workflow_runs SET status='running' WHERE run_id=?`, runID) //nolint:errcheck
	newWorkerPool().executeRun(context.Background(), runID, workflowID, "", "", "tu", map[string]string{}, 0, "")
	got, _ := getRun(context.Background(), runID)
	return got
}

func TestExecuteRun_ApprovalPauses(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "asvc", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s0 := seedHTTPStep(t, "tu", "to", "asvc", "/a")
	gate := seedApprovalStep(t, "tu", "to", "deploy to prod?", nil)
	wf := createWorkflowWith(t, []map[string]any{{"step_id": s0.StepID}, {"step_id": gate.StepID}})

	got := runOnce(t, wf)
	if got.Status != StatusAwaitingApproval {
		t.Fatalf("run status = %q, want awaiting_approval", got.Status)
	}
	srs, _ := getStepRuns(context.Background(), got.RunID)
	if len(srs) != 2 {
		t.Fatalf("expected 2 step runs (first + gate), got %d", len(srs))
	}
	// First step completed; the gate is awaiting approval and carries the prompt.
	var gateRun *WorkflowStepRun
	for i := range srs {
		if srs[i].StepIndex == 1 {
			gateRun = &srs[i]
		}
	}
	if gateRun == nil || gateRun.Status != StatusAwaitingApproval {
		t.Fatalf("gate step run = %+v, want awaiting_approval", gateRun)
	}
	if gateRun.Output == nil || *gateRun.Output != "deploy to prod?" {
		t.Errorf("gate prompt not stored: %+v", gateRun.Output)
	}
}

func TestApproveRun_Resumes(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	firstPaths := recordingService(t, "s0svc")
	lastPaths := recordingService(t, "s2svc")
	s0 := seedHTTPStep(t, "tu", "to", "s0svc", "/first")
	gate := seedApprovalStep(t, "tu", "to", "ok?", nil)
	s2 := seedHTTPStep(t, "tu", "to", "s2svc", "/last")
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": s0.StepID}, {"step_id": gate.StepID}, {"step_id": s2.StepID},
	})

	paused := runOnce(t, wf)
	if paused.Status != StatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %q", paused.Status)
	}

	// Approve via the handler.
	r := authReq(http.MethodPost, "/runs/"+paused.RunID+"/approve", []byte(`{"comment":"lgtm"}`))
	r.SetPathValue("id", paused.RunID)
	w := httptest.NewRecorder()
	handleApproveRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("approve got %d: %s", w.Code, w.Body.String())
	}

	resumed := resumeRun(t, paused.RunID, wf.WorkflowID)
	if resumed.Status != StatusCompleted {
		t.Fatalf("resumed run status = %q, want completed", resumed.Status)
	}
	// The first step ran exactly once (resume skipped it); the last step ran.
	if len(*firstPaths) != 1 {
		t.Errorf("first step ran %d times, want 1 (resume must skip completed groups)", len(*firstPaths))
	}
	if len(*lastPaths) != 1 {
		t.Errorf("last step ran %d times, want 1", len(*lastPaths))
	}
	// The gate's audit line records the approver and comment.
	srs, _ := getStepRuns(context.Background(), paused.RunID)
	for _, sr := range srs {
		if sr.StepIndex == 1 {
			if sr.Status != StatusCompleted || sr.Output == nil || *sr.Output != "approved by tu: lgtm" {
				t.Errorf("gate audit = %+v / %v", sr.Status, sr.Output)
			}
		}
	}
}

func TestRejectRun_FailsRun(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	gate := seedApprovalStep(t, "tu", "to", "", nil)
	wf := createWorkflowWith(t, []map[string]any{{"step_id": gate.StepID}})

	paused := runOnce(t, wf)
	if paused.Status != StatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %q", paused.Status)
	}

	r := authReq(http.MethodPost, "/runs/"+paused.RunID+"/reject", []byte(`{"comment":"no"}`))
	r.SetPathValue("id", paused.RunID)
	w := httptest.NewRecorder()
	handleRejectRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("reject got %d: %s", w.Code, w.Body.String())
	}

	got, _ := getRun(context.Background(), paused.RunID)
	if got.Status != StatusFailed {
		t.Fatalf("rejected run status = %q, want failed", got.Status)
	}
}

func TestApproveRun_NotAwaiting409(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "qsvc", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	step := seedHTTPStep(t, "tu", "to", "qsvc", "/x")
	wf := createWorkflowWith(t, []map[string]any{{"step_id": step.StepID}})
	got := runOnce(t, wf) // completes immediately, never pauses

	r := authReq(http.MethodPost, "/runs/"+got.RunID+"/approve", nil)
	r.SetPathValue("id", got.RunID)
	w := httptest.NewRecorder()
	handleApproveRun(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("approve of non-paused run got %d, want 409", w.Code)
	}
}

func TestApproveRun_NotAnApprover403(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	gate := seedApprovalStep(t, "tu", "to", "restricted", []string{"someone-else"})
	wf := createWorkflowWith(t, []map[string]any{{"step_id": gate.StepID}})
	paused := runOnce(t, wf)
	if paused.Status != StatusAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %q", paused.Status)
	}

	r := authReq(http.MethodPost, "/runs/"+paused.RunID+"/approve", nil)
	r.SetPathValue("id", paused.RunID)
	w := httptest.NewRecorder()
	handleApproveRun(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("approve by non-approver got %d, want 403", w.Code)
	}
}

func TestApprovalStepRejectedByMatrixValidation(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	gate := seedApprovalStep(t, "tu", "to", "", nil)
	body, _ := json.Marshal(map[string]any{
		"name": "wf-" + uuid.New().String(),
		"steps": []map[string]any{
			{"step_id": gate.StepID, "matrix": map[string]any{"var": "v", "values": []string{"a", "b"}}},
		},
	})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("approval+matrix create got %d, want 400: %s", w.Code, w.Body.String())
	}
}

// An inline approval gate (no step row) pauses and resumes just like an
// approval-action step.
func TestExecuteRun_InlineApprovalGatePauses(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "igsvc", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s0 := seedHTTPStep(t, "tu", "to", "igsvc", "/a")
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": s0.StepID},
		{"approval": map[string]any{"message": "deploy to prod?"}},
	})

	got := runOnce(t, wf)
	if got.Status != StatusAwaitingApproval {
		t.Fatalf("status = %q, want awaiting_approval", got.Status)
	}
	srs, _ := getStepRuns(context.Background(), got.RunID)
	var gate *WorkflowStepRun
	for i := range srs {
		if srs[i].StepIndex == 1 {
			gate = &srs[i]
		}
	}
	if gate == nil || gate.Status != StatusAwaitingApproval || gate.Output == nil || *gate.Output != "deploy to prod?" {
		t.Fatalf("gate step run = %+v", gate)
	}

	r := authReq(http.MethodPost, "/runs/"+got.RunID+"/approve", nil)
	r.SetPathValue("id", got.RunID)
	w := httptest.NewRecorder()
	handleApproveRun(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("approve got %d: %s", w.Code, w.Body.String())
	}
	if resumed := resumeRun(t, got.RunID, wf.WorkflowID); resumed.Status != StatusCompleted {
		t.Fatalf("resumed run status = %q, want completed", resumed.Status)
	}
}

func TestInlineApprovalGate_ValidationRejectsBadShapes(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	s0 := seedHTTPStep(t, "tu", "to", "vsvc", "/a")
	cases := []map[string]any{
		{"approval": map[string]any{"message": "x"}, "step_id": s0.StepID},                                          // both gate and step ref
		{"approval": map[string]any{"message": "x"}, "matrix": map[string]any{"var": "v", "values": []string{"a"}}}, // gate with a matrix
	}
	for i, step := range cases {
		body, _ := json.Marshal(map[string]any{"name": "wf-" + uuid.New().String(), "steps": []map[string]any{step}})
		w := httptest.NewRecorder()
		handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("case %d: got %d, want 400: %s", i, w.Code, w.Body.String())
		}
	}
}

// An async action whose poll response carries a non-empty map at OutputMapField
// uses that map (JSON-encoded) as the step output, so ${steps.NAME.output.KEY}
// resolves — this is how a forge/run step surfaces captured output_env vars.
func TestExecuteAction_OutputMapFieldBecomesOutput(t *testing.T) {
	srv := fakeService(t, "forgeom", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"execution_id":"e1"}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"completed","stdout":"build log","outputs":{"BUILD_ID":"42","VERSION":"1.0"}}`)) //nolint:errcheck
	})
	def := ActionDef{
		Name: "forge/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/executions",
		Async: &AsyncConfig{IDField: "execution_id", PollPath: "/executions/{id}", PollIntervalSecs: 1,
			StatusField: "status", SuccessStates: []string{"completed"}, OutputField: "stdout", OutputMapField: "outputs"},
	}
	res, err := (&WorkerPool{}).executeAction(context.Background(), newTokenStore("", ""), def, map[string]any{"image": "alpine"}, "", 0)
	if err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	var got map[string]string
	if jerr := json.Unmarshal([]byte(res.Output), &got); jerr != nil {
		t.Fatalf("output is not the captured JSON map: %q (%v)", res.Output, jerr)
	}
	if got["BUILD_ID"] != "42" || got["VERSION"] != "1.0" {
		t.Errorf("captured outputs = %v", got)
	}
}

// When an action declares OutputMapField, stdout is never the success output: with
// no captured outputs the step output is empty (it does NOT fall back to stdout).
func TestExecuteAction_EmptyOutputMapYieldsNoOutput(t *testing.T) {
	srv := fakeService(t, "forgeom2", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"execution_id":"e1"}`)) //nolint:errcheck
			return
		}
		w.Write([]byte(`{"status":"completed","stdout":"hello","outputs":{}}`)) //nolint:errcheck
	})
	def := ActionDef{
		Name: "forge/run", ServiceURL: srv.URL, Method: http.MethodPost, Path: "/executions",
		Async: &AsyncConfig{IDField: "execution_id", PollPath: "/executions/{id}", PollIntervalSecs: 1,
			StatusField: "status", SuccessStates: []string{"completed"}, OutputField: "stdout", OutputMapField: "outputs"},
	}
	res, err := (&WorkerPool{}).executeAction(context.Background(), newTokenStore("", ""), def, map[string]any{}, "", 0)
	if err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	if res.Output != "" {
		t.Errorf("expected no output (stdout is not a success output), got %q", res.Output)
	}
}

// A step ref's per-occurrence name overrides the definition name: the step run
// records it and a later step references that occurrence's output by it.
func TestExecuteRun_PerOccurrenceNameOverride(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "pon0", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("OK")) }) //nolint:errcheck
	paths := recordingService(t, "pon1")
	s0 := seedHTTPStep(t, "tu", "to", "pon0", "/produce")
	s1 := seedHTTPStep(t, "tu", "to", "pon1", "/got/${steps.build-prod.output}")
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": s0.StepID, "name": "build-prod"},
		{"step_id": s1.StepID},
	})

	got := runOnce(t, wf)
	if got.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	srs, _ := getStepRuns(context.Background(), got.RunID)
	named := false
	for _, sr := range srs {
		if sr.StepIndex == 0 && sr.StepName == "build-prod" {
			named = true
		}
	}
	if !named {
		t.Errorf("step 0 run name not overridden to build-prod: %+v", srs)
	}
	// The downstream step resolved ${steps.build-prod.output} to step 0's body.
	if len(*paths) != 1 || (*paths)[0] != "/got/OK" {
		t.Errorf("downstream path = %v, want [/got/OK]", *paths)
	}
}

// A per-occurrence With override wires a step's input to an earlier step's output
// without editing the shared step definition.
func TestExecuteRun_PerOccurrenceWithOverride(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	fakeService(t, "wo0", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("X")) }) //nolint:errcheck
	paths := recordingService(t, "wo1")
	s0 := seedHTTPStep(t, "tu", "to", "wo0", "/produce")
	s1 := seedHTTPStep(t, "tu", "to", "wo1", "/default") // default path, overridden below
	wf := createWorkflowWith(t, []map[string]any{
		{"step_id": s0.StepID, "name": "build"},
		{"step_id": s1.StepID, "with": map[string]any{"path": "/wired/${steps.build.output}"}},
	})

	got := runOnce(t, wf)
	if got.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	// The override path won and the wired ${steps.build.output} resolved to "X".
	if len(*paths) != 1 || (*paths)[0] != "/wired/X" {
		t.Errorf("override+wiring not applied, paths = %v, want [/wired/X]", *paths)
	}
}

// An inline step (definition on the ref, no stored step row) is enriched and
// executed end-to-end just like a stored-step reference. Its With is the full
// config (not an override), so with.path is used verbatim.
func TestExecuteRun_InlineStepRuns(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	paths := recordingService(t, "inlinesvc")
	wf := createWorkflowWith(t, []map[string]any{
		{"action": ActionHTTP, "name": "inline-hit", "with": map[string]any{"service": "inlinesvc", "path": "/inline"}},
	})

	got := runOnce(t, wf)
	if got.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if len(*paths) != 1 || (*paths)[0] != "/inline" {
		t.Errorf("inline step did not run as expected, paths = %v", *paths)
	}
	// The step run records the inline step's name (its ${steps.<name>.output} key).
	srs, _ := getStepRuns(context.Background(), got.RunID)
	if len(srs) != 1 || srs[0].StepName != "inline-hit" {
		t.Errorf("inline step run = %+v", srs)
	}
}

// A matrix over an inline step fans it out, exercising the inline enrich path's
// Matrix pass-through.
func TestExecuteRun_InlineStepMatrixFansOut(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	paths := recordingService(t, "inlinematrix")
	wf := createWorkflowWith(t, []map[string]any{
		{"action": ActionHTTP, "name": "hit", "with": map[string]any{"service": "inlinematrix", "path": "/hit/${matrix.target}"},
			"matrix": map[string]any{"var": "target", "values": []string{"a", "b", "c"}}},
	})

	got := runOnce(t, wf)
	if got.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if len(*paths) != 3 {
		t.Errorf("inline matrix did not fan out to 3, paths = %v", *paths)
	}
}

// An inline step's name shares the per-pipeline uniqueness namespace with stored
// references and gates — a collision is rejected at create time.
func TestCreateWorkflow_InlineAndRefDuplicateNameRejected(t *testing.T) {
	requireDB(t)
	stubGatekeeperRouting(t, "tu", "to")
	s0 := seedHTTPStep(t, "tu", "to", "dupsvc", "/a")
	body, _ := json.Marshal(map[string]any{
		"name": "wf-" + uuid.New().String(),
		"steps": []map[string]any{
			{"step_id": s0.StepID, "name": "dup"},
			{"action": ActionHTTP, "name": "dup", "with": map[string]any{"service": "dupsvc", "path": "/b"}},
		},
	})
	w := httptest.NewRecorder()
	handleCreateWorkflow(w, authReq(http.MethodPost, "/pipelines", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("inline+ref duplicate name got %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestRebuildResumeState(t *testing.T) {
	requireDB(t)
	runID := uuid.New().String()
	t.Cleanup(func() { connect().Exec(`DELETE FROM workflow_step_runs WHERE run_id=?`, runID) }) //nolint:errcheck
	// One normal completed step at index 0, a matrix step (two completed executions) at index 1.
	out0, outA, outB := "single", "ra", "rb"
	(WorkflowStepRun{StepRunID: uuid.New().String(), RunID: runID, StepIndex: 0, StepName: "build"}).Add(context.Background())       //nolint:errcheck
	connect().Exec(`UPDATE workflow_step_runs SET status='completed', response_body=? WHERE run_id=? AND step_index=0`, out0, runID) //nolint:errcheck
	for _, o := range []string{outA, outB} {
		sid := uuid.New().String()
		(WorkflowStepRun{StepRunID: sid, RunID: runID, StepIndex: 1, StepName: "deploy"}).Add(context.Background())     //nolint:errcheck
		connect().Exec(`UPDATE workflow_step_runs SET status='completed', response_body=? WHERE step_run_id=?`, o, sid) //nolint:errcheck
	}
	steps := []WorkflowStep{
		{Step: Step{Name: "build"}},
		{Step: Step{Name: "deploy"}, Matrix: &MatrixConfig{Var: "v", Values: []string{"a", "b"}}},
	}
	outputs, completed := rebuildResumeState(context.Background(), runID, steps)
	// Completed nodes are keyed by step name (the graph's node identity), while the
	// underlying grouping stays by step index so a matrix step's legs still
	// recombine into one node rather than one node per leg.
	if !completed["build"] || !completed["deploy"] {
		t.Fatalf("completed nodes = %v", completed)
	}
	if outputs["build"] != out0 {
		t.Errorf("build output = %q", outputs["build"])
	}
	// Matrix step's outputs recombine into a JSON array (order not guaranteed).
	var arr []string
	if err := json.Unmarshal([]byte(outputs["deploy"]), &arr); err != nil || len(arr) != 2 {
		t.Errorf("deploy aggregate = %q (%v)", outputs["deploy"], err)
	}
}
