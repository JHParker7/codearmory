package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ── Helper: tuiShortID ────────────────────────────────────────────────────────

func TestTuiShortID(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"12345678", "12345678"},
		{"123456789", "12345678"},
		{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "aaaaaaaa"},
	}
	for _, tc := range tests {
		if got := tuiShortID(tc.in); got != tc.want {
			t.Errorf("tuiShortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── Helper: tuiTrunc ──────────────────────────────────────────────────────────

func TestTuiTrunc(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello w…"},
		{"ab", 1, "a"},
		{"ab", 2, "ab"},
	}
	for _, tc := range tests {
		if got := tuiTrunc(tc.in, tc.max); got != tc.want {
			t.Errorf("tuiTrunc(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// ── Helper: tuiFormatTime ─────────────────────────────────────────────────────

func TestTuiFormatTime_Nil(t *testing.T) {
	if got := tuiFormatTime(nil); got != "—" {
		t.Errorf("tuiFormatTime(nil) = %q, want —", got)
	}
}

func TestTuiFormatTime_NonNil(t *testing.T) {
	ts := time.Date(2024, 3, 15, 9, 30, 0, 0, time.UTC)
	got := tuiFormatTime(&ts)
	if got == "—" || got == "" {
		t.Errorf("tuiFormatTime(non-nil) = %q, want a formatted time string", got)
	}
}

// ── Helper: tuiFormatDur ──────────────────────────────────────────────────────

func TestTuiFormatDur_NilStart(t *testing.T) {
	if got := tuiFormatDur(nil, nil); got != "—" {
		t.Errorf("tuiFormatDur(nil, nil) = %q, want —", got)
	}
}

func TestTuiFormatDur_Seconds(t *testing.T) {
	start := time.Now().Add(-45 * time.Second)
	end := time.Now()
	got := tuiFormatDur(&start, &end)
	if got == "—" || !strings.HasSuffix(got, "s") {
		t.Errorf("tuiFormatDur(-45s) = %q, want something like 45s", got)
	}
}

func TestTuiFormatDur_Minutes(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	end := time.Now()
	got := tuiFormatDur(&start, &end)
	if !strings.Contains(got, "m") {
		t.Errorf("tuiFormatDur(-90s) = %q, want a value containing 'm'", got)
	}
}

func TestTuiFormatDur_NilEnd_UsesNow(t *testing.T) {
	start := time.Now().Add(-10 * time.Second)
	if got := tuiFormatDur(&start, nil); got == "—" {
		t.Error("tuiFormatDur with nil end returned — instead of elapsed time")
	}
}

// ── Model: initial state ──────────────────────────────────────────────────────

func TestTUIModel_InitialState(t *testing.T) {
	m := newTUIModel()
	if m.view != tuiViewPipelines {
		t.Errorf("initial view = %v, want tuiViewPipelines", m.view)
	}
	if !m.loading {
		t.Error("loading should be true on startup")
	}
	if m.err != nil {
		t.Errorf("initial err = %v, want nil", m.err)
	}
	if m.pipelines != nil {
		t.Errorf("initial pipelines should be nil, got %v", m.pipelines)
	}
}

// ── Model: pipelinesMsg ───────────────────────────────────────────────────────

func TestTUIModel_PipelinesMsg_Populates(t *testing.T) {
	m := newTUIModel()
	ps := []tuiPipeline{
		{WorkflowID: "wf-1", Name: "build", Description: "builds stuff", Active: true},
		{WorkflowID: "wf-2", Name: "deploy", Active: false},
	}
	m2 := applyMsg(m, tuiPipelinesMsg(ps))

	if m2.loading {
		t.Error("loading should be false after pipelinesMsg")
	}
	if len(m2.pipelines) != 2 {
		t.Fatalf("len(pipelines) = %d, want 2", len(m2.pipelines))
	}
	if m2.pipelines[0].WorkflowID != "wf-1" {
		t.Errorf("pipelines[0].WorkflowID = %q, want wf-1", m2.pipelines[0].WorkflowID)
	}
}

func TestTUIModel_PipelinesMsg_Empty(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{}))
	if m.loading {
		t.Error("loading should be false even for empty list")
	}
}

// ── Model: runsMsg ────────────────────────────────────────────────────────────

func TestTUIModel_RunsMsg_Populates(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	runs := []tuiRun{
		{RunID: "run-aaa", Status: "completed"},
		{RunID: "run-bbb", Status: "running"},
	}
	m2 := applyMsg(m, tuiRunsMsg(runs))

	if m2.loading {
		t.Error("loading should be false after runsMsg")
	}
	if len(m2.runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(m2.runs))
	}
	if m2.runs[1].Status != "running" {
		t.Errorf("runs[1].Status = %q, want running", m2.runs[1].Status)
	}
}

// ── Model: runDetailMsg ───────────────────────────────────────────────────────

func TestTUIModel_RunDetailMsg_Completed_NoTick(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	detail := tuiRunFull{
		tuiRun: tuiRun{RunID: "run-abc", Status: "completed"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "build", Status: "completed"},
		},
	}
	updated, cmd := m.Update(tuiRunDetailMsg(detail))
	m2 := updated.(tuiModel)

	if m2.loading {
		t.Error("loading should be false")
	}
	if m2.runFull == nil {
		t.Fatal("runFull should not be nil")
	}
	if m2.runFull.Status != "completed" {
		t.Errorf("status = %q, want completed", m2.runFull.Status)
	}
	if len(m2.runFull.StepRuns) != 1 {
		t.Errorf("step runs = %d, want 1", len(m2.runFull.StepRuns))
	}
	if cmd != nil {
		t.Error("completed run should not emit a tick cmd")
	}
}

func TestTUIModel_RunDetailMsg_Running_EmitsTick(t *testing.T) {
	m := newTUIModel()
	_, cmd := m.Update(tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "run-live", Status: "running"},
	}))
	if cmd == nil {
		t.Error("running run should emit a tick cmd for auto-refresh")
	}
}

