package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ── tuiPipelineStages ─────────────────────────────────────────────────────────

func TestTuiPipelineStages_GroupsConsecutiveParallel(t *testing.T) {
	g := 0
	steps := []tuiWorkflowStep{
		{Name: "build"},
		{Name: "lint", ParallelGroup: &g},
		{Name: "test", ParallelGroup: &g},
		{Name: "deploy"},
	}
	stages := tuiPipelineStages(steps)
	if len(stages) != 3 {
		t.Fatalf("stages = %d, want 3 (build | [lint,test] | deploy)", len(stages))
	}
	if len(stages[0]) != 1 || stages[0][0] != "build" {
		t.Errorf("stage0 = %v, want [build]", stages[0])
	}
	if len(stages[1]) != 2 || stages[1][0] != "lint" || stages[1][1] != "test" {
		t.Errorf("stage1 = %v, want [lint test]", stages[1])
	}
	if len(stages[2]) != 1 || stages[2][0] != "deploy" {
		t.Errorf("stage2 = %v, want [deploy]", stages[2])
	}
}

func TestTuiPipelineStages_SameGroupValueButSeparated(t *testing.T) {
	// Two steps with the same group value but a sequential step between them are
	// distinct stages — only *consecutive* same-group steps collapse.
	g, other := 0, 0
	steps := []tuiWorkflowStep{
		{Name: "a", ParallelGroup: &g},
		{Name: "b"},
		{Name: "c", ParallelGroup: &other},
	}
	stages := tuiPipelineStages(steps)
	if len(stages) != 3 {
		t.Fatalf("stages = %d, want 3 (each its own stage)", len(stages))
	}
}

func TestTuiPipelineStages_Empty(t *testing.T) {
	if got := tuiPipelineStages(nil); len(got) != 0 {
		t.Errorf("stages = %v, want empty", got)
	}
}

// ── tuiStageBox ───────────────────────────────────────────────────────────────

func TestTuiStageBox_ParallelStacksNames(t *testing.T) {
	box := tuiStageBox([]string{"lint", "test"})
	if !strings.Contains(box, "lint") || !strings.Contains(box, "test") {
		t.Errorf("parallel box should contain both names, got:\n%s", box)
	}
	// Stacked vertically: each name on its own line inside the box.
	if !strings.Contains(box, "lint") || strings.Count(box, "\n") < 3 {
		t.Errorf("parallel box should stack names on separate lines, got:\n%s", box)
	}
}

func TestTuiStageBox_TruncatesManySteps(t *testing.T) {
	box := tuiStageBox([]string{"a", "b", "c", "d", "e"})
	if !strings.Contains(box, "+3 more") {
		t.Errorf("a 5-step stage should summarise the overflow as '+3 more', got:\n%s", box)
	}
}

func TestTuiStageBox_LongNameTruncated(t *testing.T) {
	box := tuiStageBox([]string{"this-is-a-really-long-step-name"})
	if !strings.Contains(box, "…") {
		t.Errorf("an over-long step name should be truncated with …, got:\n%s", box)
	}
}

// ── tuiPipelineDiagram ────────────────────────────────────────────────────────

func TestTuiPipelineDiagram_RendersNamesAndArrows(t *testing.T) {
	g := 0
	steps := []tuiWorkflowStep{
		{Name: "build"},
		{Name: "lint", ParallelGroup: &g},
		{Name: "test", ParallelGroup: &g},
		{Name: "deploy"},
	}
	d := tuiPipelineDiagram(steps, 100)
	for _, name := range []string{"build", "lint", "test", "deploy"} {
		if !strings.Contains(d, name) {
			t.Errorf("diagram should contain %q, got:\n%s", name, d)
		}
	}
	if !strings.Contains(d, "→") {
		t.Errorf("diagram should join stages with arrows, got:\n%s", d)
	}
}

