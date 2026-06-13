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

func applyGK(m gatekeeperModel, msg tea.Msg) gatekeeperModel {
	updated, _ := m.Update(msg)
	return updated.(gatekeeperModel)
}

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// ── Record builders ───────────────────────────────────────────────────────────

func gkTeamRecords(n int) []gkRecord {
	out := make([]gkRecord, n)
	for i := range out {
		out[i] = gkRecord{
			"team_id":    fmt.Sprintf("team-%04d", i),
			"team_name":  fmt.Sprintf("team-%d", i),
			"owner_id":   "owner-abc",
			"active":     true,
			"created_at": time.Now().Format(time.RFC3339),
		}
	}
	return out
}

func gkInviteRecords(n int) []gkRecord {
	out := make([]gkRecord, n)
	for i := range out {
		out[i] = gkRecord{
			"invite_id":     fmt.Sprintf("inv-%04d", i),
			"invitee_email": fmt.Sprintf("user%d@example.com", i),
			"resource_type": "team",
			"status":        "pending",
			"created_at":    time.Now().Format(time.RFC3339),
		}
	}
	return out
}

func gkSRRecords(n int) []gkRecord {
	out := make([]gkRecord, n)
	for i := range out {
		out[i] = gkRecord{
			"request_id":   fmt.Sprintf("req-%04d", i),
			"service_name": "forge",
			"name":         fmt.Sprintf("perm-%d", i),
			"status":       "pending",
			"created_at":   time.Now().Format(time.RFC3339),
		}
	}
	return out
}

// ── Initial state ───────────────────────────────────────────────────────────

func TestGKModel_InitialState(t *testing.T) {
	m := newGatekeeperModel()
	if m.section != gkTeams {
		t.Errorf("initial section = %v, want gkTeams", m.section)
	}
	if m.view != gkViewList {
		t.Errorf("initial view = %v, want gkViewList", m.view)
	}
	if !m.loading {
		t.Error("loading should be true on startup")
	}
	if len(m.sections) != len(gkDefs) {
		t.Errorf("len(sections) = %d, want %d", len(m.sections), len(gkDefs))
	}
}

func TestGKModel_Init_FetchesTeams(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, "[]")
	setupCLI(t, srv)
	newGatekeeperModel().Init()()
	if rec.Path != "/gatekeeper/teams" {
		t.Errorf("Init fetched %s, want /gatekeeper/teams", rec.Path)
	}
}

// ── Messages ──────────────────────────────────────────────────────────────────

func TestGKModel_RecordsMsg_PopulatesSection(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkInvites, records: gkInviteRecords(3)})
	if m.loading {
		t.Error("loading should be false after recordsMsg")
	}
	if len(m.sections[gkInvites].records) != 3 {
		t.Fatalf("invites records = %d, want 3", len(m.sections[gkInvites].records))
	}
	if !m.sections[gkInvites].loaded {
		t.Error("invites section should be marked loaded")
	}
	if m.sections[gkTeams].loaded {
		t.Error("teams section should not be loaded by an invites msg")
	}
}

func TestGKModel_ErrMsg_SetsError(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkErrMsg{err: fmt.Errorf("forbidden")})
	if m.err == nil {
		t.Fatal("err should be set")
	}
	if m.loading {
		t.Error("loading should be false after errMsg")
	}
}