func TestTUIModel_RunDetailMsg_Pending_EmitsTick(t *testing.T) {
	m := newTUIModel()
	_, cmd := m.Update(tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "run-pend", Status: "pending"},
	}))
	if cmd == nil {
		t.Error("pending run should emit a tick cmd for auto-refresh")
	}
}

// ── Model: errMsg ─────────────────────────────────────────────────────────────

func TestTUIModel_ErrMsg(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiErrMsg{err: fmt.Errorf("connection refused")})
	if m.err == nil {
		t.Fatal("error should be set")
	}
	if m.loading {
		t.Error("loading should be false after errMsg")
	}
	if !strings.Contains(m.err.Error(), "connection refused") {
		t.Errorf("err = %v, should contain 'connection refused'", m.err)
	}
}

// ── Model: window resize ──────────────────────────────────────────────────────

func TestTUIModel_WindowResize(t *testing.T) {
	m := applyMsg(newTUIModel(), tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.vp.Width != 116 {
		t.Errorf("vp.Width = %d, want 116 (120-4)", m.vp.Width)
	}
	if m.vp.Height != 32 {
		t.Errorf("vp.Height = %d, want 32 (40-8)", m.vp.Height)
	}
}

// ── Model: key — quit ─────────────────────────────────────────────────────────

func TestTUIModel_QuitKey_AllViews(t *testing.T) {
	views := []tuiViewID{tuiViewPipelines, tuiViewRuns, tuiViewRunDetail, tuiViewOutput}
	for _, v := range views {
		m := newTUIModel()
		m.view = v
		m.loading = false
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		if cmd == nil {
			t.Errorf("view %d: q key returned nil cmd", v)
			continue
		}
		if _, ok := cmd().(goHomeMsg); !ok {
			t.Errorf("view %d: q key cmd returned %T, want goHomeMsg", v, cmd())
		}
	}
}

func TestTUIModel_CtrlC_Quits(t *testing.T) {
	m := newTUIModel()
	m.loading = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c returned nil cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return QuitMsg")
	}
}