func TestTuiPipelineDiagram_NarrowFallsBackToCompact(t *testing.T) {
	steps := []tuiWorkflowStep{{Name: "build"}, {Name: "test"}, {Name: "deploy"}}
	// A width far too narrow for three bordered boxes side by side forces the
	// compact one-line form, which has no box-drawing border characters.
	d := tuiPipelineDiagram(steps, 12)
	if strings.ContainsAny(d, "╭╮╰╯│") {
		t.Errorf("narrow diagram should drop the boxed form, got:\n%s", d)
	}
	if !strings.Contains(d, "build") || !strings.Contains(d, "deploy") {
		t.Errorf("compact diagram should still name the steps, got:\n%s", d)
	}
}

// ── tuiCompactFlow ────────────────────────────────────────────────────────────

func TestTuiCompactFlow_BracketsParallel(t *testing.T) {
	got := tuiCompactFlow([][]string{{"build"}, {"lint", "test"}, {"deploy"}}, 0)
	want := "build → [lint, test] → deploy"
	if got != want {
		t.Errorf("tuiCompactFlow = %q, want %q", got, want)
	}
}

// ── tuiClampHeight ────────────────────────────────────────────────────────────

func TestTuiClampHeight(t *testing.T) {
	if got := tuiClampHeight("a\nb", 4); strings.Count(got, "\n") != 3 {
		t.Errorf("clamp should pad to 4 lines (3 newlines), got %q", got)
	}
	if got := tuiClampHeight("a\nb\nc\nd\ne", 2); strings.Count(got, "\n") != 1 {
		t.Errorf("clamp should truncate to 2 lines (1 newline), got %q", got)
	}
}

// ── Fetch: tuiFetchPipelineDef ────────────────────────────────────────────────

