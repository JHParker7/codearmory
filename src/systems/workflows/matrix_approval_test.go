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
	}
	for _, c := range cases {
		msg := validateMatrix(&c.m)
		if (msg == "") != c.ok {
			t.Errorf("%s: validateMatrix msg=%q ok=%v", c.name, msg, c.ok)
		}
	}
}

func TestValidateStepRefShape_MatrixParallelExclusive(t *testing.T) {
	grp := 0
	ref := WorkflowStepRef{StepID: uuid.New().String(), ParallelGroup: &grp, Matrix: &MatrixConfig{Var: "v", Values: []string{"a"}}}
	if msg := validateStepRefShape(0, ref); msg == "" {
		t.Fatal("expected matrix+parallel_group to be rejected")
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
	newWorkerPool().executeRun(context.Background(), run.RunID, wf.WorkflowID, "", "", "tu", run.Inputs)

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
	newWorkerPool().executeRun(context.Background(), runID, workflowID, "", "", "tu", map[string]string{})
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
	if !completed[0] || !completed[1] {
		t.Fatalf("completed indices = %v", completed)
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
