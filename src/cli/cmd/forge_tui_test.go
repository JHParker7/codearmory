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

func TestForgeModel_ExecsMsg_RunningExec_EmitsTick(t *testing.T) {
	_, cmd := newForgeModel().Update(forgeExecsMsg([]forgeExec{
		{ExecutionID: "e1", Status: "running"},
	}))
	if cmd == nil {
		t.Error("running exec should schedule an auto-refresh tick")
	}
}

func TestForgeModel_ExecsMsg_PendingExec_EmitsTick(t *testing.T) {
	_, cmd := newForgeModel().Update(forgeExecsMsg([]forgeExec{
		{ExecutionID: "e1", Status: "pending"},
	}))
	if cmd == nil {
		t.Error("pending exec should schedule an auto-refresh tick")
	}
}

func TestForgeModel_ExecsMsg_CompletedOnly_NoTick(t *testing.T) {
	_, cmd := newForgeModel().Update(forgeExecsMsg([]forgeExec{
		{ExecutionID: "e1", Status: "completed"},
		{ExecutionID: "e2", Status: "failed"},
	}))
	if cmd != nil {
		t.Error("completed/failed-only execs should not schedule a tick")
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

// ── Messages: forgeTickMsg ────────────────────────────────────────────────────

func TestForgeModel_TickMsg_InListView_EmitsFetch(t *testing.T) {
	m := newForgeModel()
	m.loading = false
	_, cmd := m.Update(forgeTickMsg{})
	if cmd == nil {
		t.Error("tick in list view should emit a fetch cmd")
	}
}

func TestForgeModel_TickMsg_InOutputView_Noop(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	_, cmd := m.Update(forgeTickMsg{})
	if cmd != nil {
		t.Error("tick in output view should be a noop")
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

func TestForgeModel_List_Q_GoesHome(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeExecsMsg([]forgeExec{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q key should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("q key returned %T, want goHomeMsg", cmd())
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

func TestForgeModel_Output_Q_GoesHome(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("q in output view should return goHomeMsg")
	}
}

func TestForgeModel_Output_B_Back(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	m2 := updated.(forgeModel)
	if m2.view != forgeViewList {
		t.Error("b in output view should return to list")
	}
	if cmd != nil {
		t.Error("b should not emit a cmd")
	}
	if m2.detail != nil {
		t.Error("detail should be cleared on back")
	}
}

func TestForgeModel_Output_Esc_Back(t *testing.T) {
	m := newForgeModel()
	m.view = forgeViewOutput
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(forgeModel).view != forgeViewList {
		t.Error("esc in output view should return to list")
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

func TestForgeModel_Error_Q_GoesHome(t *testing.T) {
	m := applyForgeMsg(newForgeModel(), forgeErrMsg{err: fmt.Errorf("boom")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q in error state should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("q in error state should return goHomeMsg")
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