func TestTuiFetchPipelineDef_Success(t *testing.T) {
	g := 0
	def := tuiPipelineDef{
		WorkflowID: "wf-1",
		Steps: []tuiWorkflowStep{
			{StepID: "s0", Name: "build"},
			{StepID: "s1", Name: "lint", ParallelGroup: &g},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/pipelines/wf-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(def)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg, ok := tuiFetchPipelineDef("wf-1")().(tuiPipelineDefMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiPipelineDefMsg", msg)
	}
	if msg.workflowID != "wf-1" || len(msg.steps) != 2 {
		t.Errorf("msg = %+v, want wf-1 with 2 steps", msg)
	}
}

func TestTuiFetchPipelineDef_ErrorYieldsEmptyDef(t *testing.T) {
	// A failed definition fetch must not surface as a tuiErrMsg (which would take
	// over the whole pipelines view); it degrades to an empty-steps message.
	srv, _ := recordingServer(t, http.StatusNotFound, `{"error":"nope"}`)
	setupCLI(t, srv)

	msg, ok := tuiFetchPipelineDef("wf-x")().(tuiPipelineDefMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiPipelineDefMsg even on error", msg)
	}
	if msg.workflowID != "wf-x" || msg.steps != nil {
		t.Errorf("msg = %+v, want wf-x with nil steps", msg)
	}
}

// ── Model: diagram wiring ─────────────────────────────────────────────────────

func TestTUIModel_PipelinesMsg_RequestsDiagramFetch(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"workflow_id":"wf-1","steps":[]}`)
	setupCLI(t, srv)

	m := newTUIModel()
	_, cmd := m.Update(tuiPipelinesMsg([]tuiPipeline{{WorkflowID: "wf-1", Name: "build"}}))
	if cmd == nil {
		t.Fatal("loading pipelines should request the highlighted pipeline's diagram")
	}
	cmd()
	if rec.Path != "/workflows/pipelines/wf-1" {
		t.Errorf("diagram fetch path = %q, want /workflows/pipelines/wf-1", rec.Path)
	}
}

func TestTUIModel_PipelineDefMsg_Caches(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelineDefMsg{
		workflowID: "wf-1",
		steps:      []tuiWorkflowStep{{Name: "build"}},
	})
	if len(m.pipeDefs["wf-1"]) != 1 {
		t.Errorf("pipeDefs[wf-1] = %v, want 1 cached step", m.pipeDefs["wf-1"])
	}
}

func TestTuiEnsureDiagram_SkipsWhenCached(t *testing.T) {
	m := newTUIModel()
	m.pipelines = []tuiPipeline{{WorkflowID: "wf-1"}}
	m.pipeDefs["wf-1"] = []tuiWorkflowStep{} // cached (fetched, no steps)
	if cmd := m.tuiEnsureDiagram(); cmd != nil {
		t.Error("a cached pipeline definition should not be refetched")
	}
}

// ── View: diagram panel ───────────────────────────────────────────────────────

func TestTUIView_Pipelines_ShowsDiagram(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "deploy-flow"},
	}))
	m.pipeDefs["wf-1"] = []tuiWorkflowStep{{Name: "build"}, {Name: "deploy"}}
	view := m.View()
	if !strings.Contains(view, "pipeline flow") {
		t.Errorf("pipelines view should caption the flow diagram, got:\n%s", view)
	}
	if !strings.Contains(view, "build") || !strings.Contains(view, "deploy") {
		t.Errorf("pipelines view should render the pipeline's steps, got:\n%s", view)
	}
}

func TestTUIView_Pipelines_DiagramLoadingPlaceholder(t *testing.T) {
	// Highlighted pipeline whose definition has not been fetched yet.
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "build"},
	}))
	if !strings.Contains(m.View(), "loading") {
		t.Errorf("diagram should show a loading placeholder before the def arrives, got:\n%s", m.View())
	}
}

func TestTUIView_Pipelines_DiagramNoSteps(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "empty"},
	}))
	m.pipeDefs["wf-1"] = []tuiWorkflowStep{}
	if !strings.Contains(m.View(), "no steps") {
		t.Errorf("a pipeline with no steps should say so in the diagram, got:\n%s", m.View())
	}
}

// Sanity: the diagram panel occupies exactly its reserved height so the list
// layout does not shift as the cursor moves between pipelines.
func TestTuiPipelineDiagramPanel_FixedHeight(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{{WorkflowID: "wf-1", Name: "p"}}))
	g := 0
	m.pipeDefs["wf-1"] = []tuiWorkflowStep{
		{Name: "a"}, {Name: "b", ParallelGroup: &g}, {Name: "c", ParallelGroup: &g}, {Name: "d"},
	}
	panel := m.tuiPipelineDiagramPanel()
	if got := strings.Count(panel, "\n") + 1; got != tuiDiagReserve {
		t.Errorf("diagram panel = %d lines, want exactly %d", got, tuiDiagReserve)
	}
}

// Ensure the panel is also clamped when nothing is highlighted (defensive).
func TestTuiPipelineDiagramPanel_EmptyStillReserved(t *testing.T) {
	m := newTUIModel()
	if got := strings.Count(m.tuiPipelineDiagramPanel(), "\n") + 1; got != tuiDiagReserve {
		t.Errorf("empty diagram panel = %d lines, want %d", got, tuiDiagReserve)
	}
}

// ── Live run diagram ──────────────────────────────────────────────────────────

func TestTuiStatusGlyph(t *testing.T) {
	cases := map[string]string{
		"completed": "✓", "running": "●", "failed": "✗", "cancelled": "⊘",
		"pending": "○", "": "○", "weird": "○",
	}
	for status, want := range cases {
		if got := tuiStatusGlyph(status); got != want {
			t.Errorf("tuiStatusGlyph(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestTuiStageStatus_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"all completed", []string{"completed", "completed"}, "completed"},
		{"failed wins", []string{"completed", "failed", "running"}, "failed"},
		{"running over pending", []string{"pending", "running"}, "running"},
		{"pending over completed", []string{"completed", "pending"}, "pending"},
		{"cancelled over completed", []string{"completed", "cancelled"}, "cancelled"},
	}
	for _, c := range cases {
		batch := make([]tuiStepRun, len(c.statuses))
		for i, s := range c.statuses {
			batch[i] = tuiStepRun{Status: s}
		}
		if got := tuiStageStatus(batch); got != c.want {
			t.Errorf("%s: tuiStageStatus = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTuiRunBatches_GroupsParallel(t *testing.T) {
	g := 0
	batches := tuiRunBatches([]tuiStepRun{
		{StepName: "build"},
		{StepName: "lint", ParallelGroup: &g},
		{StepName: "test", ParallelGroup: &g},
		{StepName: "deploy"},
	})
	if len(batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(batches))
	}
	if len(batches[1]) != 2 {
		t.Errorf("middle batch = %d steps, want 2 (parallel)", len(batches[1]))
	}
}

func TestTuiFillPendingSteps_FillsActiveRun(t *testing.T) {
	def := []tuiWorkflowStep{{Name: "build"}, {Name: "test"}, {Name: "deploy"}}
	r := &tuiRunFull{
		tuiRun:   tuiRun{RunID: "r1", Status: "running"},
		StepRuns: []tuiStepRun{{StepIndex: 0, StepName: "build", Status: "completed"}},
	}
	tuiFillPendingSteps(r, def)
	if len(r.StepRuns) != 3 {
		t.Fatalf("active run should synthesise the steps ahead, got %d step runs", len(r.StepRuns))
	}
	for _, sr := range r.StepRuns {
		if sr.StepIndex > 0 && sr.Status != "pending" {
			t.Errorf("unreached step %d status = %q, want pending", sr.StepIndex, sr.Status)
		}
	}
}

func TestTuiFillPendingSteps_SkipsTerminalRun(t *testing.T) {
	def := []tuiWorkflowStep{{Name: "build"}, {Name: "test"}, {Name: "deploy"}}
	for _, status := range []string{"failed", "cancelled", "completed"} {
		r := &tuiRunFull{
			tuiRun:   tuiRun{RunID: "r1", Status: status},
			StepRuns: []tuiStepRun{{StepIndex: 0, StepName: "build", Status: "failed"}},
		}
		tuiFillPendingSteps(r, def)
		if len(r.StepRuns) != 1 {
			t.Errorf("%s run must not gain synthetic pending steps for steps it never reached, got %d step runs", status, len(r.StepRuns))
		}
	}
}

func TestTuiRunDiagram_RendersGlyphsNamesArrows(t *testing.T) {
	d := tuiRunDiagram([]tuiStepRun{
		{StepName: "build", Status: "completed"},
		{StepName: "deploy", Status: "running"},
	}, 100)
	for _, want := range []string{"build", "deploy", "✓", "●", "→"} {
		if !strings.Contains(d, want) {
			t.Errorf("live diagram should contain %q, got:\n%s", want, d)
		}
	}
}

func TestTuiRunDiagram_Empty(t *testing.T) {
	if !strings.Contains(tuiRunDiagram(nil, 100), "no steps") {
		t.Error("empty run diagram should say '(no steps)'")
	}
}

func TestTuiRunDiagram_NarrowFallsBackToCompact(t *testing.T) {
	d := tuiRunDiagram([]tuiStepRun{
		{StepName: "build", Status: "completed"},
		{StepName: "test", Status: "running"},
		{StepName: "deploy", Status: "pending"},
	}, 12)
	if strings.ContainsAny(d, "╭╮╰╯") {
		t.Errorf("narrow live diagram should drop the boxed form, got:\n%s", d)
	}
	if !strings.Contains(d, "build") || !strings.Contains(d, "deploy") {
		t.Errorf("compact live diagram should still name the steps, got:\n%s", d)
	}
}

// ── Fetch: tuiFetchRunPreview ─────────────────────────────────────────────────

func TestTuiFetchRunPreview_Success(t *testing.T) {
	// No workflow_id on the run, so parallel-group annotation is skipped and only
	// the run GET is needed.
	run := tuiRunFull{
		tuiRun:   tuiRun{RunID: "run-1", Status: "running"},
		StepRuns: []tuiStepRun{{StepIndex: 0, StepName: "build", Status: "completed"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(run)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg, ok := tuiFetchRunPreview("run-1", nil)().(tuiRunDiagramMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiRunDiagramMsg", msg)
	}
	if msg.runID != "run-1" || msg.detail == nil || len(msg.detail.StepRuns) != 1 {
		t.Errorf("msg = %+v, want run-1 with 1 step", msg)
	}
}

func TestTuiFetchRunPreview_ErrorYieldsEmptyDetail(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusNotFound, `{"error":"nope"}`)
	setupCLI(t, srv)

	msg, ok := tuiFetchRunPreview("run-x", nil)().(tuiRunDiagramMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiRunDiagramMsg even on error", msg)
	}
	if msg.runID != "run-x" || msg.detail != nil {
		t.Errorf("msg = %+v, want run-x with nil detail", msg)
	}
}

// ── Model: live diagram wiring ────────────────────────────────────────────────

func TestTUIModel_RunsMsg_RequestsRunDiagramFetch(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"run_id":"run-1","status":"running","step_runs":[]}`)
	setupCLI(t, srv)

	m := newTUIModel()
	m.view = tuiViewRuns
	_, cmd := m.Update(tuiRunsMsg([]tuiRun{{RunID: "run-1", Status: "running"}}))
	if cmd == nil {
		t.Fatal("loading runs should request the highlighted run's live diagram")
	}
	cmd()
	if rec.Path != "/workflows/runs/run-1" {
		t.Errorf("diagram fetch path = %q, want /workflows/runs/run-1", rec.Path)
	}
}

