package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyForgeMsg(m forgeModel, msg tea.Msg) forgeModel {
	updated, _ := m.Update(msg)
	return updated.(forgeModel)
}

// ── Initial state ─────────────────────────────────────────────────────────────

func TestForgeModel_InitialState(t *testing.T) {
	m := newForgeModel()
	if m.view != forgeViewList {
		t.Errorf("initial view = %v, want forgeViewList", m.view)
	}
	if !m.loading {
		t.Error("loading should be true on startup")
	}
	if m.err != nil {
		t.Errorf("initial err = %v, want nil", m.err)
	}
	if m.execs != nil {
		t.Error("initial execs should be nil")
	}
	if m.detail != nil {
		t.Error("initial detail should be nil")
	}
}

// ── Messages: forgeExecsMsg ───────────────────────────────────────────────────

func TestForgeModel_ExecsMsg_Populates(t *testing.T) {
	execs := []forgeExec{
		{ExecutionID: "exec-aaa", Image: "ubuntu:22.04", Status: "completed"},
		{ExecutionID: "exec-bbb", Image: "python:3.12", Status: "running"},
	}
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg(execs))

	if m.loading {
		t.Error("loading should be false after execsMsg")
	}
	if len(m.execs) != 2 {
		t.Fatalf("len(execs) = %d, want 2", len(m.execs))
	}
	if m.execs[0].ExecutionID != "exec-aaa" {
		t.Errorf("execs[0].ExecutionID = %q, want exec-aaa", m.execs[0].ExecutionID)
	}
}

func TestForgeModel_ExecsMsg_Empty(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	if m.loading {
		t.Error("loading should be false after empty execsMsg")
	}
}

func TestForgeModel_ExecsMsg_NoSelfTick(t *testing.T) {
	// Auto-refresh is host-driven (one shared 5s ticker), so loading the exec
	// list must not schedule a per-model tick — regardless of exec status.
	for _, status := range []string{"running", "pending", "completed", "failed"} {
		_, cmd := newForgeModel().Update(forgeExecsMsg([]forgeExec{
			{ExecutionID: "e1", Status: status},
		}))
		if cmd != nil {
			t.Errorf("status %q: execsMsg should not schedule its own tick", status)
		}
	}
}

// ── Messages: forgeExecDetailMsg ──────────────────────────────────────────────

func TestForgeModel_ExecDetailMsg_SetsDetail(t *testing.T) {
	stdout := "hello world"
	exec := forgeExec{ExecutionID: "exec-abc", Status: "completed", Stdout: &stdout}
	m := newForgeModel()
	m.view = forgeViewOutput
	m.loading = true
	m2 := applyForgeMsg(m, forgeExecDetailMsg(exec))

	if m2.loading {
		t.Error("loading should be false after execDetailMsg")
	}
	if m2.detail == nil {
		t.Fatal("detail should be set after execDetailMsg")
	}
	if m2.detail.ExecutionID != "exec-abc" {
		t.Errorf("detail.ExecutionID = %q, want exec-abc", m2.detail.ExecutionID)
	}
}

func TestForgeModel_ExecDetailMsg_PopulatesViewport(t *testing.T) {
	stdout := "build output here"
	exec := forgeExec{ExecutionID: "e1", Stdout: &stdout, Status: "completed"}
	m := newForgeModel()
	m.view = forgeViewOutput
	m2 := applyForgeMsg(m, forgeExecDetailMsg(exec))
	// Viewport content should contain the stdout text.
	if !strings.Contains(m2.vp.View(), "build output here") {
		t.Error("viewport should show stdout content after execDetailMsg")
	}
}

func TestForgeModel_ExecDetailMsg_NoSelfTick(t *testing.T) {
	// Receiving exec detail must never schedule a per-model tick — auto-refresh
	// is host-driven — whatever the status or view.
	cases := []struct {
		view   forgeViewID
		status string
	}{
		{forgeViewOutput, "running"},
		{forgeViewOutput, "pending"},
		{forgeViewOutput, "completed"},
		{forgeViewList, "running"},
	}
	for _, c := range cases {
		m := newForgeModel()
		m.view = c.view
		_, cmd := m.Update(forgeExecDetailMsg(forgeExec{ExecutionID: "e1", Status: c.status}))
		if cmd != nil {
			t.Errorf("view %d status %q: execDetailMsg should not schedule its own tick", c.view, c.status)
		}
	}
}