// ── Model: key — pipelines view navigation ────────────────────────────────────

func TestTUIModel_Pipelines_EnterNavigatesToRuns(t *testing.T) {
	// Populate the model via message (also tests the message path).
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "build"},
	}))

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)

	if m2.view != tuiViewRuns {
		t.Errorf("view = %v, want tuiViewRuns", m2.view)
	}
	if !m2.loading {
		t.Error("loading should be true while fetching runs")
	}
	if m2.selPipeline == nil || m2.selPipeline.WorkflowID != "wf-1" {
		t.Errorf("selPipeline = %v, want wf-1", m2.selPipeline)
	}
	if cmd == nil {
		t.Error("enter should return a fetch cmd")
	}
}

func TestTUIModel_Pipelines_EnterNoop_WhenEmpty(t *testing.T) {
	// No pipelines loaded; enter should not change view.
	m := newTUIModel()
	m.loading = false
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewPipelines {
		t.Errorf("view changed to %v on enter with no pipelines", m2.view)
	}
}

func TestTUIModel_Pipelines_RRefreshes(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r key should emit a refresh cmd")
	}
}

// ── Model: key — runs view navigation ────────────────────────────────────────

func TestTUIModel_Runs_BackReturns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m.loading = false

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewPipelines {
		t.Errorf("b key: view = %v, want tuiViewPipelines", m2.view)
	}
	if cmd != nil {
		t.Error("back key should not emit a cmd")
	}
}

func TestTUIModel_Runs_EscReturns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m.loading = false
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(tuiModel).view != tuiViewPipelines {
		t.Error("esc should return to pipelines view")
	}
}

func TestTUIModel_Runs_EnterNavigatesToDetail(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m = applyMsg(m, tuiRunsMsg([]tuiRun{{RunID: "run-aaa", Status: "completed"}}))

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)

	if m2.view != tuiViewRunDetail {
		t.Errorf("view = %v, want tuiViewRunDetail", m2.view)
	}
	if m2.selRun == nil || m2.selRun.RunID != "run-aaa" {
		t.Errorf("selRun = %v, want run-aaa", m2.selRun)
	}
	if cmd == nil {
		t.Error("enter should emit a fetch cmd")
	}
}

func TestTUIModel_Runs_CancelKey_NonCancellable_Noop(t *testing.T) {
	// Pressing c on a completed run is a no-op: the cancel branch is skipped, so
	// no cancel/fetch cmd is emitted (and thus no HTTP call is made).
	m := newTUIModel()
	m.view = tuiViewRuns
	m = applyMsg(m, tuiRunsMsg([]tuiRun{{RunID: "run-done", Status: "completed"}}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if cmd != nil {
		t.Error("c on a completed run should not emit a cmd")
	}
}

func TestTUIModel_Runs_CancelKey_PendingRun(t *testing.T) {
	mux := http.NewServeMux()
	var deleteCalled bool
	mux.HandleFunc("/workflows/runs/run-pend", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleteCalled = true
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}")) //nolint:errcheck
	})
	mux.HandleFunc("/workflows/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]")) //nolint:errcheck
	})
	srv := routeServer(t, mux)
	setupCLI(t, srv)

	m := newTUIModel()
	m.view = tuiViewRuns
	m = applyMsg(m, tuiRunsMsg([]tuiRun{{RunID: "run-pend", Status: "pending"}}))

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})

	if !deleteCalled {
		t.Error("cancel key should have issued a DELETE request for a pending run")
	}
	if cmd == nil {
		t.Error("cancel should return a reload cmd")
	}
}