func TestTUIModel_RunDiagramMsg_Caches(t *testing.T) {
	detail := &tuiRunFull{tuiRun: tuiRun{RunID: "run-1"}, StepRuns: []tuiStepRun{{StepName: "build"}}}
	m := applyMsg(newTUIModel(), tuiRunDiagramMsg{runID: "run-1", detail: detail})
	if m.runDetails["run-1"] == nil || len(m.runDetails["run-1"].StepRuns) != 1 {
		t.Errorf("runDetails[run-1] = %v, want cached detail with 1 step", m.runDetails["run-1"])
	}
}

func TestTuiEnsureRunDiagram_FetchesUncached(t *testing.T) {
	m := newTUIModel()
	m.runs = []tuiRun{{RunID: "run-1", Status: "completed"}}
	if m.tuiEnsureRunDiagram(false) == nil {
		t.Error("an uncached run should be fetched")
	}
}

func TestTuiEnsureRunDiagram_SkipsCachedCompleted(t *testing.T) {
	m := newTUIModel()
	m.runs = []tuiRun{{RunID: "run-1", Status: "completed"}}
	m.runDetails["run-1"] = &tuiRunFull{}
	if m.tuiEnsureRunDiagram(true) != nil {
		t.Error("a cached completed run should not be refetched")
	}
}