// ── Messages: forgeErrMsg ─────────────────────────────────────────────────────

func TestForgeModel_ErrMsg_SetsError(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeErrMsg{err: fmt.Errorf("dial refused")})
	if m.err == nil {
		t.Fatal("err should be set after errMsg")
	}
	if m.loading {
		t.Error("loading should be false after errMsg")
	}
	if !strings.Contains(m.err.Error(), "dial refused") {
		t.Errorf("err = %v, expected 'dial refused'", m.err)
	}
}

// ── Messages: forgeCancelledMsg ───────────────────────────────────────────────

func TestForgeModel_CancelledMsg_SetsLoading(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeCancelledMsg{})
	if !m.loading {
		t.Error("loading should be true after forgeCancelledMsg")
	}
}

// ── Messages: tuiAutoRefreshMsg ───────────────────────────────────────────────

func TestForgeModel_AutoRefresh_InListView_EmitsFetch(t *testing.T) {
	m := newForgeModel()
	m.loading = false
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh in list view should emit a fetch cmd")
	}
}

func TestForgeModel_AutoRefresh_InOutputView_FetchesDetail(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"execution_id":"exec-abc","status":"running"}`)
	setupCLI(t, srv)

	m := newForgeModel()
	m.view = forgeViewOutput
	m.loading = false // detail already shown; an auto-refresh must stay silent
	m.selExec = &forgeExec{ExecutionID: "exec-abc"}
	updated, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Fatal("auto-refresh should emit a detail-fetch cmd")
	}
	// The auto-refresh must be silent — no "Loading…" flash mid-stream.
	if updated.(forgeModel).loading {
		t.Error("auto-refresh should not set loading=true (silent refresh)")
	}
	if _, ok := cmd().(forgeExecDetailMsg); !ok {
		t.Errorf("auto-refresh cmd returned %T, want forgeExecDetailMsg", cmd())
	}
	if rec.Method != "GET" || rec.Path != "/forge/executions/exec-abc" {
		t.Errorf("request = %s %s, want GET /forge/executions/exec-abc", rec.Method, rec.Path)
	}
}

func TestForgeModel_AutoRefresh_OutputViewNoSelExec_Noop(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	m.selExec = nil
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd != nil {
		t.Error("auto-refresh in output view with no selection should be a noop")
	}
}

func TestForgeModel_AutoRefresh_InCreateView_Noop(t *testing.T) {
	// The create form must not be disturbed by an auto-refresh.
	m := newForgeModel()
	m.view = forgeViewCreate
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd != nil {
		t.Error("auto-refresh in the create form should be a noop")
	}
}

// ── Messages: WindowSizeMsg ───────────────────────────────────────────────────

func TestForgeModel_WindowResize(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.vp.Width != 116 {
		t.Errorf("vp.Width = %d, want 116 (120-4)", m.vp.Width)
	}
	if m.vp.Height != 32 {
		t.Errorf("vp.Height = %d, want 32 (40-8)", m.vp.Height)
	}
}

// ── Keys: list view ───────────────────────────────────────────────────────────

func TestForgeModel_List_Esc_GoesHome(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc key should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("esc key returned %T, want goHomeMsg", cmd())
	}
}

func TestForgeModel_List_CtrlC_Quits(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should return a cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return QuitMsg")
	}
}

func TestForgeModel_List_Enter_NavigatesToOutput(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"execution_id":"exec-aaa","status":"completed"}`)
	setupCLI(t, srv)

	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{
		{ExecutionID: "exec-aaa", Status: "completed"},
	}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(forgeModel)

	if m2.view != forgeViewOutput {
		t.Errorf("view = %v, want forgeViewOutput", m2.view)
	}
	if m2.selExec == nil || m2.selExec.ExecutionID != "exec-aaa" {
		t.Errorf("selExec = %v, want exec-aaa", m2.selExec)
	}
	if !m2.loading {
		t.Error("loading should be true while fetching detail")
	}
	if cmd == nil {
		t.Error("enter should emit a detail-fetch cmd")
	}
}

