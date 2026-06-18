package cmd

import (
	"encoding/json"
	"fmt"
	"io"
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

func TestTUIModel_RunDetailMsg_Running_NoSelfTick(t *testing.T) {
	// Auto-refresh is host-driven (one shared 5s ticker), so receiving a running
	// run's detail must not schedule a per-model tick of its own.
	m := newTUIModel()
	_, cmd := m.Update(tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "run-live", Status: "running"},
	}))
	if cmd != nil {
		t.Error("runDetailMsg should not schedule its own tick; refresh is host-driven")
	}
}

func TestTUIModel_RunDetailMsg_Pending_NoSelfTick(t *testing.T) {
	m := newTUIModel()
	_, cmd := m.Update(tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "run-pend", Status: "pending"},
	}))
	if cmd != nil {
		t.Error("runDetailMsg should not schedule its own tick; refresh is host-driven")
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

// ── Model: key — quit / home ──────────────────────────────────────────────────

// esc from the top-level pipelines view returns home; from nested views it goes
// back one level (covered by the *_EscReturns / *_Esc_Back tests).
func TestTUIModel_Pipelines_Esc_GoesHome(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewPipelines
	m.loading = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in the pipelines view should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("esc cmd returned %T, want goHomeMsg", cmd())
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

func TestTUIModel_RunDetail_EscReturns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m.loading = false
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(tuiModel).view != tuiViewRuns {
		t.Error("esc in run detail should return to runs view")
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

func TestTUIModel_Output_EscReturns(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewOutput
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(tuiModel).view != tuiViewRunDetail {
		t.Error("esc in output view should return to run detail")
	}
}

// ── Model: auto-refresh ───────────────────────────────────────────────────────

func TestTUIModel_AutoRefresh_InRunDetailView_Fetches(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"run_id":"run-xyz","status":"running","step_runs":[]}`)
	setupCLI(t, srv)

	m := newTUIModel()
	m.view = tuiViewRunDetail
	m.selRun = &tuiRun{RunID: "run-xyz"}

	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Fatal("auto-refresh in run-detail view should emit a fetch cmd")
	}
	// Execute the cmd to trigger the HTTP call.
	cmd()
	if rec.Method != "GET" || rec.Path != "/workflows/runs/run-xyz" {
		t.Errorf("auto-refresh: request = %s %s, want GET /workflows/runs/run-xyz", rec.Method, rec.Path)
	}
}

func TestTUIModel_AutoRefresh_InListViews_Fetches(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	for _, v := range []tuiViewID{tuiViewPipelines, tuiViewRuns} {
		m := newTUIModel()
		m.view = v
		_, cmd := m.Update(tuiAutoRefreshMsg{})
		if cmd == nil {
			t.Errorf("view %d: auto-refresh should emit a fetch cmd", v)
		}
	}
}

func TestTUIModel_AutoRefresh_InOutputAndForms_Noop(t *testing.T) {
	// The captured step-output snapshot and the create/run forms must not be
	// disturbed by an auto-refresh.
	for _, v := range []tuiViewID{tuiViewOutput, tuiViewCreate, tuiViewRun} {
		m := newTUIModel()
		m.view = v
		_, cmd := m.Update(tuiAutoRefreshMsg{})
		if cmd != nil {
			t.Errorf("view %d: auto-refresh should be a noop", v)
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

	msg := tuiFetchRunDetail("run-abc", nil)()

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

	msg := tuiFetchRunDetail("nope", nil)()
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

func TestTUIModel_ErrorState_EscKey_GoesHome(t *testing.T) {
	m := newTUIModel()
	m.err = fmt.Errorf("HTTP 401: unauthorized")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in error state should return home cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("esc in error state should return goHomeMsg")
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

// ── Create flow ───────────────────────────────────────────────────────────────

func TestTUIModel_Pipelines_N_OpensCreateForm(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewCreate {
		t.Errorf("view = %v, want tuiViewCreate", m2.view)
	}
	if cmd == nil {
		t.Error("opening the form should return a focus/blink cmd")
	}
	if len(m2.form.fields) == 0 {
		t.Error("create form should have fields")
	}
}

func TestTUIModel_Create_Esc_BacksToPipelines(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewCreate
	m.form, _ = newCIPipelineForm()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(tuiModel).view != tuiViewPipelines {
		t.Error("esc in create view should return to the pipelines list")
	}
}

func TestTUIModel_Create_Submit_MissingName_StaysWithError(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewCreate
	m.form, _ = newCIPipelineForm()
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewCreate {
		t.Error("submit with no name should stay in the create view")
	}
	if m2.form.errMsg == "" {
		t.Error("submit with no name should set an inline error")
	}
	if cmd != nil {
		t.Error("invalid submit should not emit a request cmd")
	}
}

func TestTUIModel_PipelineCreatedMsg_ReturnsToListAndRefetches(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewCreate
	m.submitting = true
	updated, cmd := m.Update(tuiPipelineCreatedMsg{})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewPipelines {
		t.Error("tuiPipelineCreatedMsg should return to the pipelines view")
	}
	if !m2.loading || cmd == nil {
		t.Error("tuiPipelineCreatedMsg should set loading and emit a refetch cmd")
	}
	if m2.submitting {
		t.Error("tuiPipelineCreatedMsg should clear the submitting guard")
	}
}

// A second Enter landing before the create POST completes must not fire a
// second create cmd — the submitting guard swallows it.
func TestTUIModel_Create_DoubleSubmit_OnlyCreatesOnce(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewCreate
	m.form, _ = newCIPipelineForm()
	m.form.setValues(map[string]string{"name": "my-pl", "steps": "build->test"})

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(tuiModel)
	if cmd == nil {
		t.Fatal("first create submit should emit a request cmd")
	}
	if !m2.submitting {
		t.Fatal("first create submit should set the submitting guard")
	}

	updated2, cmd2 := m2.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd2 != nil {
		t.Error("second create submit while in-flight should not emit a second cmd")
	}
	if !updated2.(tuiModel).submitting {
		t.Error("guard should stay set until the create resolves")
	}
}

func TestTUIModel_FormErrMsg_ShowsInlineError(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewCreate
	m.form, _ = newCIPipelineForm()
	updated, _ := m.Update(tuiFormErrMsg{err: fmt.Errorf("step \"build\" not found")})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewCreate {
		t.Error("form error should stay in the create view")
	}
	if !strings.Contains(m2.form.errMsg, "not found") {
		t.Errorf("form.errMsg = %q, want to mention 'not found'", m2.form.errMsg)
	}
}

func TestCISubmitCreatePipeline_ResolvesStepsAndPosts(t *testing.T) {
	mux := http.NewServeMux()
	var postBody []byte
	mux.HandleFunc("/workflows/steps", func(w http.ResponseWriter, r *http.Request) {
		// resolveStepName looks up each name and expects [{step_id,name}].
		name := r.URL.Query().Get("name")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"step_id":"id-%s","name":%q}]`, name, name)
	})
	mux.HandleFunc("/workflows/pipelines", func(w http.ResponseWriter, r *http.Request) {
		postBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"workflow_id":"wf1"}`)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg := ciSubmitCreatePipeline("my-pl", "desc", "build->test")()
	if _, ok := msg.(tuiPipelineCreatedMsg); !ok {
		t.Fatalf("msg = %T, want tuiPipelineCreatedMsg", msg)
	}

	var got map[string]any
	if err := json.Unmarshal(postBody, &got); err != nil {
		t.Fatalf("pipeline body not JSON: %v", err)
	}
	if got["name"] != "my-pl" || got["description"] != "desc" {
		t.Errorf("name/description = %v/%v, want my-pl/desc", got["name"], got["description"])
	}
	steps, _ := got["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps = %v, want 2 resolved refs", got["steps"])
	}
	first, _ := steps[0].(map[string]any)
	if first["step_id"] != "id-build" {
		t.Errorf("first step_id = %v, want id-build", first["step_id"])
	}
}

func TestCISubmitCreatePipeline_BadDSL_ReturnsFormErr(t *testing.T) {
	// An empty parallel-group segment is a DSL parse error; no HTTP needed.
	if _, ok := ciSubmitCreatePipeline("n", "", "build->[]")().(tuiFormErrMsg); !ok {
		t.Error("a malformed DSL should return tuiFormErrMsg")
	}
}

func TestTUIView_CreateView_RendersForm(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewCreate
	m.form, _ = newCIPipelineForm()
	if !strings.Contains(m.View(), "New Pipeline") {
		t.Error("create view should show the form heading")
	}
}

func TestTUIView_PipelinesHelp_MentionsNew(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{}))
	if !strings.Contains(m.View(), "new") {
		t.Error("pipelines help should mention the new-pipeline shortcut")
	}
}

// ── Run (manual trigger) flow ─────────────────────────────────────────────────

func TestTUIModel_Pipelines_R_OpensRunForm(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "build"},
	}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewRun {
		t.Errorf("view = %v, want tuiViewRun", m2.view)
	}
	if m2.selPipeline == nil || m2.selPipeline.WorkflowID != "wf-1" {
		t.Errorf("selPipeline = %v, want wf-1", m2.selPipeline)
	}
	if m2.runReturn != tuiViewPipelines {
		t.Errorf("runReturn = %v, want tuiViewPipelines", m2.runReturn)
	}
	if cmd == nil {
		t.Error("opening the run form should return a focus/blink cmd")
	}
	if len(m2.form.fields) == 0 {
		t.Error("run form should have fields")
	}
}

func TestTUIModel_Pipelines_R_Noop_WhenEmpty(t *testing.T) {
	m := newTUIModel()
	m.loading = false
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if updated.(tuiModel).view != tuiViewPipelines {
		t.Error("R with no pipelines should stay in the pipelines view")
	}
}

func TestTUIModel_Runs_R_OpensRunForm(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRuns
	m.selPipeline = &tuiPipeline{WorkflowID: "wf-9", Name: "deploy"}
	m.loading = false
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewRun {
		t.Errorf("view = %v, want tuiViewRun", m2.view)
	}
	if m2.runReturn != tuiViewRuns {
		t.Errorf("runReturn = %v, want tuiViewRuns (so cancel returns to runs)", m2.runReturn)
	}
}

func TestTUIModel_Run_Esc_ReturnsToOrigin(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "build"},
	}))
	m = applyMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(tuiModel).view != tuiViewPipelines {
		t.Error("esc in the run form should return to the originating view")
	}
}

func TestTUIModel_Run_Submit_NoPipeline_SetsError(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRun
	m.form, _ = newCIRunForm("x")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(tuiModel)
	if m2.form.errMsg == "" {
		t.Error("submit with no selected pipeline should set an inline error")
	}
	if cmd != nil {
		t.Error("invalid submit should not emit a request cmd")
	}
}

func TestTUIModel_RunTriggeredMsg_NavigatesToRuns(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)

	m := newTUIModel()
	m.view = tuiViewRun
	m.submitting = true
	m.selPipeline = &tuiPipeline{WorkflowID: "wf-1"}
	updated, cmd := m.Update(tuiRunTriggeredMsg{})
	m2 := updated.(tuiModel)
	if m2.view != tuiViewRuns {
		t.Errorf("view = %v, want tuiViewRuns after a run is triggered", m2.view)
	}
	if !m2.loading || cmd == nil {
		t.Error("tuiRunTriggeredMsg should set loading and emit a fetch-runs cmd")
	}
	if m2.submitting {
		t.Error("tuiRunTriggeredMsg should clear the submitting guard")
	}
}

// Pressing Enter twice quickly on the run form must trigger exactly one run:
// the second submit is swallowed while the first POST is still in flight.
func TestTUIModel_Run_DoubleSubmit_OnlyTriggersOnce(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRun
	m.selPipeline = &tuiPipeline{WorkflowID: "wf-1"}
	m.form, _ = newCIRunForm("build")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(tuiModel)
	if cmd == nil {
		t.Fatal("first run submit should emit a trigger cmd")
	}
	if !m2.submitting {
		t.Fatal("first run submit should set the submitting guard")
	}

	updated2, cmd2 := m2.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd2 != nil {
		t.Error("second run submit while in-flight should not emit a second trigger cmd")
	}
	if m2.form.errMsg != updated2.(tuiModel).form.errMsg {
		t.Error("the swallowed submit should not alter the form error state")
	}
}

// Reopening the run form clears any stale guard so a fresh form is submittable
// even if a prior submit's terminal message was never delivered.
func TestTUIModel_OpenRunForm_ClearsStaleSubmitting(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{{WorkflowID: "wf-1", Name: "build"}}))
	m.submitting = true
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if updated.(tuiModel).submitting {
		t.Error("opening the run form should clear a stale submitting guard")
	}
}

func TestParseRunInputs(t *testing.T) {
	tests := []struct {
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"", map[string]string{}, false},
		{"   ", map[string]string{}, false},
		{"FOO=bar", map[string]string{"FOO": "bar"}, false},
		{"A=1 B=2", map[string]string{"A": "1", "B": "2"}, false},
		{"URL=http://x?a=b", map[string]string{"URL": "http://x?a=b"}, false},
		{"=novalue", nil, true},
		{"novalue", nil, true},
	}
	for _, tc := range tests {
		got, err := parseRunInputs(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRunInputs(%q): want error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRunInputs(%q): unexpected error %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseRunInputs(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("parseRunInputs(%q)[%q] = %q, want %q", tc.in, k, got[k], v)
			}
		}
	}
}

func TestCISubmitRunPipeline_PostsInputs(t *testing.T) {
	mux := http.NewServeMux()
	var (
		postBody []byte
		gotPath  string
	)
	mux.HandleFunc("/workflows/pipelines/wf-1/runs", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		postBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"run_id":"run-1"}`)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg := ciSubmitRunPipeline("wf-1", map[string]string{"FOO": "bar"})()
	if _, ok := msg.(tuiRunTriggeredMsg); !ok {
		t.Fatalf("msg = %T, want tuiRunTriggeredMsg", msg)
	}
	if gotPath != "/workflows/pipelines/wf-1/runs" {
		t.Errorf("path = %q, want /workflows/pipelines/wf-1/runs", gotPath)
	}
	var got map[string]any
	if err := json.Unmarshal(postBody, &got); err != nil {
		t.Fatalf("run body not JSON: %v", err)
	}
	inputs, _ := got["inputs"].(map[string]any)
	if inputs["FOO"] != "bar" {
		t.Errorf("inputs = %v, want FOO=bar", got["inputs"])
	}
}