func TestGKModel_WindowResize(t *testing.T) {
	m := applyGK(newGatekeeperModel(), tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.vp.Width != 116 {
		t.Errorf("vp.Width = %d, want 116", m.vp.Width)
	}
	if m.vp.Height != 31 {
		t.Errorf("vp.Height = %d, want 31", m.vp.Height)
	}
}

// ── Section switching ─────────────────────────────────────────────────────────

func TestGKModel_Tab_SwitchesSectionAndFetches(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m2 := updated.(gatekeeperModel)
	if m2.section != gkInvites {
		t.Errorf("after tab, section = %v, want gkInvites", m2.section)
	}
	if !m2.loading {
		t.Error("switching to an unloaded section should set loading")
	}
	if cmd == nil {
		t.Fatal("switching to an unloaded section should emit a fetch cmd")
	}
}

func TestGKModel_ShiftTab_WrapsBackwards(t *testing.T) {
	m := newGatekeeperModel() // section = gkTeams (0)
	m2 := applyGK(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m2.section != gkUsers {
		t.Errorf("shift+tab from teams should wrap to gkUsers, got %v", m2.section)
	}
}

func TestGKModel_NumberKey_JumpsToSection(t *testing.T) {
	m := applyGK(newGatekeeperModel(), keyRunes("3"))
	if m.section != gkServiceRequests {
		t.Errorf("key '3' should jump to gkServiceRequests, got %v", m.section)
	}
}

func TestGKModel_Switch_NoRefetchWhenLoaded(t *testing.T) {
	m := newGatekeeperModel()
	m = applyGK(m, gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m = applyGK(m, gkRecordsMsg{section: gkInvites, records: gkInviteRecords(1)})
	// Jump to the already-loaded invites section.
	updated, cmd := m.Update(keyRunes("2"))
	if cmd != nil {
		t.Error("switching to a loaded section should not refetch")
	}
	if updated.(gatekeeperModel).loading {
		t.Error("switching to a loaded section should not set loading")
	}
}

// ── List navigation / detail ───────────────────────────────────────────────────

func TestGKModel_Enter_OpensDetail(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(2)})
	m2 := applyGK(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m2.view != gkViewDetail {
		t.Errorf("enter should open detail, view = %v", m2.view)
	}
	if m2.selLabel != "team-0" {
		t.Errorf("selLabel = %q, want team-0", m2.selLabel)
	}
}

func TestGKModel_Enter_NoopWhenEmpty(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: []gkRecord{}})
	m2 := applyGK(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m2.view != gkViewList {
		t.Error("enter with no records should stay in list view")
	}
}

func TestGKModel_Detail_Back(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m = applyGK(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = applyGK(m, keyRunes("b"))
	if m.view != gkViewList {
		t.Error("b should return to list from detail")
	}
}

func TestGKModel_List_Q_GoesHome(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: []gkRecord{}})
	_, cmd := m.Update(keyRunes("q"))
	if cmd == nil {
		t.Fatal("q should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("q returned %T, want goHomeMsg", cmd())
	}
}

// ── Actions ─────────────────────────────────────────────────────────────────

func TestGKModel_Invite_Accept_HitsEndpoint(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, "{}")
	setupCLI(t, srv)

	m := newGatekeeperModel()
	m.section = gkInvites
	m = applyGK(m, gkRecordsMsg{section: gkInvites, records: gkInviteRecords(1)})

	_, cmd := m.Update(keyRunes("a"))
	if cmd == nil {
		t.Fatal("'a' on an invite should emit an accept cmd")
	}
	msg := cmd()
	if rec.Method != "POST" || rec.Path != "/gatekeeper/invites/inv-0000/accept" {
		t.Errorf("request = %s %s, want POST /gatekeeper/invites/inv-0000/accept", rec.Method, rec.Path)
	}
	if am, ok := msg.(gkActionMsg); !ok || am.label != "accept" {
		t.Errorf("msg = %#v, want gkActionMsg{label:accept}", msg)
	}
}

func TestGKModel_SR_Approve_HitsEndpoint(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, "{}")
	setupCLI(t, srv)

	m := newGatekeeperModel()
	m.section = gkServiceRequests
	m = applyGK(m, gkRecordsMsg{section: gkServiceRequests, records: gkSRRecords(1)})

	_, cmd := m.Update(keyRunes("a"))
	if cmd == nil {
		t.Fatal("'a' on a service request should emit an approve cmd")
	}
	cmd()
	if rec.Method != "POST" || rec.Path != "/gatekeeper/service-permission-requests/req-0000/approve" {
		t.Errorf("request = %s %s, want POST .../req-0000/approve", rec.Method, rec.Path)
	}
}

func TestGKModel_Delete_RequiresConfirm(t *testing.T) {
	m := newGatekeeperModel()
	m = applyGK(m, gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})

	updated, cmd := m.Update(keyRunes("D"))
	m2 := updated.(gatekeeperModel)
	if m2.pending == nil {
		t.Fatal("D should set a pending confirmation")
	}
	if cmd != nil {
		t.Error("D should not run the delete before confirmation")
	}
	if !strings.Contains(m2.pending.prompt, "team-0") {
		t.Errorf("confirm prompt should name the team, got %q", m2.pending.prompt)
	}
}

func TestGKModel_Delete_Confirm_Runs(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, "")
	setupCLI(t, srv)

	m := newGatekeeperModel()
	m = applyGK(m, gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m = applyGK(m, keyRunes("D")) // arm confirmation

	_, cmd := m.Update(keyRunes("y"))
	if cmd == nil {
		t.Fatal("y should run the pending delete")
	}
	cmd()
	if rec.Method != "DELETE" || rec.Path != "/gatekeeper/teams/team-0000" {
		t.Errorf("request = %s %s, want DELETE /gatekeeper/teams/team-0000", rec.Method, rec.Path)
	}
}