func TestForgeModel_List_Enter_Noop_WhenEmpty(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(forgeModel).view != forgeViewList {
		t.Error("enter with no execs should stay in list view")
	}
}

func TestForgeModel_List_X_CancelsActiveExec(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)

	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{
		{ExecutionID: "exec-pending", Status: "pending"},
	}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd == nil {
		t.Fatal("x key on pending exec should emit a cancel cmd")
	}
	msg := cmd()
	if _, ok := msg.(forgeCancelledMsg); !ok {
		t.Errorf("cancel cmd returned %T, want forgeCancelledMsg", msg)
	}
	if rec.Method != "DELETE" {
		t.Errorf("method = %q, want DELETE", rec.Method)
	}
	if !strings.Contains(rec.Path, "exec-pending") {
		t.Errorf("path = %q, want to contain exec-pending", rec.Path)
	}
}

func TestForgeModel_List_X_AlsoCancelsRunning(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)

	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{
		{ExecutionID: "exec-running", Status: "running"},
	}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd == nil {
		t.Fatal("x key on running exec should emit a cancel cmd")
	}
	cmd()
	if rec.Method != "DELETE" {
		t.Errorf("method = %q, want DELETE for running exec", rec.Method)
	}
}

func TestForgeModel_List_X_Noop_WhenCompleted(t *testing.T) {
	// Completed execs cannot be cancelled; pressing x must not emit a cancel cmd
	// (so doRequest is never called).
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{
		{ExecutionID: "exec-done", Status: "completed"},
	}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil {
		t.Error("x on a completed exec should not emit a cmd")
	}
}

func TestForgeModel_List_R_Refreshes(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r key should emit a refresh cmd")
	}
}

// ── Keys: output view ─────────────────────────────────────────────────────────

func TestForgeModel_Output_Esc_Back(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := updated.(forgeModel)
	if m2.view != forgeViewList {
		t.Error("esc in output view should return to list")
	}
	if cmd != nil {
		t.Error("esc should not emit a cmd")
	}
	if m2.detail != nil {
		t.Error("detail should be cleared on back")
	}
}