func TestCISubmitRunPipeline_HTTPError_ReturnsFormErr(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	setupCLI(t, srv)
	if _, ok := ciSubmitRunPipeline("wf-1", nil)().(tuiFormErrMsg); !ok {
		t.Error("a failed trigger should return tuiFormErrMsg")
	}
}

func TestTUIView_RunForm_RendersTitle(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRun
	m.form, _ = newCIRunForm("my-pipeline")
	if !strings.Contains(m.View(), "Run Pipeline: my-pipeline") {
		t.Error("run view should show the run-form heading with the pipeline name")
	}
}

func TestTUIView_PipelinesHelp_MentionsRun(t *testing.T) {
	m := applyMsg(newTUIModel(), tuiPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "my-pipeline"},
	}))
	if !strings.Contains(m.View(), "[R] run") {
		t.Error("pipelines help should mention the [R] run shortcut")
	}
}

// ── Parallel-group grouping ───────────────────────────────────────────────────

func TestTuiStepRunRows_GroupsParallelBatch(t *testing.T) {
	g := 0
	steps := []tuiStepRun{
		{StepIndex: 0, StepName: "build", Status: "completed"},
		{StepIndex: 1, StepName: "test", Status: "completed", ParallelGroup: &g},
		{StepIndex: 2, StepName: "lint", Status: "running", ParallelGroup: &g},
		{StepIndex: 3, StepName: "scan", Status: "pending", ParallelGroup: &g},
		{StepIndex: 4, StepName: "deploy", Status: "pending"},
	}
	rows := tuiStepRunRows(steps)
	if len(rows) != len(steps) {
		t.Fatalf("rows = %d, want %d (1:1 with step runs)", len(rows), len(steps))
	}
	// Sequential build → stage 1, no bracket glyph.
	if rows[0][0] != "1" || rows[0][1] != "build" {
		t.Errorf("row0 = %q/%q, want 1/build", rows[0][0], rows[0][1])
	}
	// Parallel batch shares stage 2: number on the first row only; ┌ ├ └ brackets.
	if rows[1][0] != "2" || !strings.HasPrefix(rows[1][1], "┌ ") {
		t.Errorf("row1 = %q/%q, want stage 2 with ┌ bracket", rows[1][0], rows[1][1])
	}
	if rows[2][0] != "" || !strings.HasPrefix(rows[2][1], "├ ") {
		t.Errorf("row2 = %q/%q, want blank stage with ├ bracket", rows[2][0], rows[2][1])
	}
	if rows[3][0] != "" || !strings.HasPrefix(rows[3][1], "└ ") {
		t.Errorf("row3 = %q/%q, want blank stage with └ bracket", rows[3][0], rows[3][1])
	}
	// Sequential numbering resumes at stage 3 after the batch (not step index 5).
	if rows[4][0] != "3" || rows[4][1] != "deploy" {
		t.Errorf("row4 = %q/%q, want 3/deploy", rows[4][0], rows[4][1])
	}
}

