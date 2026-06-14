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

func applyAuditMsg(m auditModel, msg tea.Msg) auditModel {
	updated, _ := m.Update(msg)
	return updated.(auditModel)
}

// makeAuditEntries returns n trivial audit entries.
func makeAuditEntries(n int) []auditEntry {
	out := make([]auditEntry, n)
	for i := range out {
		out[i] = auditEntry{
			AuditLogID: fmt.Sprintf("log-%04d", i),
			ActorID:    "user-abc",
			ActorType:  "user",
			Action:     "role.update",
			ResourceID: "role-xyz",
			CreatedAt:  time.Now(),
		}
	}
	return out
}

// ── Initial state ─────────────────────────────────────────────────────────────

func TestAuditModel_InitialState(t *testing.T) {
	m := newAuditModel()
	if m.view != auditViewList {
		t.Errorf("initial view = %v, want auditViewList", m.view)
	}
	if !m.loading {
		t.Error("loading should be true on startup")
	}
	if m.err != nil {
		t.Errorf("initial err = %v, want nil", m.err)
	}
	if m.entries != nil {
		t.Error("initial entries should be nil")
	}
	if m.page != 0 {
		t.Errorf("initial page = %d, want 0", m.page)
	}
}

// ── Messages: auditEntriesMsg ─────────────────────────────────────────────────

func TestAuditModel_EntriesMsg_Populates(t *testing.T) {
	entries := makeAuditEntries(3)
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))

	if m.loading {
		t.Error("loading should be false after entriesMsg")
	}
	if len(m.entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(m.entries))
	}
	if m.entries[0].AuditLogID != "log-0000" {
		t.Errorf("entries[0].AuditLogID = %q, want log-0000", m.entries[0].AuditLogID)
	}
}

func TestAuditModel_EntriesMsg_Empty(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	if m.loading {
		t.Error("loading should be false after empty entriesMsg")
	}
}

// ── Messages: auditErrMsg ─────────────────────────────────────────────────────

func TestAuditModel_ErrMsg_SetsError(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditErrMsg{err: fmt.Errorf("forbidden")})
	if m.err == nil {
		t.Fatal("err should be set after errMsg")
	}
	if m.loading {
		t.Error("loading should be false after errMsg")
	}
	if !strings.Contains(m.err.Error(), "forbidden") {
		t.Errorf("err = %v", m.err)
	}
}

// ── Messages: WindowSizeMsg ───────────────────────────────────────────────────

func TestAuditModel_WindowResize(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.vp.Width != 116 {
		t.Errorf("vp.Width = %d, want 116", m.vp.Width)
	}
	if m.vp.Height != 32 {
		t.Errorf("vp.Height = %d, want 32", m.vp.Height)
	}
}

// ── Keys: list view ───────────────────────────────────────────────────────────

func TestAuditModel_List_Esc_GoesHome(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc key should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("esc returned %T, want goHomeMsg", cmd())
	}
}

func TestAuditModel_List_CtrlC_Quits(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should return a cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return QuitMsg")
	}
}

func TestAuditModel_List_Enter_NavigatesToDetail(t *testing.T) {
	entries := makeAuditEntries(2)
	entries[0].AuditLogID = "log-target"
	entries[0].Action = "user.delete"
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(auditModel)

	if m2.view != auditViewDetail {
		t.Errorf("view = %v, want auditViewDetail", m2.view)
	}
	if m2.selEntry == nil {
		t.Fatal("selEntry should be set after enter")
	}
	if m2.selEntry.AuditLogID != "log-target" {
		t.Errorf("selEntry.AuditLogID = %q, want log-target", m2.selEntry.AuditLogID)
	}
}

func TestAuditModel_List_Enter_Noop_WhenEmpty(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(auditModel).view != auditViewList {
		t.Error("enter with no entries should stay in list view")
	}
}

func TestAuditModel_List_NextPage_OnFullPage(t *testing.T) {
	entries := makeAuditEntries(auditPageSize) // exactly a full page
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("]")})
	m2 := updated.(auditModel)

	if m2.page != 1 {
		t.Errorf("page = %d, want 1 after ] on full page", m2.page)
	}
	if !m2.loading {
		t.Error("loading should be true after page advance")
	}
	if cmd == nil {
		t.Error("] on full page should emit a fetch cmd")
	}
}