func TestForgeModel_Output_R_Refreshes(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"execution_id":"exec-abc","status":"completed"}`)
	setupCLI(t, srv)

	m := newForgeModel()
	m.view = forgeViewOutput
	m.selExec = &forgeExec{ExecutionID: "exec-abc"}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r in output view should emit a detail-fetch cmd")
	}
}

func TestForgeModel_Output_R_Noop_WhenNoSelection(t *testing.T) {
	// Apply an execs message to clear the initial loading=true state.
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	m.view = forgeViewOutput
	m.selExec = nil
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if updated.(forgeModel).loading {
		t.Error("r with no selExec should not set loading=true")
	}
}

// ── Keys: error state ─────────────────────────────────────────────────────────

func TestForgeModel_Error_Esc_GoesHome(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeErrMsg{err: fmt.Errorf("boom")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in error state should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("esc in error state should return goHomeMsg")
	}
}

func TestForgeModel_Error_R_Retries(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeErrMsg{err: fmt.Errorf("boom")})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m2 := updated.(forgeModel)
	if m2.err != nil {
		t.Error("r should clear the error")
	}
	if !m2.loading {
		t.Error("r should set loading=true")
	}
	if cmd == nil {
		t.Error("r should emit a fetch cmd")
	}
}

func TestForgeModel_Error_CtrlC_Quits(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeErrMsg{err: fmt.Errorf("boom")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c in error state should return a cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return QuitMsg")
	}
}

// ── View rendering ────────────────────────────────────────────────────────────

func TestForgeView_Loading(t *testing.T) {
	v := newForgeModel().View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("loading view should say Loading, got: %q", v)
	}
}

func TestForgeView_Error(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeErrMsg{err: fmt.Errorf("connection refused")})
	v := m.View()
	if !strings.Contains(v, "error") {
		t.Errorf("error view should contain 'error', got: %q", v)
	}
	if !strings.Contains(v, "connection refused") {
		t.Errorf("error view should show error text, got: %q", v)
	}
}

func TestForgeView_EmptyExecs(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	v := m.View()
	if !strings.Contains(v, "No executions") {
		t.Errorf("empty view should say 'No executions', got: %q", v)
	}
}

func TestForgeView_NonEmptyExecs_ShowsImage(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{
		{ExecutionID: "exec-abc", Image: "ubuntu:22.04", Status: "completed"},
	}))
	v := m.View()
	if !strings.Contains(v, "ubuntu:22.04") {
		t.Errorf("list view should show image name, got: %q", v)
	}
}

func TestForgeView_ListHelpText(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	v := m.View()
	if !strings.Contains(v, "cancel") || !strings.Contains(v, "home") {
		t.Errorf("list help should mention cancel and home, got: %q", v)
	}
}

func TestForgeView_OutputLoading(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	m.loading = true
	v := m.View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("output loading view should say Loading, got: %q", v)
	}
}

func TestForgeView_OutputHelpText(t *testing.T) {
	stdout := "x"
	m := newForgeModel()
	m.view = forgeViewOutput
	m = applyForgeMsg(m, forgeExecDetailMsg(forgeExec{ExecutionID: "e1", Stdout: &stdout}))
	v := m.View()
	if !strings.Contains(v, "back") || !strings.Contains(v, "scroll") {
		t.Errorf("output help should mention back and scroll, got: %q", v)
	}
}

// ── forgeRenderOutput ─────────────────────────────────────────────────────────

func TestForgeRenderOutput_NoOutput(t *testing.T) {
	out := forgeRenderOutput(forgeExec{ExecutionID: "e1", Status: "completed"})
	if !strings.Contains(out, "no stdout") {
		t.Errorf("no stdout should show fallback text, got: %q", out)
	}
}

func TestForgeRenderOutput_WithStdout(t *testing.T) {
	s := "hello from container"
	out := forgeRenderOutput(forgeExec{Stdout: &s})
	if !strings.Contains(out, "hello from container") {
		t.Errorf("stdout should appear in output, got: %q", out)
	}
	if strings.Contains(out, "STDERR") {
		t.Error("should not show STDERR section when there is none")
	}
}

func TestForgeRenderOutput_WithStderr(t *testing.T) {
	stdout := "ok"
	stderr := "warning: deprecated flag"
	out := forgeRenderOutput(forgeExec{Stdout: &stdout, Stderr: &stderr})
	if !strings.Contains(out, "STDERR") {
		t.Error("should show STDERR section when stderr is non-empty")
	}
	if !strings.Contains(out, "warning: deprecated flag") {
		t.Errorf("stderr content should appear, got: %q", out)
	}
}

func TestForgeRenderOutput_EmptyStdout_EmptyStderr(t *testing.T) {
	empty := ""
	out := forgeRenderOutput(forgeExec{Stdout: &empty, Stderr: &empty})
	if !strings.Contains(out, "no stdout") {
		t.Error("empty stdout should use the fallback text")
	}
	if strings.Contains(out, "STDERR") {
		t.Error("empty stderr should not show STDERR section")
	}
}

// ── Fetch functions ───────────────────────────────────────────────────────────

func TestForgeFetchExecs_Success(t *testing.T) {
	execs := []forgeExec{{ExecutionID: "e1", Status: "completed"}}
	body, _ := json.Marshal(execs)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := forgeFetchExecs()

	if rec.Method != "GET" || rec.Path != "/forge/executions" {
		t.Errorf("request = %s %s, want GET /forge/executions", rec.Method, rec.Path)
	}
	result, ok := msg.(forgeExecsMsg)
	if !ok {
		t.Fatalf("msg type = %T, want forgeExecsMsg", msg)
	}
	if len(result) != 1 || result[0].ExecutionID != "e1" {
		t.Errorf("result = %v, want [{e1 ...}]", result)
	}
}

func TestForgeFetchExecs_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	setupCLI(t, srv)
	if _, ok := forgeFetchExecs().(forgeErrMsg); !ok {
		t.Error("HTTP error should return forgeErrMsg")
	}
}

func TestForgeFetchExecs_InvalidJSON(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `not json`)
	setupCLI(t, srv)
	if _, ok := forgeFetchExecs().(forgeErrMsg); !ok {
		t.Error("malformed JSON should return forgeErrMsg")
	}
}

func TestForgeFetchDetail_Success(t *testing.T) {
	stdout := "output"
	exec := forgeExec{ExecutionID: "exec-abc", Status: "completed", Stdout: &stdout}
	body, _ := json.Marshal(exec)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := forgeFetchDetail("exec-abc")()

	if rec.Method != "GET" || rec.Path != "/forge/executions/exec-abc" {
		t.Errorf("request = %s %s, want GET /forge/executions/exec-abc", rec.Method, rec.Path)
	}
	result, ok := msg.(forgeExecDetailMsg)
	if !ok {
		t.Fatalf("msg type = %T, want forgeExecDetailMsg", msg)
	}
	if result.ExecutionID != "exec-abc" {
		t.Errorf("ExecutionID = %q, want exec-abc", result.ExecutionID)
	}
}

func TestForgeFetchDetail_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusNotFound, `{"error":"not found"}`)
	setupCLI(t, srv)
	if _, ok := forgeFetchDetail("nope")().(forgeErrMsg); !ok {
		t.Error("HTTP error should return forgeErrMsg")
	}
}

// ── Create flow ───────────────────────────────────────────────────────────────

func TestForgeModel_List_N_OpensCreateForm(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(forgeModel)
	if m2.view != forgeViewCreate {
		t.Errorf("view = %v, want forgeViewCreate", m2.view)
	}
	if cmd == nil {
		t.Error("opening the form should return a focus/blink cmd")
	}
	if len(m2.form.fields) == 0 {
		t.Error("create form should have fields")
	}
}

func TestForgeModel_Create_Esc_BacksToList(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewCreate
	m.form, _ = newForgeCreateForm(nil, nil, kvCatalog{})
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(forgeModel).view != forgeViewList {
		t.Error("esc in create view should return to list")
	}
}

func TestForgeModel_Create_Submit_MissingImage_StaysWithError(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewCreate
	m.form, _ = newForgeCreateForm(nil, nil, kvCatalog{})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(forgeModel)
	if m2.view != forgeViewCreate {
		t.Error("submit with no image should stay in the create view")
	}
	if m2.form.errMsg == "" {
		t.Error("submit with no image should set an inline error")
	}
	if cmd != nil {
		t.Error("invalid submit should not emit a request cmd")
	}
}

func TestForgeModel_Create_CtrlC_Quits(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewCreate
	m.form, _ = newForgeCreateForm(nil, nil, kvCatalog{})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should return a cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c in create view should quit")
	}
}

func TestForgeModel_CreatedMsg_ReturnsToListAndRefetches(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewCreate
	updated, cmd := m.Update(forgeCreatedMsg{})
	m2 := updated.(forgeModel)
	if m2.view != forgeViewList {
		t.Error("forgeCreatedMsg should return to the list view")
	}
	if !m2.loading {
		t.Error("forgeCreatedMsg should set loading=true")
	}
	if cmd == nil {
		t.Error("forgeCreatedMsg should emit a refetch cmd")
	}
}

func TestForgeModel_FormErrMsg_ShowsInlineError(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewCreate
	m.form, _ = newForgeCreateForm(nil, nil, kvCatalog{})
	updated, _ := m.Update(forgeFormErrMsg{err: fmt.Errorf("image not allowed")})
	m2 := updated.(forgeModel)
	if m2.view != forgeViewCreate {
		t.Error("form error should stay in the create view")
	}
	if !strings.Contains(m2.form.errMsg, "image not allowed") {
		t.Errorf("form.errMsg = %q, want to contain 'image not allowed'", m2.form.errMsg)
	}
}

func TestForgeSubmitExec_PostsCorrectPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"execution_id":"e1"}`)
	setupCLI(t, srv)

	msg := forgeSubmitExec("ubuntu:22.04", []string{"sh", "-c", "echo hi"},
		map[string]string{"FOO": "bar"}, 120, "large", "")()

	if _, ok := msg.(forgeCreatedMsg); !ok {
		t.Fatalf("msg = %T, want forgeCreatedMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/forge/executions" {
		t.Errorf("request = %s %s, want POST /forge/executions", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["image"] != "ubuntu:22.04" {
		t.Errorf("image = %v, want ubuntu:22.04", got["image"])
	}
	cmd, _ := got["command"].([]any)
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[2] != "echo hi" {
		t.Errorf("command = %v, want [sh -c echo hi]", got["command"])
	}
	if got["runner_class"] != "large" {
		t.Errorf("runner_class = %v, want large", got["runner_class"])
	}
	if got["timeout"].(float64) != 120 {
		t.Errorf("timeout = %v, want 120", got["timeout"])
	}
}

func TestForgeSubmitExec_HTTPError_ReturnsFormErr(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusBadRequest, `{"error":"image not allowed"}`)
	setupCLI(t, srv)
	if _, ok := forgeSubmitExec("x", []string{"sh"}, nil, 0, "", "")().(forgeFormErrMsg); !ok {
		t.Error("HTTP error should return forgeFormErrMsg")
	}
}

// ── Rerun flow ────────────────────────────────────────────────────────────────

func TestForgeRerunExec_PostsCopiedPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"execution_id":"new-1"}`)
	setupCLI(t, srv)

	e := forgeExec{
		ExecutionID: "orig-1",
		Image:       "ubuntu:22.04",
		Command:     []string{"sh", "-c", "echo hi"},
		Env:         map[string]string{"FOO": "bar"},
		Timeout:     120,
		RunnerClass: "large",
		Status:      "completed",
	}
	msg := forgeRerunExec(e)()
	if _, ok := msg.(forgeCreatedMsg); !ok {
		t.Fatalf("msg = %T, want forgeCreatedMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/forge/executions" {
		t.Errorf("request = %s %s, want POST /forge/executions", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["image"] != "ubuntu:22.04" {
		t.Errorf("image = %v, want ubuntu:22.04", got["image"])
	}
	cmd, _ := got["command"].([]any)
	if len(cmd) != 3 || cmd[2] != "echo hi" {
		t.Errorf("command = %v, want [sh -c echo hi]", got["command"])
	}
	if got["runner_class"] != "large" {
		t.Errorf("runner_class = %v, want large", got["runner_class"])
	}
}

func TestForgeRerunExec_HTTPError_ReturnsErr(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusBadRequest, `{"error":"image not allowed"}`)
	setupCLI(t, srv)
	e := forgeExec{Image: "x", Command: []string{"sh"}}
	if _, ok := forgeRerunExec(e)().(forgeErrMsg); !ok {
		t.Error("HTTP error should return forgeErrMsg so the failure isn't swallowed")
	}
}

func TestForgeModel_List_R_Capital_Reruns(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"execution_id":"new-1"}`)
	setupCLI(t, srv)

	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{
		{ExecutionID: "orig-1", Image: "ubuntu:22.04", Command: []string{"sh", "-c", "echo hi"}, Status: "completed"},
	}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if cmd == nil {
		t.Fatal("R on a row with a command should emit a rerun cmd")
	}
	if _, ok := cmd().(forgeCreatedMsg); !ok {
		t.Errorf("rerun cmd returned %T, want forgeCreatedMsg", cmd())
	}
	if rec.Method != "POST" || rec.Path != "/forge/executions" {
		t.Errorf("request = %s %s, want POST /forge/executions", rec.Method, rec.Path)
	}
}