func TestGKModel_Delete_Cancel(t *testing.T) {
	m := newGatekeeperModel()
	m = applyGK(m, gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m = applyGK(m, keyRunes("D"))

	updated, cmd := m.Update(keyRunes("n"))
	m2 := updated.(gatekeeperModel)
	if m2.pending != nil {
		t.Error("a non-y key should clear the pending confirmation")
	}
	if cmd != nil {
		t.Error("cancelling should not run the delete")
	}
}

func TestGKModel_ActionMsg_Success_Refetches(t *testing.T) {
	m := newGatekeeperModel()
	updated, cmd := m.Update(gkActionMsg{section: gkInvites, label: "accept"})
	m2 := updated.(gatekeeperModel)
	if !strings.Contains(m2.status, "accept") {
		t.Errorf("status should mention the action, got %q", m2.status)
	}
	if m2.statusErr {
		t.Error("a successful action should not set statusErr")
	}
	if cmd == nil {
		t.Error("a successful action should trigger a refetch")
	}
}

func TestGKModel_ActionMsg_Error_SetsStatus(t *testing.T) {
	m := newGatekeeperModel()
	updated, cmd := m.Update(gkActionMsg{section: gkInvites, label: "accept", err: fmt.Errorf("nope")})
	m2 := updated.(gatekeeperModel)
	if !m2.statusErr {
		t.Error("an action error should set statusErr")
	}
	if cmd != nil {
		t.Error("a failed action should not refetch")
	}
	if m2.err != nil {
		t.Error("an action error should not blow away the whole view")
	}
}

// ── Service-request filter ────────────────────────────────────────────────────

func TestGKModel_SR_FilterCycles(t *testing.T) {
	m := newGatekeeperModel()
	m.section = gkServiceRequests
	m = applyGK(m, gkRecordsMsg{section: gkServiceRequests, records: gkSRRecords(1)})

	updated, cmd := m.Update(keyRunes("f"))
	m2 := updated.(gatekeeperModel)
	if m2.srFilter != "pending" {
		t.Errorf("first 'f' should set filter=pending, got %q", m2.srFilter)
	}
	if cmd == nil {
		t.Error("changing the filter should refetch")
	}
}

func TestGKModel_FilterIgnoredOutsideSR(t *testing.T) {
	m := newGatekeeperModel() // teams
	m = applyGK(m, gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m2 := applyGK(m, keyRunes("f"))
	if m2.srFilter != "" {
		t.Error("'f' outside service requests should not set a filter")
	}
}

func TestGKNextFilter(t *testing.T) {
	cases := map[string]string{"": "pending", "pending": "approved", "approved": "declined", "declined": ""}
	for in, want := range cases {
		if got := gkNextFilter(in); got != want {
			t.Errorf("gkNextFilter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGKListPath_SRFilter(t *testing.T) {
	if p := gkListPath(gkServiceRequests, "pending"); p != "/gatekeeper/service-permission-requests?status=pending" {
		t.Errorf("filtered SR path = %q", p)
	}
	if p := gkListPath(gkServiceRequests, ""); p != "/gatekeeper/service-permission-requests" {
		t.Errorf("unfiltered SR path = %q", p)
	}
	if p := gkListPath(gkTeams, "pending"); p != "/gatekeeper/teams" {
		t.Errorf("teams ignores filter, got %q", p)
	}
}

// ── Forms ─────────────────────────────────────────────────────────────────────

func TestGKModel_N_OpensCreateTeamForm(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m2 := applyGK(m, keyRunes("n"))
	if m2.view != gkViewForm {
		t.Errorf("'n' should open a form, view = %v", m2.view)
	}
	if m2.formKind != gkFormCreateTeam {
		t.Errorf("formKind = %v, want gkFormCreateTeam", m2.formKind)
	}
}

func TestGKModel_N_OnUsers_NoForm(t *testing.T) {
	m := newGatekeeperModel()
	m.section = gkUsers
	m = applyGK(m, gkRecordsMsg{section: gkUsers, records: []gkRecord{}})
	m2 := applyGK(m, keyRunes("n"))
	if m2.view == gkViewForm {
		t.Error("'n' on users (no create) should not open a form")
	}
}

func TestGKModel_I_OpensInviteForm_WithTarget(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m2 := applyGK(m, keyRunes("i"))
	if m2.view != gkViewForm || m2.formKind != gkFormInviteTeam {
		t.Fatalf("'i' should open the team invite form, got view=%v kind=%v", m2.view, m2.formKind)
	}
	if m2.formTargetID != "team-0000" {
		t.Errorf("invite target = %q, want team-0000", m2.formTargetID)
	}
}

func TestGKModel_CreateTeamForm_RequiresName(t *testing.T) {
	m := newGatekeeperModel()
	m.formKind = gkFormCreateTeam
	m.form, _ = newTUIForm("New Team", formInput("name", "Name", ""), formInput("role", "Role", ""))
	m2, cmd := m.submitForm()
	if cmd != nil {
		t.Error("submitting with no name should not emit a cmd")
	}
	if m2.form.errMsg == "" {
		t.Error("submitting with no name should set a form error")
	}
}

func TestGKModel_CreateTeamForm_Submits(t *testing.T) {
	m := newGatekeeperModel()
	m.formKind = gkFormCreateTeam
	m.form, _ = newTUIForm("New Team", formInputDefault("name", "Name", "", "myteam"), formInput("role", "Role", ""))
	_, cmd := m.submitForm()
	if cmd == nil {
		t.Fatal("submitting a valid team form should emit a cmd")
	}
}

func TestGKModel_FormCancel_ReturnsToList(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m = applyGK(m, keyRunes("n"))
	m2 := applyGK(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m2.view != gkViewList {
		t.Error("esc should cancel the form and return to the list")
	}
}

func TestGKModel_FormDoneMsg_Refetches(t *testing.T) {
	m := newGatekeeperModel()
	m.view = gkViewForm
	updated, cmd := m.Update(gkFormDoneMsg{section: gkTeams, status: "✓ team created"})
	m2 := updated.(gatekeeperModel)
	if m2.view != gkViewList {
		t.Error("formDoneMsg should return to the list view")
	}
	if m2.status != "✓ team created" {
		t.Errorf("status = %q", m2.status)
	}
	if cmd == nil {
		t.Error("formDoneMsg should refetch the section")
	}
}

func TestGKModel_FormErrMsg_SetsFormError(t *testing.T) {
	m := newGatekeeperModel()
	m.view = gkViewForm
	m.form, _ = newTUIForm("New Team", formInput("name", "Name", ""))
	m2 := applyGK(m, gkFormErrMsg{err: fmt.Errorf("duplicate")})
	if !strings.Contains(m2.form.errMsg, "duplicate") {
		t.Errorf("form error = %q, want it to mention 'duplicate'", m2.form.errMsg)
	}
}

// ── Create / invite cmd builders ───────────────────────────────────────────────

func TestGKCreateTeam_PostsPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, "{}")
	setupCLI(t, srv)

	msg := gkCreateTeam("ci", "role-1")()
	if rec.Method != "POST" || rec.Path != "/gatekeeper/teams" {
		t.Errorf("request = %s %s, want POST /gatekeeper/teams", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["team_name"] != "ci" || got["role_id"] != "role-1" {
		t.Errorf("body = %v, want team_name=ci role_id=role-1", got)
	}
	if _, ok := msg.(gkFormDoneMsg); !ok {
		t.Errorf("msg = %T, want gkFormDoneMsg", msg)
	}
}

func TestGKSendInvite_PostsEmail(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, "{}")
	setupCLI(t, srv)

	gkSendInvite(gkTeams, "/gatekeeper/teams/team-1/invites", "a@b.com")()
	if rec.Path != "/gatekeeper/teams/team-1/invites" {
		t.Errorf("invite path = %s", rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["email"] != "a@b.com" {
		t.Errorf("invite body = %v, want email=a@b.com", got)
	}
}

// ── Fetch ─────────────────────────────────────────────────────────────────────

func TestGKFetch_Success(t *testing.T) {
	body, _ := json.Marshal(gkInviteRecords(2))
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := gkFetch(gkInvites, "")()
	if rec.Path != "/gatekeeper/invites" {
		t.Errorf("fetch path = %s, want /gatekeeper/invites", rec.Path)
	}
	res, ok := msg.(gkRecordsMsg)
	if !ok {
		t.Fatalf("msg = %T, want gkRecordsMsg", msg)
	}
	if res.section != gkInvites || len(res.records) != 2 {
		t.Errorf("got section=%v len=%d", res.section, len(res.records))
	}
}

func TestGKFetch_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `{"error":"no"}`)
	setupCLI(t, srv)
	if _, ok := gkFetch(gkTeams, "")().(gkErrMsg); !ok {
		t.Error("HTTP error should yield gkErrMsg")
	}
}

func TestGKFetch_SRFilterQuery(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, "[]")
	setupCLI(t, srv)
	gkFetch(gkServiceRequests, "pending")()
	if !strings.Contains(rec.Query, "status=pending") {
		t.Errorf("query = %q, want status=pending", rec.Query)
	}
}

// ── Record helpers ─────────────────────────────────────────────────────────────

func TestGKHelpers(t *testing.T) {
	r := gkRecord{"a": "x", "active": true, "perms": []any{1, 2, 3}}
	if gkStr(r, "a") != "x" || gkStr(r, "missing") != "" {
		t.Error("gkStr")
	}
	if gkActiveDot(r) != "●" || gkActiveDot(gkRecord{}) != "○" {
		t.Error("gkActiveDot")
	}
	if gkCount(r, "perms") != "3" || gkCount(r, "missing") != "0" {
		t.Error("gkCount")
	}
	if gkDash("") != "—" || gkDash("y") != "y" {
		t.Error("gkDash")
	}
	if gkCapitalize("delete") != "Delete" || gkCapitalize("") != "" {
		t.Error("gkCapitalize")
	}
	when := time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC).Format(time.RFC3339)
	if got := gkTimeCell(gkRecord{"t": when}, "t"); got == "" {
		t.Error("gkTimeCell should format a valid RFC3339 string")
	}
	if gkTimeCell(gkRecord{}, "t") != "" {
		t.Error("gkTimeCell of a missing field should be empty")
	}
}

// ── View rendering ─────────────────────────────────────────────────────────────

func TestGKView_Loading(t *testing.T) {
	if !strings.Contains(newGatekeeperModel().View(), "Loading") {
		t.Error("loading view should say Loading")
	}
}

func TestGKView_Error(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkErrMsg{err: fmt.Errorf("connection refused")})
	if !strings.Contains(m.View(), "error") {
		t.Error("error view should contain 'error'")
	}
}

func TestGKView_ListShowsSectionBarAndRows(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	v := m.View()
	if !strings.Contains(v, "Teams") || !strings.Contains(v, "Invites") {
		t.Error("list view should render the section bar")
	}
	if !strings.Contains(v, "team-0") {
		t.Error("list view should render team rows")
	}
}

func TestGKView_Empty(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: []gkRecord{}})
	if !strings.Contains(m.View(), "No teams") {
		t.Errorf("empty view should say 'No teams', got: %q", m.View())
	}
}