func TestAuditModel_List_NextPage_Noop_OnPartialPage(t *testing.T) {
	entries := makeAuditEntries(5) // partial page
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("]")})
	m2 := updated.(auditModel)

	if m2.page != 0 {
		t.Errorf("page = %d, want 0 — ] on partial page should not advance", m2.page)
	}
	if cmd != nil {
		t.Error("] on partial page should not emit a fetch cmd")
	}
}

func TestAuditModel_List_PrevPage(t *testing.T) {
	entries := makeAuditEntries(1)
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))
	m.page = 2

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[")})
	m2 := updated.(auditModel)

	if m2.page != 1 {
		t.Errorf("page = %d, want 1 after [", m2.page)
	}
	if !m2.loading {
		t.Error("loading should be true after page change")
	}
	if cmd == nil {
		t.Error("[ should emit a fetch cmd")
	}
}

func TestAuditModel_List_PrevPage_Noop_AtFirst(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	// page is already 0
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[")})
	if updated.(auditModel).page != 0 {
		t.Error("[ at page 0 should not go negative")
	}
	if cmd != nil {
		t.Error("[ at page 0 should not emit a cmd")
	}
}

func TestAuditModel_List_R_Refreshes(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r key should emit a refresh cmd")
	}
}

// ── Keys: detail view ─────────────────────────────────────────────────────────

func TestAuditModel_Detail_Esc_Back(t *testing.T) {
	m := newAuditModel()
	m.view = auditViewDetail
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := updated.(auditModel)
	if m2.view != auditViewList {
		t.Errorf("esc: view = %v, want auditViewList", m2.view)
	}
	if cmd != nil {
		t.Error("esc should not emit a cmd")
	}
}

// ── Keys: error state ─────────────────────────────────────────────────────────

func TestAuditModel_Error_Esc_GoesHome(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditErrMsg{err: fmt.Errorf("boom")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in error state should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("esc in error state should return goHomeMsg")
	}
}

func TestAuditModel_Error_R_Retries(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditErrMsg{err: fmt.Errorf("boom")})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m2 := updated.(auditModel)
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

// ── View rendering ────────────────────────────────────────────────────────────

func TestAuditView_Loading(t *testing.T) {
	v := newAuditModel().View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("loading view should say Loading, got: %q", v)
	}
}

func TestAuditView_Error(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditErrMsg{err: fmt.Errorf("connection refused")})
	v := m.View()
	if !strings.Contains(v, "error") {
		t.Errorf("error view should contain 'error', got: %q", v)
	}
}

func TestAuditView_Empty(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	v := m.View()
	if !strings.Contains(v, "No audit") {
		t.Errorf("empty view should say 'No audit ...', got: %q", v)
	}
}

func TestAuditView_NonEmpty_ShowsAction(t *testing.T) {
	entries := makeAuditEntries(1)
	entries[0].Action = "org.create"
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))
	v := m.View()
	if !strings.Contains(v, "org.create") {
		t.Errorf("list view should show action, got: %q", v)
	}
}

func TestAuditView_PageInfo_HiddenOnFirstPage(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(makeAuditEntries(1)))
	v := m.View()
	if strings.Contains(v, "page 2") {
		t.Error("page info should not show 'page 2' when on page 0")
	}
}

func TestAuditView_PageInfo_ShownAfterAdvance(t *testing.T) {
	entries := makeAuditEntries(auditPageSize)
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))
	m.page = 1
	v := m.View()
	if !strings.Contains(v, "page 2") {
		t.Errorf("page info should show 'page 2' on page index 1, got: %q", v)
	}
}

func TestAuditView_EmptyNextPage_ShowsNoMore(t *testing.T) {
	m := newAuditModel()
	m.page = 2
	m = applyAuditMsg(m, auditEntriesMsg([]auditEntry{}))
	v := m.View()
	if !strings.Contains(v, "No more") {
		t.Errorf("empty page 2+ should say 'No more entries', got: %q", v)
	}
}