func TestForgeModel_List_R_Capital_Noop_WhenNoCommand(t *testing.T) {
	// A row without a captured command can't be rerun; R must not emit a cmd
	// (so no request is sent).
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{
		{ExecutionID: "orig-1", Image: "ubuntu:22.04", Status: "completed"},
	}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if cmd != nil {
		t.Error("R on a row without a command should not emit a cmd")
	}
}

func TestForgeModel_Output_R_Capital_Reruns(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"execution_id":"new-1"}`)
	setupCLI(t, srv)

	m := newForgeModel()
	m.view = forgeViewOutput
	m.selExec = &forgeExec{ExecutionID: "orig-1", Image: "ubuntu:22.04", Command: []string{"sh", "-c", "echo hi"}}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if cmd == nil {
		t.Fatal("R in output view should emit a rerun cmd")
	}
	if _, ok := cmd().(forgeCreatedMsg); !ok {
		t.Errorf("rerun cmd returned %T, want forgeCreatedMsg", cmd())
	}
	if rec.Method != "POST" || rec.Path != "/forge/executions" {
		t.Errorf("request = %s %s, want POST /forge/executions", rec.Method, rec.Path)
	}
}

func TestForgeModel_Output_R_Capital_Noop_WhenNoCommand(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	m.selExec = &forgeExec{ExecutionID: "orig-1", Image: "ubuntu:22.04"}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	if cmd != nil {
		t.Error("R with no captured command should not emit a cmd")
	}
}

// The list endpoint returns command/env; the model must decode them so a rerun
// works straight from the list without a second fetch.
func TestForgeFetchExecs_DecodesCommand(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK,
		`[{"execution_id":"e1","image":"ubuntu:22.04","command":["sh","-c","echo hi"],"status":"completed"}]`)
	setupCLI(t, srv)

	msg := forgeFetchExecs()
	execs, ok := msg.(forgeExecsMsg)
	if !ok {
		t.Fatalf("msg = %T, want forgeExecsMsg", msg)
	}
	if len(execs) != 1 || len(execs[0].Command) != 3 || execs[0].Command[2] != "echo hi" {
		t.Errorf("command = %v, want [sh -c echo hi] decoded from the list", execs[0].Command)
	}
}

// ── buildForgeCommand ──────────────────────────────────────────────────────────

// A single line is tokenised into argv (forge runs argv with no shell).
func TestBuildForgeCommand_SingleLineIsArgv(t *testing.T) {
	got, err := buildForgeCommand(`sh -c "echo hi"`)
	if err != nil {
		t.Fatalf("buildForgeCommand error: %v", err)
	}
	want := []string{"sh", "-c", "echo hi"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("command = %v, want %v", got, want)
	}
}

// Regression: a multi-line entry is a script — it must run through a shell, not be
// flattened into argv (which made `echo "hello world"` swallow the later lines as
// its arguments and print them all on one line).
func TestBuildForgeCommand_MultiLineWrapsInShell(t *testing.T) {
	script := "echo \"hello world\"\nls -la\ncd /\nls -la"
	got, err := buildForgeCommand(script)
	if err != nil {
		t.Fatalf("buildForgeCommand error: %v", err)
	}
	if len(got) != 3 || got[0] != "sh" || got[1] != "-c" {
		t.Fatalf("command = %v, want [sh -c <script>]", got)
	}
	if got[2] != script {
		t.Errorf("script arg = %q, want the verbatim multi-line script %q", got[2], script)
	}
}

func TestForgeView_CreateView_RendersForm(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewCreate
	m.form, _ = newForgeCreateForm(nil, nil, kvCatalog{})
	v := m.View()
	if !strings.Contains(v, "New Execution") {
		t.Errorf("create view should show the form heading, got: %q", v)
	}
}

func TestForgeView_ListHelp_MentionsNew(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	if !strings.Contains(m.View(), "new") {
		t.Error("list help should mention the new-execution shortcut")
	}
}

func TestForgeView_ListHelp_MentionsRerun(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	if !strings.Contains(m.View(), "rerun") {
		t.Error("list help should mention the rerun shortcut")
	}
}

// ── Create form: image/runner selectors ───────────────────────────────────────

func TestForgeFetchImages_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `["alpine:3.19","ubuntu:22.04"]`)
	setupCLI(t, srv)
	msg := forgeFetchImages()
	if rec.Method != "GET" || rec.Path != "/forge/images" {
		t.Errorf("request = %s %s, want GET /forge/images", rec.Method, rec.Path)
	}
	imgs, ok := msg.(forgeImagesMsg)
	if !ok || len(imgs) != 2 || imgs[0] != "alpine:3.19" {
		t.Errorf("msg = %#v, want forgeImagesMsg[alpine,ubuntu]", msg)
	}
}

func TestForgeFetchImages_ErrorDegradesToEmpty(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	// A failure must not surface as forgeErrMsg (which would break the list view).
	msg, ok := forgeFetchImages().(forgeImagesMsg)
	if !ok {
		t.Fatalf("msg = %T, want forgeImagesMsg even on error", forgeFetchImages())
	}
	if len(msg) != 0 {
		t.Errorf("images = %v, want empty on error", msg)
	}
}

func TestForgeFetchRunners_FiltersEnabled(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK,
		`[{"name":"standard","enabled":true},{"name":"old","enabled":false},{"name":"large","enabled":true}]`)
	setupCLI(t, srv)
	msg, ok := forgeFetchRunners().(forgeRunnersMsg)
	if !ok {
		t.Fatalf("msg = %T, want forgeRunnersMsg", forgeFetchRunners())
	}
	if len(msg) != 2 || msg[0] != "standard" || msg[1] != "large" {
		t.Errorf("runners = %v, want [standard large] (enabled only)", msg)
	}
}

func TestForgeModel_ImagesMsg_Caches(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeImagesMsg([]string{"alpine:3.19"}))
	if len(m.images) != 1 || m.images[0] != "alpine:3.19" {
		t.Errorf("images = %v, want [alpine:3.19]", m.images)
	}
}

func TestForgeModel_RunnersMsg_Caches(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeRunnersMsg([]string{"large"}))
	if len(m.runners) != 1 || m.runners[0] != "large" {
		t.Errorf("runners = %v, want [large]", m.runners)
	}
}

func TestForgeModel_CreateForm_UsesSelectorsWhenListsKnown(t *testing.T) {
	m := newForgeModel()
	m.images = []string{"alpine:3.19", "ubuntu:22.04"}
	m.runners = []string{"large"}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(forgeModel)
	// The image selector starts on the first allowed image; ←/→ cycles it.
	if got := m2.form.value("image"); got != "alpine:3.19" {
		t.Errorf("image = %q, want alpine:3.19 (first option)", got)
	}
	m3, _, _ := m2.form.update(tea.KeyMsg{Type: tea.KeyRight})
	if got := m3.value("image"); got != "ubuntu:22.04" {
		t.Errorf("image after cycle = %q, want ubuntu:22.04", got)
	}
	// The runner selector defaults to the "(default)" empty option.
	if got := m2.form.value("runner"); got != "" {
		t.Errorf("runner = %q, want empty default", got)
	}
}

func TestForgeModel_CreateForm_FallsBackToTextWhenNoLists(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(forgeModel)
	// With no cached lists the image field is free text — typing populates it.
	m2.form = typeForm(m2.form, "myimage:latest")
	if got := m2.form.value("image"); got != "myimage:latest" {
		t.Errorf("image = %q, want typed text (free-text fallback)", got)
	}
}

func TestForgeSubmitExec_SelectedDefaultRunner_OmitsRunnerClass(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"execution_id":"e1"}`)
	setupCLI(t, srv)
	// Empty runner (the "(default)" option) must not send runner_class.
	forgeSubmitExec("alpine:3.19", []string{"sh"}, nil, 0, "", "")()
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, present := got["runner_class"]; present {
		t.Errorf("runner_class should be omitted when empty, body = %s", rec.Body)
	}
}

// ── Command registration ──────────────────────────────────────────────────────

func TestForgeTUICmd_RegisteredUnderForge(t *testing.T) {
	if findSubcmd(t, forgeCmd, "tui") == nil {
		t.Error("tui subcommand not registered under forge")
	}
}

func TestForgeCmd_HasRunE(t *testing.T) {
	if forgeCmd.RunE == nil {
		t.Error("forgeCmd.RunE should be set so 'armory forge' launches the TUI")
	}
}