func TestTUIModel_Runs_RRefreshes(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m.loading = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r key should emit a refresh cmd in runs view")
	}
}

// ── Model: key — run detail view ─────────────────────────────────────────────

func TestTUIModel_RunDetail_BackReturns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m.loading = false
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if updated.(tuiModel).view != tuiViewRuns {
		t.Error("b in run detail should return to runs view")
	}
}

func TestTUIModel_RunDetail_EnterNavigatesToOutput(t *testing.T) {
	out := "exit code 0"
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m = applyMsg(m, tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "run-abc", Status: "completed"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "build", Status: "completed", Output: &out},
		},
	}))

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)

	if m2.view != tuiViewOutput {
		t.Errorf("view = %v, want tuiViewOutput", m2.view)
	}
	if !strings.Contains(m2.outputTitle, "build") {
		t.Errorf("outputTitle = %q, should mention 'build'", m2.outputTitle)
	}
}

func TestTUIModel_RunDetail_EnterNoOutput_ShowsDefault(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m = applyMsg(m, tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "run-abc", Status: "completed"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "init", Status: "completed", Output: nil},
		},
	}))

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(tuiModel)

	if m2.view != tuiViewOutput {
		t.Fatalf("view = %v, want tuiViewOutput", m2.view)
	}
	// viewport content should show the default "(no output)" message
	rendered := m2.vp.View()
	if !strings.Contains(rendered, "no output") {
		t.Errorf("viewport should show '(no output)', got: %q", rendered)
	}
}

func TestTUIModel_RunDetail_RRefreshes(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"run_id":"run-abc","status":"completed","step_runs":[]}`)
	setupCLI(t, srv)

	m := newTUIModel()
	m.view = tuiViewRunDetail
	m.selRun = &tuiRun{RunID: "run-abc", Status: "completed"}
	m.loading = false

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r key in run detail should emit a refresh cmd")
	}
}

// ── Model: key — output view ──────────────────────────────────────────────────

func TestTUIModel_Output_BackReturns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewOutput
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if updated.(tuiModel).view != tuiViewRunDetail {
		t.Error("b in output view should return to run detail")
	}
}

func TestTUIModel_Output_EscReturns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewOutput
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(tuiModel).view != tuiViewRunDetail {
		t.Error("esc in output view should return to run detail")
	}
}

// ── Model: tick behaviour ─────────────────────────────────────────────────────

func TestTUIModel_Tick_InRunDetailView_Fetches(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"run_id":"run-xyz","status":"running","step_runs":[]}`)
	setupCLI(t, srv)

	m := newTUIModel()
	m.view = tuiViewRunDetail
	m.selRun = &tuiRun{RunID: "run-xyz"}

	_, cmd := m.Update(tuiTickMsg{})
	if cmd == nil {
		t.Fatal("tick in run-detail view should emit a fetch cmd")
	}
	// Execute the cmd to trigger the HTTP call.
	cmd()
	if rec.Method != "GET" || rec.Path != "/workflows/runs/run-xyz" {
		t.Errorf("tick: request = %s %s, want GET /workflows/runs/run-xyz", rec.Method, rec.Path)
	}
}

func TestTUIModel_Tick_OutsideRunDetailView_Noop(t *testing.T) {
	for _, v := range []tuiViewID{tuiViewPipelines, tuiViewRuns, tuiViewOutput} {
		m := newTUIModel()
		m.view = v
		_, cmd := m.Update(tuiTickMsg{})
		if cmd != nil {
			t.Errorf("view %d: tick should be a noop, got cmd %v", v, cmd)
		}
	}
}

// ── Fetch: tuiFetchPipelines ──────────────────────────────────────────────────