func TestAuditView_Detail_ShowsFields(t *testing.T) {
	entries := []auditEntry{{
		AuditLogID: "log-test",
		ActorID:    "user-xyz",
		ActorType:  "user",
		Action:     "team.delete",
		ResourceID: "team-abc",
		Detail:     "team removed by admin",
		CreatedAt:  time.Now(),
	}}
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg(entries))
	// Navigate to detail view.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	v := updated.(auditModel).View()
	if !strings.Contains(v, "team.delete") {
		t.Errorf("detail view should show action, got: %q", v)
	}
	if !strings.Contains(v, "user-xyz") {
		t.Errorf("detail view should show actor ID, got: %q", v)
	}
	if !strings.Contains(v, "team removed by admin") {
		t.Errorf("detail view should show detail text, got: %q", v)
	}
}

func TestAuditView_ListHelpText(t *testing.T) {
	m := applyAuditMsg(newAuditModel(), auditEntriesMsg([]auditEntry{}))
	v := m.View()
	if !strings.Contains(v, "home") || !strings.Contains(v, "refresh") {
		t.Errorf("list help should mention home and refresh, got: %q", v)
	}
}

// ── Fetch function ────────────────────────────────────────────────────────────

func TestAuditFetch_Success(t *testing.T) {
	entries := makeAuditEntries(2)
	body, _ := json.Marshal(entries)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := auditFetch(0)()

	if rec.Method != "GET" || rec.Path != "/gatekeeper/audit-logs" {
		t.Errorf("request = %s %s, want GET /gatekeeper/audit-logs", rec.Method, rec.Path)
	}
	result, ok := msg.(auditEntriesMsg)
	if !ok {
		t.Fatalf("msg type = %T, want auditEntriesMsg", msg)
	}
	if len(result) != 2 {
		t.Errorf("len(result) = %d, want 2", len(result))
	}
}

func TestAuditFetch_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
	setupCLI(t, srv)
	if _, ok := auditFetch(0)().(auditErrMsg); !ok {
		t.Error("HTTP error should return auditErrMsg")
	}
}

func TestAuditFetch_InvalidJSON(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `not json`)
	setupCLI(t, srv)
	if _, ok := auditFetch(0)().(auditErrMsg); !ok {
		t.Error("malformed JSON should return auditErrMsg")
	}
}

func TestAuditFetch_PaginationOffset(t *testing.T) {
	mux := http.NewServeMux()
	var gotQuery string
	mux.HandleFunc("/gatekeeper/audit-logs", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]")) //nolint:errcheck
	})
	srv := routeServer(t, mux)
	setupCLI(t, srv)

	auditFetch(2)() // page 2 → offset = 2*50 = 100

	if !strings.Contains(gotQuery, "offset=100") {
		t.Errorf("query = %q, want to contain offset=100", gotQuery)
	}
	if !strings.Contains(gotQuery, fmt.Sprintf("limit=%d", auditPageSize)) {
		t.Errorf("query = %q, want to contain limit=%d", gotQuery, auditPageSize)
	}
}

func TestAuditFetch_PageZeroOffset(t *testing.T) {
	mux := http.NewServeMux()
	var gotQuery string
	mux.HandleFunc("/gatekeeper/audit-logs", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]")) //nolint:errcheck
	})
	srv := routeServer(t, mux)
	setupCLI(t, srv)

	auditFetch(0)()

	if !strings.Contains(gotQuery, "offset=0") {
		t.Errorf("query = %q, want to contain offset=0", gotQuery)
	}
}

// ── Command registration ──────────────────────────────────────────────────────

func TestAuditTUICmd_RegisteredUnderAudit(t *testing.T) {
	if findSubcmd(t, auditCmd, "tui") == nil {
		t.Error("tui subcommand not registered under audit")
	}
}

func TestAuditCmd_HasRunE(t *testing.T) {
	if auditCmd.RunE == nil {
		t.Error("auditCmd.RunE should be set so 'armory audit' launches the TUI")
	}
}

// ── Auto-refresh ──────────────────────────────────────────────────────────────

func TestAuditModel_AutoRefresh_ListEmitsFetch(t *testing.T) {
	m := newAuditModel()
	m.loading = false
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh in the list view should emit a fetch cmd")
	}
}

func TestAuditModel_AutoRefresh_DetailNoop(t *testing.T) {
	m := newAuditModel()
	m.view = auditViewDetail
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd != nil {
		t.Error("auto-refresh in the detail view (immutable entry) should be a noop")
	}
}