func TestTuiStepRunRows_AllSequential_NoBrackets(t *testing.T) {
	rows := tuiStepRunRows([]tuiStepRun{
		{StepIndex: 0, StepName: "a"},
		{StepIndex: 1, StepName: "b"},
	})
	for i, r := range rows {
		if r[0] != fmt.Sprintf("%d", i+1) {
			t.Errorf("row %d stage = %q, want %d", i, r[0], i+1)
		}
		if strings.ContainsAny(r[1], "┌├└") {
			t.Errorf("row %d step %q should carry no bracket glyph", i, r[1])
		}
	}
}

func TestTuiRunHasParallel(t *testing.T) {
	g, other := 1, 1
	cases := []struct {
		name  string
		steps []tuiStepRun
		want  bool
	}{
		{"no groups", []tuiStepRun{{StepName: "a"}, {StepName: "b"}}, false},
		{"shared consecutive group", []tuiStepRun{{ParallelGroup: &g}, {ParallelGroup: &g}}, true},
		{"same value but separated", []tuiStepRun{{ParallelGroup: &g}, {StepName: "x"}, {ParallelGroup: &other}}, false},
	}
	for _, c := range cases {
		if got := tuiRunHasParallel(c.steps); got != c.want {
			t.Errorf("%s: tuiRunHasParallel = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTuiFetchRunDetail_AnnotatesParallelGroups(t *testing.T) {
	g := 0
	run := tuiRunFull{
		tuiRun: tuiRun{RunID: "run-1", WorkflowID: "wf-1", Status: "completed"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "build", Status: "completed"},
			{StepIndex: 1, StepName: "test", Status: "completed"},
			{StepIndex: 2, StepName: "lint", Status: "completed"},
		},
	}
	def := tuiPipelineDef{
		WorkflowID: "wf-1",
		Steps: []tuiWorkflowStep{
			{StepID: "s0", Name: "build"},
			{StepID: "s1", Name: "test", ParallelGroup: &g},
			{StepID: "s2", Name: "lint", ParallelGroup: &g},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(run)) //nolint:errcheck
	})
	mux.HandleFunc("/workflows/pipelines/wf-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(def)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg := tuiFetchRunDetail("run-1", nil)()
	result, ok := msg.(tuiRunDetailMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiRunDetailMsg", msg)
	}
	if result.StepRuns[0].ParallelGroup != nil {
		t.Errorf("build group = %d, want nil (sequential)", *result.StepRuns[0].ParallelGroup)
	}
	if result.StepRuns[1].ParallelGroup == nil || result.StepRuns[2].ParallelGroup == nil {
		t.Fatal("test/lint should be annotated with their parallel group from the definition")
	}
	if *result.StepRuns[1].ParallelGroup != *result.StepRuns[2].ParallelGroup {
		t.Error("test and lint should share the same parallel group")
	}
}

func TestTuiFetchRunDetail_NoDefStillSucceeds(t *testing.T) {
	// The pipeline definition is unavailable (404); the run detail must still load,
	// just without parallel grouping.
	run := tuiRunFull{
		tuiRun:   tuiRun{RunID: "run-1", WorkflowID: "wf-1", Status: "completed"},
		StepRuns: []tuiStepRun{{StepIndex: 0, StepName: "build", Status: "completed"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(run)) //nolint:errcheck
	})
	mux.HandleFunc("/workflows/pipelines/wf-1", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	setupCLI(t, routeServer(t, mux))

	msg := tuiFetchRunDetail("run-1", nil)()
	result, ok := msg.(tuiRunDetailMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiRunDetailMsg", msg)
	}
	if len(result.StepRuns) != 1 || result.StepRuns[0].ParallelGroup != nil {
		t.Error("run should load with no grouping when the definition is unavailable")
	}
}

func TestTuiFetchRunDetail_FillsPendingSteps(t *testing.T) {
	// The run has only reached its first step; the definition has three. The two
	// not-yet-started steps must show up as pending so the live pipeline renders
	// the whole plan, in step_index order.
	run := tuiRunFull{
		tuiRun: tuiRun{RunID: "run-1", WorkflowID: "wf-1", Status: "running"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "build", Status: "completed"},
		},
	}
	def := tuiPipelineDef{
		WorkflowID: "wf-1",
		Steps: []tuiWorkflowStep{
			{StepID: "s0", Name: "build"},
			{StepID: "s1", Name: "test"},
			{StepID: "s2", Name: "deploy"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(run)) //nolint:errcheck
	})
	mux.HandleFunc("/workflows/pipelines/wf-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(def)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg := tuiFetchRunDetail("run-1", nil)()
	result, ok := msg.(tuiRunDetailMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiRunDetailMsg", msg)
	}
	if len(result.StepRuns) != 3 {
		t.Fatalf("len(StepRuns) = %d, want 3 (1 run + 2 synthesised pending)", len(result.StepRuns))
	}
	want := []struct{ name, status string }{
		{"build", "completed"}, {"test", "pending"}, {"deploy", "pending"},
	}
	for i, w := range want {
		if result.StepRuns[i].StepIndex != i {
			t.Errorf("StepRuns[%d].StepIndex = %d, want %d", i, result.StepRuns[i].StepIndex, i)
		}
		if result.StepRuns[i].StepName != w.name || result.StepRuns[i].Status != w.status {
			t.Errorf("StepRuns[%d] = %q/%q, want %q/%q", i,
				result.StepRuns[i].StepName, result.StepRuns[i].Status, w.name, w.status)
		}
	}
}

func TestTuiFetchRunPreview_FillsPendingForUnstartedRun(t *testing.T) {
	// A freshly-triggered run with no step runs yet still shows its full plan as
	// pending, so the live diagram is populated the moment the run is selected.
	run := tuiRunFull{
		tuiRun:   tuiRun{RunID: "run-1", WorkflowID: "wf-1", Status: "pending"},
		StepRuns: nil,
	}
	def := tuiPipelineDef{
		WorkflowID: "wf-1",
		Steps: []tuiWorkflowStep{
			{StepID: "s0", Name: "build"},
			{StepID: "s1", Name: "test"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(run)) //nolint:errcheck
	})
	mux.HandleFunc("/workflows/pipelines/wf-1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(jsonBody(def)) //nolint:errcheck
	})
	setupCLI(t, routeServer(t, mux))

	msg, ok := tuiFetchRunPreview("run-1", nil)().(tuiRunDiagramMsg)
	if !ok {
		t.Fatalf("msg type = %T, want tuiRunDiagramMsg", msg)
	}
	if msg.detail == nil || len(msg.detail.StepRuns) != 2 {
		t.Fatalf("detail StepRuns = %+v, want 2 pending steps", msg.detail)
	}
	for i, sr := range msg.detail.StepRuns {
		if sr.Status != "pending" {
			t.Errorf("StepRuns[%d].Status = %q, want pending", i, sr.Status)
		}
	}
}

func TestTUIView_RunDetail_ShowsParallelLegend(t *testing.T) {
	g := 0
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m = applyMsg(m, tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "r", Status: "completed"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "test", Status: "completed", ParallelGroup: &g},
			{StepIndex: 1, StepName: "lint", Status: "completed", ParallelGroup: &g},
		},
	}))
	if !strings.Contains(m.View(), "parallel") {
		t.Errorf("run detail with a parallel batch should show the legend, got: %q", m.View())
	}
}

func TestTUIView_RunDetail_NoLegend_WhenSequential(t *testing.T) {
	m := newTUIModel()
	m.view = tuiViewRunDetail
	m = applyMsg(m, tuiRunDetailMsg(tuiRunFull{
		tuiRun: tuiRun{RunID: "r", Status: "completed"},
		StepRuns: []tuiStepRun{
			{StepIndex: 0, StepName: "build", Status: "completed"},
			{StepIndex: 1, StepName: "deploy", Status: "completed"},
		},
	}))
	if strings.Contains(m.View(), "ran in parallel") {
		t.Error("a purely sequential run should not show the parallel legend")
	}
}

// ── Utility ───────────────────────────────────────────────────────────────────

// applyMsg is a convenience that calls Update and returns the updated model.
func applyMsg(m tuiModel, msg tea.Msg) tuiModel {
	updated, _ := m.Update(msg)
	return updated.(tuiModel)
}