func TestGKView_ConfirmPrompt(t *testing.T) {
	m := applyGK(newGatekeeperModel(), gkRecordsMsg{section: gkTeams, records: gkTeamRecords(1)})
	m = applyGK(m, keyRunes("D"))
	if !strings.Contains(m.View(), "confirm") {
		t.Error("armed delete should render a confirm prompt")
	}
}

func TestGKView_SRHelpHasFilter(t *testing.T) {
	m := newGatekeeperModel()
	m.section = gkServiceRequests
	m = applyGK(m, gkRecordsMsg{section: gkServiceRequests, records: gkSRRecords(1)})
	if !strings.Contains(m.View(), "filter") {
		t.Error("service-request help should mention the filter key")
	}
}

// ── Command registration / home wiring ─────────────────────────────────────────

func TestGKCmd_Registered(t *testing.T) {
	// wireModules() only runs from Execute(), so assert against the command var
	// directly rather than rootCmd.
	if gatekeeperCmd.RunE == nil {
		t.Error("gatekeeperCmd.RunE should launch the TUI")
	}
	if findSubcmd(t, gatekeeperCmd, "tui") == nil {
		t.Error("tui subcommand not registered under gatekeeper")
	}
}

// gkScreenIndex returns the hub-menu position of the Gatekeeper screen, or -1.
func gkScreenIndex() int {
	for i, s := range hubScreens() {
		if s.Title == "Gatekeeper" {
			return i
		}
	}
	return -1
}

func TestGKHome_RegisteredAsScreen(t *testing.T) {
	isolateHome(t)
	idx := gkScreenIndex()
	if idx < 0 {
		t.Fatal("Gatekeeper should be registered as a hub screen")
	}
	if _, ok := hubScreens()[idx].New().(gatekeeperModel); !ok {
		t.Error("the Gatekeeper screen's New() should build a gatekeeperModel")
	}
}

func TestGKHome_LaunchOpensModel(t *testing.T) {
	isolateHome(t)
	idx := gkScreenIndex()
	if idx < 0 {
		t.Fatal("Gatekeeper screen not registered")
	}
	updated, _ := newAppModel().Update(launchMsg{idx: idx})
	if _, ok := updated.(appModel).active.(gatekeeperModel); !ok {
		t.Error("launching the Gatekeeper screen should make a gatekeeperModel active")
	}
}