func TestTuiFetchPipelines_Success(t *testing.T) {
	ps := []tuiPipeline{{WorkflowID: "wf-1", Name: "build"}}
	body, _ := json.Marshal(ps)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := tuiFetchPipelines()

	if rec.Method != "GET" || rec.Path != "/workflows/pipelines" {
		t.Errorf("request = %s %s, want GET /workflows/pipelines", rec.Method, rec.Path)
	}
	result, ok := msg.(tuiPipelinesMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiPipelinesMsg", msg)
	}
	if len(result) != 1 || result[0].WorkflowID != "wf-1" {
		t.Errorf("result = %v, want [{wf-1 build}]", result)
	}
}

func TestTuiFetchPipelines_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	setupCLI(t, srv)

	msg := tuiFetchPipelines()
	if _, ok := msg.(tuiErrMsg); !ok {
		t.Errorf("msg type = %T, want tuiErrMsg", msg)
	}
}

func TestTuiFetchPipelines_InvalidJSON(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `not json`)
	setupCLI(t, srv)

	msg := tuiFetchPipelines()
	if _, ok := msg.(tuiErrMsg); !ok {
		t.Errorf("msg type = %T, want tuiErrMsg for malformed JSON", msg)
	}
}

// ── Fetch: tuiFetchRuns ───────────────────────────────────────────────────────

func TestTuiFetchRuns_AllRuns(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)

	msg := tuiFetchRuns("")()
	if _, ok := msg.(tuiRunsMsg); !ok {
		t.Fatalf("msg type = %T, want tuiRunsMsg", msg)
	}
	if rec.Path != "/workflows/runs" {
		t.Errorf("path = %q, want /workflows/runs", rec.Path)
	}
}

func TestTuiFetchRuns_WithWorkflowID(t *testing.T) {
	// Verifies the path is /workflows/runs; query param is checked in WorkflowIDInQuery.
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	tuiFetchRuns("wf-123")()
	if rec.Path != "/workflows/runs" {
		t.Errorf("path = %q, want /workflows/runs", rec.Path)
	}
}

func TestTuiFetchRuns_WorkflowIDInQuery(t *testing.T) {
	// Use routeServer to capture the full URL including query string.
	mux := http.NewServeMux()
	var gotQuery string
	mux.HandleFunc("/workflows/runs", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]")) //nolint:errcheck
	})
	srv := routeServer(t, mux)
	setupCLI(t, srv)

	tuiFetchRuns("wf-abc-123")()

	if !strings.Contains(gotQuery, "wf-abc-123") {
		t.Errorf("query = %q, want to contain wf-abc-123", gotQuery)
	}
}

func TestTuiFetchRuns_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `{"error":"forbidden"}`)
	setupCLI(t, srv)

	msg := tuiFetchRuns("")()
	if _, ok := msg.(tuiErrMsg); !ok {
		t.Errorf("msg type = %T, want tuiErrMsg", msg)
	}
}

// ── Fetch: tuiFetchRunDetail ──────────────────────────────────────────────────

func TestTuiFetchRunDetail_Success(t *testing.T) {
	out := "exit 0"
	detail := tuiRunFull{
		tuiRun: tuiRun{RunID: "run-abc", Status: "completed"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "build", Status: "completed", Output: &out},
		},
	}
	body, _ := json.Marshal(detail)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := tuiFetchRunDetail("run-abc")()

	if rec.Method != "GET" || rec.Path != "/workflows/runs/run-abc" {
		t.Errorf("request = %s %s, want GET /workflows/runs/run-abc", rec.Method, rec.Path)
	}
	result, ok := msg.(tuiRunDetailMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiRunDetailMsg", msg)
	}
	if result.RunID != "run-abc" {
		t.Errorf("RunID = %q, want run-abc", result.RunID)
	}
	if len(result.StepRuns) != 1 {
		t.Errorf("len(StepRuns) = %d, want 1", len(result.StepRuns))
	}
}

func TestTuiFetchRunDetail_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusNotFound, `{"error":"not found"}`)
	setupCLI(t, srv)

	msg := tuiFetchRunDetail("nope")()
	if _, ok := msg.(tuiErrMsg); !ok {
		t.Errorf("msg type = %T, want tuiErrMsg", msg)
	}
}