func TestTuiEnsureRunDiagram_RefetchesActiveOnTick(t *testing.T) {
	m := newTUIModel()
	m.runs = []tuiRun{{RunID: "run-1", Status: "running"}}
	m.runDetails["run-1"] = &tuiRunFull{}
	if m.tuiEnsureRunDiagram(true) == nil {
		t.Error("an active run should be refetched on the auto-refresh tick")
	}
	if m.tuiEnsureRunDiagram(false) != nil {
		t.Error("plain cursor movement should not refetch a cached active run")
	}
}

// ── View: live diagram ────────────────────────────────────────────────────────

func TestTUIView_Runs_ShowsLiveDiagram(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m.loading = false
	m = applyMsg(m, tuiRunsMsg([]tuiRun{{RunID: "run-abcdef12", Status: "running"}}))
	m.runDetails["run-abcdef12"] = &tuiRunFull{
		tuiRun: tuiRun{RunID: "run-abcdef12", Status: "running"},
		StepRuns: []tuiStepRun{
			{StepName: "build", Status: "completed"},
			{StepName: "deploy", Status: "running"},
		},
	}
	view := m.View()
	if !strings.Contains(view, "live pipeline") {
		t.Errorf("runs view should caption the live pipeline diagram, got:\n%s", view)
	}
	for _, want := range []string{"build", "deploy"} {
		if !strings.Contains(view, want) {
			t.Errorf("runs view live diagram should render %q, got:\n%s", want, view)
		}
	}
}