// ── View: rendering ───────────────────────────────────────────────────────────

func TestTUIView_LoadingPipelines(t *testing.T) {
	m := newTUIModel() // loading=true by default
	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Errorf("loading view should say Loading, got: %q", view)
	}
}

func TestTUIView_ErrorState(t *testing.T) {
	m := newTUIModel()
	m.err = fmt.Errorf("dial tcp: connection refused")
	view := m.View()
	if !strings.Contains(view, "error") {
		t.Errorf("error view should contain 'error', got: %q", view)
	}
	if !strings.Contains(view, "connection refused") {
		t.Errorf("error view should contain the error text, got: %q", view)
	}
}

func TestTUIView_ErrorState_401_ShowsAuthHint(t *testing.T) {
	m := newTUIModel()
	m.err = fmt.Errorf("HTTP 401: unauthorized")
	view := m.View()
	if !strings.Contains(view, "auth login") {
		t.Errorf("401 error view should hint at armory auth login, got: %q", view)
	}
}

func TestTUIModel_ErrorState_RKey_Retries(t *testing.T) {
	m := newTUIModel()
	m.err = fmt.Errorf("HTTP 401: unauthorized")
	m.loading = false

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m2 := updated.(tuiModel)

	if m2.err != nil {
		t.Error("r key should clear the error")
	}
	if !m2.loading {
		t.Error("r key should set loading=true")
	}
	if cmd == nil {
		t.Error("r key in error state should emit a fetch cmd")
	}
}

func TestTUIModel_ErrorState_QKey_GoesHome(t *testing.T) {
	m := newTUIModel()
	m.err = fmt.Errorf("HTTP 401: unauthorized")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q in error state should return home cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("q in error state should return goHomeMsg")
	}
}

func TestTUIView_EmptyPipelines(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{}))
	view := m.View()
	if !strings.Contains(view, "No pipelines") {
		t.Errorf("empty-pipelines view should say 'No pipelines', got: %q", view)
	}
}

func TestTUIView_EmptyRuns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m = applyMsg(m, tuiRunsMsg([]tuiRun{}))
	view := m.View()
	if !strings.Contains(view, "No runs") {
		t.Errorf("empty-runs view should say 'No runs', got: %q", view)
	}
}

func TestTUIView_RunDetailLoading(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m.loading = true
	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Errorf("run-detail loading view should say Loading, got: %q", view)
	}
}

func TestTUIView_PipelinesHelpText(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "my-pipeline"},
	}))
	view := m.View()
	if !strings.Contains(view, "enter") {
		t.Errorf("pipelines view should show keybinding hints with 'enter', got: %q", view)
	}
}

func TestTUIView_RunsHelpText(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m.loading = false
	view := m.View()
	if !strings.Contains(view, "back") || !strings.Contains(view, "cancel") {
		t.Errorf("runs view should show 'back' and 'cancel' hints, got: %q", view)
	}
}

func TestTUIView_RunDetailAutoRefreshHint(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m = applyMsg(m, tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "r", Status: "running"},
	}))
	view := m.View()
	if !strings.Contains(view, "auto-refresh") {
		t.Errorf("running run detail view should show auto-refresh hint, got: %q", view)
	}
}

func TestTUIView_RunDetailNoAutoRefreshHint_WhenDone(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m = applyMsg(m, tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "r", Status: "completed"},
	}))
	view := m.View()
	if strings.Contains(view, "auto-refresh") {
		t.Error("completed run detail should not show auto-refresh hint")
	}
}

// ── Utility ───────────────────────────────────────────────────────────────────

// applyMsg is a convenience that calls Update and returns the updated model.
func applyMsg(m tuiModel, msg tea.Msg) tuiModel {
	updated, _ := m.Update(msg)
	return updated.(tuiModel)
}