func TestTUIView_RunDetail_ShowsLiveDiagram(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m = applyMsg(m, tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "r", Status: "running"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "build", Status: "completed"},
			{StepIndex: 1, StepName: "deploy", Status: "running"},
		},
	}))
	if !strings.Contains(m.View(), "live pipeline") {
		t.Errorf("run-detail view should render the live pipeline diagram, got:\n%s", m.View())
	}
}

func TestTuiRunDiagramPanel_FixedHeight(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m = applyMsg(m, tuiRunsMsg([]tuiRun{{RunID: "run-1", Status: "running"}}))
	g := 0
	m.runDetails["run-1"] = &tuiRunFull{
		StepRuns: []tuiStepRun{
			{StepName: "a", Status: "completed"},
			{StepName: "b", Status: "running", ParallelGroup: &g},
			{StepName: "c", Status: "pending", ParallelGroup: &g},
			{StepName: "d", Status: "pending"},
		},
	}
	if got := strings.Count(m.tuiRunDiagramPanel(), "\n") + 1; got != tuiDiagReserve {
		t.Errorf("run diagram panel = %d lines, want exactly %d", got, tuiDiagReserve)
	}
}

// ── inline steps (DSL guard + -f round-trip) ───────────────────────────────────

// The one-line DSL only round-trips stored-step references (and @repo). Inline
// steps, gates, matrices, and wired `with` overrides must be flagged non-expressible
// so the TUI refuses to lossily edit them via the DSL.
func TestStepDSLExpressible(t *testing.T) {
	g := 0
	cases := []struct {
		name string
		s    tuiWorkflowStep
		ok   bool
	}{
		{"stored reference", tuiWorkflowStep{StepID: "s1", Name: "build"}, true},
		{"reference with @repo only", tuiWorkflowStep{StepID: "s1", Name: "build", With: map[string]any{"secret_refs": map[string]any{gitCloneEnv: "git:https://x/r.git"}}}, true},
		{"inline step", tuiWorkflowStep{Name: "build", Action: "forge/run", With: map[string]any{"image": "alpine"}}, false},
		{"approval gate", tuiWorkflowStep{Approval: &approvalGate{Message: "ok?"}}, false},
		{"matrix", tuiWorkflowStep{StepID: "s1", Name: "build", Matrix: &matrixConfig{Var: "v", Values: []string{"a"}}}, false},
		{"wired with override", tuiWorkflowStep{StepID: "s1", Name: "build", With: map[string]any{"env": map[string]any{"X": "${steps.a.output}"}}}, false},
		{"parallel reference", tuiWorkflowStep{StepID: "s1", Name: "build", ParallelGroup: &g}, true},
	}
	for _, c := range cases {
		if stepDSLExpressible(c.s) != c.ok {
			t.Errorf("%s: stepDSLExpressible = %v, want %v", c.name, !c.ok, c.ok)
		}
	}
}

// A -f pipeline file with an inline step must round-trip its action/name/with/timeout
// into workflowStepRef (a regression guard: without the fields they were dropped).
func TestPipelineFile_InlineStepRoundTrip(t *testing.T) {
	raw := `{"name":"p","steps":[
		{"step_id":"s1"},
		{"action":"forge/run","name":"build","timeout":90,"with":{"image":"alpine","run":"make"}}
	]}`
	var pf pipelineFile
	if err := json.Unmarshal([]byte(raw), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(pf.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(pf.Steps))
	}
	in := pf.Steps[1]
	if in.StepID != "" || in.Action != "forge/run" || in.Name != "build" || in.Timeout != 90 {
		t.Errorf("inline ref not preserved: %+v", in)
	}
	if in.With["image"] != "alpine" || in.With["run"] != "make" {
		t.Errorf("inline with not preserved: %+v", in.With)
	}
	// It must re-marshal without a step_id (so the backend reads it as inline).
	out, _ := json.Marshal(in)
	if strings.Contains(string(out), "step_id") {
		t.Errorf("inline ref marshalled with a step_id: %s", out)
	}
}
