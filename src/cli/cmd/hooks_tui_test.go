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

func applyHooksMsg(m hooksModel, msg tea.Msg) hooksModel {
	updated, _ := m.Update(msg)
	return updated.(hooksModel)
}

// ── Initial state ─────────────────────────────────────────────────────────────

func TestHooksModel_InitialState(t *testing.T) {
	m := newHooksModel()
	if m.view != hooksViewRules {
		t.Errorf("initial view = %v, want hooksViewRules", m.view)
	}
	if !m.loading {
		t.Error("loading should be true on startup")
	}
	if m.err != nil {
		t.Errorf("initial err = %v, want nil", m.err)
	}
	if m.rules != nil {
		t.Error("initial rules should be nil")
	}
	if m.confirmDel {
		t.Error("initial confirmDel should be false")
	}
}

// ── Messages: hookRulesMsg ────────────────────────────────────────────────────

func TestHooksModel_RulesMsg_Populates(t *testing.T) {
	rules := []hookRule{
		{RuleID: "r1", Name: "ci", Repo: "org/repo", Events: []string{"push"}, Active: true},
		{RuleID: "r2", Name: "pr", Repo: "org/repo", Events: []string{"pull_request"}, Active: false},
	}
	m := applyHooksMsg(newHooksModel(), hookRulesMsg(rules))

	if m.loading {
		t.Error("loading should be false after rulesMsg")
	}
	if len(m.rules) != 2 {
		t.Fatalf("len(rules) = %d, want 2", len(m.rules))
	}
	if m.rules[0].RuleID != "r1" {
		t.Errorf("rules[0].RuleID = %q, want r1", m.rules[0].RuleID)
	}
}

func TestHooksModel_RulesMsg_Empty(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	if m.loading {
		t.Error("loading should be false after empty rulesMsg")
	}
}

// ── Messages: hookEventsMsg ───────────────────────────────────────────────────

func TestHooksModel_EventsMsg_Populates(t *testing.T) {
	events := []hookEvent{
		{EventID: "evt-1", Repo: "org/repo", EventType: "push", RulesMatched: 1},
		{EventID: "evt-2", Repo: "org/repo", EventType: "pull_request", RulesMatched: 0},
	}
	m := newHooksModel()
	m.view = hooksViewEvents
	m2 := applyHooksMsg(m, hookEventsMsg(events))

	if m2.loading {
		t.Error("loading should be false after eventsMsg")
	}
	if len(m2.events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(m2.events))
	}
	if m2.events[0].EventID != "evt-1" {
		t.Errorf("events[0].EventID = %q, want evt-1", m2.events[0].EventID)
	}
}

// ── Messages: hookEventDetailMsg ──────────────────────────────────────────────

func TestHooksModel_EventDetailMsg_SetsEventAndSwitchesView(t *testing.T) {
	e := hookEvent{
		EventID:   "evt-abc",
		EventType: "push",
		Repo:      "org/repo",
		Status:    "received",
	}
	m := newHooksModel()
	m.view = hooksViewEvents
	m2 := applyHooksMsg(m, hookEventDetailMsg(e))

	if m2.view != hooksViewEventDetail {
		t.Errorf("view = %v, want hooksViewEventDetail", m2.view)
	}
	if m2.loading {
		t.Error("loading should be false after eventDetailMsg")
	}
	if m2.selEvent == nil || m2.selEvent.EventID != "evt-abc" {
		t.Errorf("selEvent = %v, want evt-abc", m2.selEvent)
	}
}

// ── Messages: hooksErrMsg ─────────────────────────────────────────────────────

func TestHooksModel_ErrMsg_SetsError(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hooksErrMsg{err: fmt.Errorf("api unreachable")})
	if m.err == nil {
		t.Fatal("err should be set after errMsg")
	}
	if m.loading {
		t.Error("loading should be false after errMsg")
	}
	if !strings.Contains(m.err.Error(), "api unreachable") {
		t.Errorf("err = %v", m.err)
	}
}

// ── Messages: hookDeletedMsg ──────────────────────────────────────────────────

func TestHooksModel_DeletedMsg_ClearsSelRuleAndRefetches(t *testing.T) {
	m := newHooksModel()
	m.selRule = &hookRule{RuleID: "r1", Name: "ci"}
	updated, cmd := m.Update(hookDeletedMsg{})
	m2 := updated.(hooksModel)

	if m2.selRule != nil {
		t.Error("selRule should be cleared after deletion")
	}
	if !m2.loading {
		t.Error("loading should be true while re-fetching rules")
	}
	if cmd == nil {
		t.Error("hookDeletedMsg should return a fetch cmd")
	}
}

// ── Messages: WindowSizeMsg ───────────────────────────────────────────────────

func TestHooksModel_WindowResize(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.vp.Width != 116 {
		t.Errorf("vp.Width = %d, want 116", m.vp.Width)
	}
	if m.vp.Height != 32 {
		t.Errorf("vp.Height = %d, want 32", m.vp.Height)
	}
}

// ── Keys: rules view ─────────────────────────────────────────────────────────

func TestHooksModel_Rules_Esc_GoesHome(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc key should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("esc key returned %T, want goHomeMsg", cmd())
	}
}

func TestHooksModel_Rules_CtrlC_Quits(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should return a cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should return QuitMsg")
	}
}

func TestHooksModel_Rules_Enter_NavigatesToEvents(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)

	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{
		{RuleID: "r1", Name: "ci", Repo: "org/repo"},
	}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(hooksModel)

	if m2.view != hooksViewEvents {
		t.Errorf("view = %v, want hooksViewEvents", m2.view)
	}
	if m2.selRule == nil || m2.selRule.RuleID != "r1" {
		t.Errorf("selRule = %v, want r1", m2.selRule)
	}
	if !m2.loading {
		t.Error("loading should be true while fetching events")
	}
	if cmd == nil {
		t.Error("enter should emit a fetch cmd")
	}
}

func TestHooksModel_Rules_Enter_Noop_WhenEmpty(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(hooksModel).view != hooksViewRules {
		t.Error("enter with no rules should stay in rules view")
	}
}

func TestHooksModel_Rules_A_LoadsAllEvents(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)

	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m2 := updated.(hooksModel)

	if m2.view != hooksViewEvents {
		t.Errorf("a key: view = %v, want hooksViewEvents", m2.view)
	}
	if m2.selRule != nil {
		t.Error("a key should not set selRule (shows all events)")
	}
	if cmd == nil {
		t.Fatal("a key should emit a fetch cmd")
	}
	cmd()
	// No ?repo= query parameter should be in the URL.
	if strings.Contains(rec.Query, "repo=") {
		t.Errorf("a key should fetch events without a repo filter; query = %q", rec.Query)
	}
}

func TestHooksModel_Rules_E_OpensPrefilledEditForm(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{
		{RuleID: "r1", Name: "ci-push", Repo: "org/repo", Events: []string{"push"}, WorkflowID: "wf1", RefFilter: "refs/heads/main", InputMapping: map[string]string{"k": "v"}},
	}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	m2 := updated.(hooksModel)
	if m2.view != hooksViewCreate || m2.formMode != hooksFormEdit || m2.editRuleID != "r1" {
		t.Fatalf("e on a rule should open the edit form (view=%v mode=%v id=%q)", m2.view, m2.formMode, m2.editRuleID)
	}
	if m2.form.value("name") != "ci-push" || m2.form.value("repo") != "org/repo" {
		t.Errorf("edit form not pre-filled: name=%q repo=%q", m2.form.value("name"), m2.form.value("repo"))
	}
	// input_mapping is preserved out-of-band so the edit can't wipe it.
	if m2.editInputMapping["k"] != "v" {
		t.Errorf("editInputMapping = %v, want preserved {k:v}", m2.editInputMapping)
	}
}

func TestHooksModel_Rules_D_SetsConfirmDel(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{
		{RuleID: "r1", Name: "ci"},
	}))
	m2 := applyHooksMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if !m2.confirmDel {
		t.Error("D key should set confirmDel=true")
	}
	if m2.selRule == nil || m2.selRule.RuleID != "r1" {
		t.Errorf("selRule = %v, want r1", m2.selRule)
	}
}

func TestHooksModel_Rules_D_Y_Deletes(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)

	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{
		{RuleID: "rule-abc", Name: "ci"},
	}))
	m = applyHooksMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if !m.confirmDel {
		t.Fatal("D should set confirmDel=true")
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("y should emit a delete cmd")
	}
	msg := cmd()
	if _, ok := msg.(hookDeletedMsg); !ok {
		t.Errorf("delete cmd returned %T, want hookDeletedMsg", msg)
	}
	if rec.Method != "DELETE" {
		t.Errorf("method = %q, want DELETE", rec.Method)
	}
	if !strings.Contains(rec.Path, "rule-abc") {
		t.Errorf("path = %q, want to contain rule-abc", rec.Path)
	}
}

func TestHooksModel_Rules_D_OtherKey_CancelsConfirm(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{
		{RuleID: "r1", Name: "ci"},
	}))
	m = applyHooksMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if !m.confirmDel {
		t.Fatal("D should set confirmDel=true")
	}
	// Any key other than y/Y cancels the deletion.
	m2 := applyHooksMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m2.confirmDel {
		t.Error("n key should cancel confirmDel")
	}
}

func TestHooksModel_Rules_R_Refreshes(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Error("r key should emit a refresh cmd")
	}
}

// ── Keys: events view ─────────────────────────────────────────────────────────

func TestHooksModel_Events_Esc_Back(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEvents
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := updated.(hooksModel)
	if m2.view != hooksViewRules {
		t.Errorf("esc: view = %v, want hooksViewRules", m2.view)
	}
	if cmd != nil {
		t.Error("esc should not emit a cmd")
	}
}

func TestHooksModel_Events_Enter_NavigatesToDetail(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK,
		`{"event_id":"evt-abc","event_type":"push","repo":"org/repo","status":"received"}`)
	setupCLI(t, srv)

	m := newHooksModel()
	m.view = hooksViewEvents
	m = applyHooksMsg(m, hookEventsMsg([]hookEvent{
		{EventID: "evt-abc", EventType: "push", Repo: "org/repo"},
	}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(hooksModel)

	if m2.view != hooksViewEventDetail {
		t.Errorf("view = %v, want hooksViewEventDetail", m2.view)
	}
	if !m2.loading {
		t.Error("loading should be true while fetching event detail")
	}
	if cmd == nil {
		t.Error("enter should emit a fetch cmd")
	}
}

func TestHooksModel_Events_Enter_Noop_WhenEmpty(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEvents
	m = applyHooksMsg(m, hookEventsMsg([]hookEvent{}))
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(hooksModel).view == hooksViewEventDetail {
		t.Error("enter with no events should not switch to the event-detail view")
	}
}

func TestHooksModel_Events_R_Refreshes_WithRepoFilter(t *testing.T) {
	mux := http.NewServeMux()
	var gotQuery string
	mux.HandleFunc("/hooks/events", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]")) //nolint:errcheck
	})
	srv := routeServer(t, mux)
	setupCLI(t, srv)

	m := newHooksModel()
	m.view = hooksViewEvents
	m.selRule = &hookRule{RuleID: "r1", Repo: "org/myrepo"}
	m = applyHooksMsg(m, hookEventsMsg([]hookEvent{}))

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("r should emit a fetch cmd")
	}
	cmd()
	if !strings.Contains(gotQuery, "myrepo") {
		t.Errorf("query = %q, want to contain repo filter", gotQuery)
	}
}

// ── Keys: event detail view ───────────────────────────────────────────────────

func TestHooksModel_EventDetail_Esc_Back(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEventDetail
	m.selEvent = &hookEvent{EventID: "evt-1"}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := updated.(hooksModel)
	if m2.view != hooksViewEvents {
		t.Errorf("esc: view = %v, want hooksViewEvents", m2.view)
	}
	if m2.selEvent != nil {
		t.Error("selEvent should be cleared on back")
	}
}

// ── Keys: error state ─────────────────────────────────────────────────────────

func TestHooksModel_Error_Esc_GoesHome(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hooksErrMsg{err: fmt.Errorf("boom")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in error state should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Error("esc in error state should return goHomeMsg")
	}
}

func TestHooksModel_Error_R_Retries(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hooksErrMsg{err: fmt.Errorf("boom")})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m2 := updated.(hooksModel)
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

func TestHooksView_Loading(t *testing.T) {
	v := newHooksModel().View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("loading view should say Loading, got: %q", v)
	}
}

func TestHooksView_Error(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hooksErrMsg{err: fmt.Errorf("connection refused")})
	v := m.View()
	if !strings.Contains(v, "error") {
		t.Errorf("error view should contain 'error', got: %q", v)
	}
}

func TestHooksView_RulesEmpty(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	v := m.View()
	if !strings.Contains(v, "No rules") {
		t.Errorf("empty rules view should say 'No rules', got: %q", v)
	}
}

func TestHooksView_RulesNonEmpty_ShowsName(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{
		{RuleID: "r1", Name: "build-ci", Repo: "org/repo", Events: []string{"push"}, Active: true},
	}))
	v := m.View()
	if !strings.Contains(v, "build-ci") {
		t.Errorf("rules view should show rule name, got: %q", v)
	}
}

func TestHooksView_ConfirmDelete_ShowsPrompt(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{
		{RuleID: "r1", Name: "ci"},
	}))
	m = applyHooksMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	v := m.View()
	if !strings.Contains(v, "Delete") || !strings.Contains(v, "confirm") {
		t.Errorf("confirm delete view should show 'Delete' and 'confirm', got: %q", v)
	}
}

func TestHooksView_EventsEmpty(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEvents
	m = applyHooksMsg(m, hookEventsMsg([]hookEvent{}))
	v := m.View()
	if !strings.Contains(v, "No events") {
		t.Errorf("empty events view should say 'No events', got: %q", v)
	}
}

func TestHooksView_EventsNonEmpty_ShowsType(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEvents
	m = applyHooksMsg(m, hookEventsMsg([]hookEvent{
		{EventID: "evt-1", EventType: "pull_request", Repo: "org/repo"},
	}))
	v := m.View()
	if !strings.Contains(v, "pull_request") {
		t.Errorf("events view should show event type, got: %q", v)
	}
}

func TestHooksView_EventsSubtitle_WithRepo(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEvents
	m.selRule = &hookRule{RuleID: "r1", Repo: "myorg/myrepo"}
	m = applyHooksMsg(m, hookEventsMsg([]hookEvent{}))
	v := m.View()
	if !strings.Contains(v, "myorg/myrepo") {
		t.Errorf("events view should show repo in title when filtered, got: %q", v)
	}
}

func TestHooksView_EventDetailLoading(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEventDetail
	m.loading = true
	v := m.View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("event detail loading view should say Loading, got: %q", v)
	}
}

func TestHooksView_RulesHelpText(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	v := m.View()
	if !strings.Contains(v, "delete") || !strings.Contains(v, "home") {
		t.Errorf("rules help should mention delete and home, got: %q", v)
	}
}

// ── hooksRenderEventDetail ────────────────────────────────────────────────────

func TestHooksRenderEventDetail_BasicFields(t *testing.T) {
	e := hookEvent{
		EventID:      "evt-abc",
		Repo:         "org/repo",
		EventType:    "push",
		Ref:          "refs/heads/main",
		Status:       "received",
		RulesMatched: 2,
		CreatedAt:    time.Time{},
	}
	out := hooksRenderEventDetail(e, nil)
	if !strings.Contains(out, "evt-abc") {
		t.Errorf("output should contain event ID, got: %q", out)
	}
	if !strings.Contains(out, "org/repo") {
		t.Errorf("output should contain repo, got: %q", out)
	}
	if !strings.Contains(out, "push") {
		t.Errorf("output should contain event type, got: %q", out)
	}
}

func TestHooksRenderEventDetail_NoTriggers(t *testing.T) {
	e := hookEvent{EventID: "e1", Triggers: nil}
	out := hooksRenderEventDetail(e, nil)
	if strings.Contains(out, "TRIGGERS") {
		t.Error("should not show TRIGGERS section when there are none")
	}
}

func TestHooksRenderEventDetail_WithTriggers(t *testing.T) {
	runID := "run-xyz"
	errMsg := "workflow not found"
	e := hookEvent{
		EventID: "evt-1",
		Triggers: []hookTrigger{
			{
				TriggerID:  "t1",
				RuleID:     "rule-abc",
				WorkflowID: "wf-xyz",
				Status:     "dispatched",
				RunID:      &runID,
			},
			{
				TriggerID:  "t2",
				RuleID:     "rule-def",
				WorkflowID: "wf-nope",
				Status:     "failed",
				Error:      &errMsg,
			},
		},
	}
	out := hooksRenderEventDetail(e, nil)
	if !strings.Contains(out, "TRIGGERS") {
		t.Error("output should contain TRIGGERS section")
	}
	if !strings.Contains(out, "dispatched") {
		t.Error("output should show trigger status")
	}
	if !strings.Contains(out, "run-xyz") {
		t.Error("output should show run ID when present")
	}
	if !strings.Contains(out, "workflow not found") {
		t.Error("output should show error message when present")
	}
}

// ── Fetch functions ───────────────────────────────────────────────────────────

func TestHooksFetchRules_Success(t *testing.T) {
	rules := []hookRule{{RuleID: "r1", Name: "ci", Repo: "org/repo"}}
	body, _ := json.Marshal(rules)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := hooksFetchRules()

	if rec.Method != "GET" || rec.Path != "/hooks/rules" {
		t.Errorf("request = %s %s, want GET /hooks/rules", rec.Method, rec.Path)
	}
	result, ok := msg.(hookRulesMsg)
	if !ok {
		t.Fatalf("msg type = %T, want hookRulesMsg", msg)
	}
	if len(result) != 1 || result[0].RuleID != "r1" {
		t.Errorf("result = %v", result)
	}
}

func TestHooksFetchRules_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `{"error":"forbidden"}`)
	setupCLI(t, srv)
	if _, ok := hooksFetchRules().(hooksErrMsg); !ok {
		t.Error("HTTP error should return hooksErrMsg")
	}
}

func TestHooksFetchRules_InvalidJSON(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `not json`)
	setupCLI(t, srv)
	if _, ok := hooksFetchRules().(hooksErrMsg); !ok {
		t.Error("malformed JSON should return hooksErrMsg")
	}
}

func TestHooksFetchEvents_NoRepo(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)

	hooksFetchEvents("")()

	if rec.Path != "/hooks/events" {
		t.Errorf("path = %q, want /hooks/events", rec.Path)
	}
}

func TestHooksFetchEvents_WithRepo(t *testing.T) {
	mux := http.NewServeMux()
	var gotQuery string
	mux.HandleFunc("/hooks/events", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]")) //nolint:errcheck
	})
	srv := routeServer(t, mux)
	setupCLI(t, srv)

	hooksFetchEvents("myorg/myrepo")()

	if !strings.Contains(gotQuery, "myorg") {
		t.Errorf("query = %q, want to contain repo param", gotQuery)
	}
}

func TestHooksFetchEventDetail_Success(t *testing.T) {
	e := hookEvent{EventID: "evt-abc", EventType: "push", Repo: "org/repo"}
	body, _ := json.Marshal(e)
	srv, rec := recordingServer(t, http.StatusOK, string(body))
	setupCLI(t, srv)

	msg := hooksFetchEventDetail("evt-abc")()

	if rec.Method != "GET" || rec.Path != "/hooks/events/evt-abc" {
		t.Errorf("request = %s %s, want GET /hooks/events/evt-abc", rec.Method, rec.Path)
	}
	result, ok := msg.(hookEventDetailMsg)
	if !ok {
		t.Fatalf("msg type = %T, want hookEventDetailMsg", msg)
	}
	if result.EventID != "evt-abc" {
		t.Errorf("EventID = %q, want evt-abc", result.EventID)
	}
}

func TestHooksFetchEventDetail_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusNotFound, `{"error":"not found"}`)
	setupCLI(t, srv)
	if _, ok := hooksFetchEventDetail("nope")().(hooksErrMsg); !ok {
		t.Error("HTTP error should return hooksErrMsg")
	}
}

func TestHooksDeleteRule_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)

	msg := hooksDeleteRule("rule-xyz")()

	if rec.Method != "DELETE" || rec.Path != "/hooks/rules/rule-xyz" {
		t.Errorf("request = %s %s, want DELETE /hooks/rules/rule-xyz", rec.Method, rec.Path)
	}
	if _, ok := msg.(hookDeletedMsg); !ok {
		t.Errorf("msg type = %T, want hookDeletedMsg", msg)
	}
}

func TestHooksDeleteRule_HTTPError_ReturnsErrMsg(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `{"error":"forbidden"}`)
	setupCLI(t, srv)
	msg := hooksDeleteRule("rule-xyz")()
	if _, ok := msg.(hooksErrMsg); !ok {
		t.Errorf("msg type = %T, want hooksErrMsg on delete failure", msg)
	}
}

// ── Create flow ───────────────────────────────────────────────────────────────

func TestHooksModel_Rules_N_OpensCreateForm(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m2 := updated.(hooksModel)
	if m2.view != hooksViewCreate {
		t.Errorf("view = %v, want hooksViewCreate", m2.view)
	}
	if cmd == nil {
		t.Error("opening the form should return a focus/blink cmd")
	}
	if len(m2.form.fields) == 0 {
		t.Error("create form should have fields")
	}
}

func TestHooksWorkflowField_SelectorWhenPipelinesKnown(t *testing.T) {
	f := hooksWorkflowField([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "build-ci"},
		{WorkflowID: "wf-2", Name: "deploy"},
	})
	if f.kind != fieldSelect {
		t.Fatalf("workflow field kind = %v, want fieldSelect", f.kind)
	}
	if f.options[0] != "build-ci" || f.values[0] != "wf-1" {
		t.Errorf("first option = %q/%q, want build-ci/wf-1", f.options[0], f.values[0])
	}
}

func TestHooksWorkflowField_TextFallbackWhenEmpty(t *testing.T) {
	if f := hooksWorkflowField(nil); f.kind != fieldText {
		t.Errorf("workflow field with no pipelines should fall back to text, got kind %v", f.kind)
	}
}

func TestHooksModel_Create_WorkflowSelectorSubmitsID(t *testing.T) {
	m := newHooksModel()
	m.pipelines = []tuiPipeline{{WorkflowID: "wf-xyz", Name: "build-ci"}}
	m.form, _ = newHooksRuleForm(m.pipelines)
	// The workflow field reads back the id, not the displayed name.
	if got := m.form.value("workflow"); got != "wf-xyz" {
		t.Errorf("workflow value = %q, want wf-xyz (the id)", got)
	}
}

func TestHooksModel_PipelinesMsg_Populates(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookPipelinesMsg([]tuiPipeline{
		{WorkflowID: "wf-1", Name: "build-ci"},
	}))
	if len(m.pipelines) != 1 || m.pipelines[0].WorkflowID != "wf-1" {
		t.Errorf("pipelines = %v, want one wf-1", m.pipelines)
	}
}

func TestHooksRenderEventDetail_ShowsWorkflowName(t *testing.T) {
	e := hookEvent{
		EventID:  "evt-1",
		Triggers: []hookTrigger{{TriggerID: "t1", RuleID: "rule-abc", WorkflowID: "wf-xyz", Status: "dispatched"}},
	}
	out := hooksRenderEventDetail(e, map[string]string{"wf-xyz": "build-ci"})
	if !strings.Contains(out, "build-ci") {
		t.Errorf("trigger should render the pipeline name, got: %q", out)
	}
}

func TestHooksModel_Create_Esc_BacksToRules(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewCreate
	m.form, _ = newHooksRuleForm(nil)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(hooksModel).view != hooksViewRules {
		t.Error("esc in create view should return to the rules list")
	}
}

func TestHooksModel_Create_Submit_MissingFields_StaysWithError(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewCreate
	m.form, _ = newHooksRuleForm(nil)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m2 := updated.(hooksModel)
	if m2.view != hooksViewCreate {
		t.Error("submit with empty fields should stay in the create view")
	}
	if m2.form.errMsg == "" {
		t.Error("submit with empty fields should set an inline error")
	}
	if cmd != nil {
		t.Error("invalid submit should not emit a request cmd")
	}
}

func TestHooksModel_RuleCreatedMsg_ReturnsToRulesAndRefetches(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewCreate
	updated, cmd := m.Update(hookRuleCreatedMsg{})
	m2 := updated.(hooksModel)
	if m2.view != hooksViewRules {
		t.Error("hookRuleCreatedMsg should return to the rules view")
	}
	if !m2.loading || cmd == nil {
		t.Error("hookRuleCreatedMsg should set loading and emit a refetch cmd")
	}
}

func TestHooksModel_FormErrMsg_ShowsInlineError(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewCreate
	m.form, _ = newHooksRuleForm(nil)
	updated, _ := m.Update(hooksFormErrMsg{err: fmt.Errorf("workflow_id not found")})
	m2 := updated.(hooksModel)
	if m2.view != hooksViewCreate {
		t.Error("form error should stay in the create view")
	}
	if !strings.Contains(m2.form.errMsg, "workflow_id not found") {
		t.Errorf("form.errMsg = %q, want to contain 'workflow_id not found'", m2.form.errMsg)
	}
}

func TestHooksSubmitRule_PostsCorrectPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"rule_id":"r1"}`)
	setupCLI(t, srv)

	msg := hooksSubmitRule(hooksFormCreate, "", "ci-push", "org/repo", []string{"push", "tag"},
		"wf-1", "s3cr3t", "refs/heads/main", map[string]string{})()

	if _, ok := msg.(hookRuleCreatedMsg); !ok {
		t.Fatalf("msg = %T, want hookRuleCreatedMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/hooks/rules" {
		t.Errorf("request = %s %s, want POST /hooks/rules", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if got["name"] != "ci-push" || got["source"] != "org/repo" {
		t.Errorf("name/source = %v/%v, want ci-push/org/repo", got["name"], got["source"])
	}
	if got["workflow_id"] != "wf-1" || got["secret"] != "s3cr3t" {
		t.Errorf("workflow_id/secret = %v/%v, want wf-1/s3cr3t", got["workflow_id"], got["secret"])
	}
	if got["ref_filter"] != "refs/heads/main" {
		t.Errorf("ref_filter = %v, want refs/heads/main", got["ref_filter"])
	}
	events, _ := got["events"].([]any)
	if len(events) != 2 || events[0] != "push" {
		t.Errorf("events = %v, want [push tag]", got["events"])
	}
}

func TestHooksSubmitRule_HTTPError_ReturnsFormErr(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusUnprocessableEntity, `{"error":"workflow_id not found"}`)
	setupCLI(t, srv)
	msg := hooksSubmitRule(hooksFormCreate, "", "n", "r", []string{"push"}, "bad", "s", "", map[string]string{})()
	if _, ok := msg.(hooksFormErrMsg); !ok {
		t.Errorf("msg = %T, want hooksFormErrMsg on HTTP error", msg)
	}
}

func TestHooksView_CreateView_RendersForm(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewCreate
	m.form, _ = newHooksRuleForm(nil)
	if !strings.Contains(m.View(), "New Hook Rule") {
		t.Error("create view should show the form heading")
	}
}

func TestHooksSubmitRule_EditPutsAndKeepsSecretAndMapping(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"rule_id":"r1"}`)
	setupCLI(t, srv)

	// Edit with a blank secret: the secret key must be omitted (server keeps it),
	// and the preserved input_mapping must be sent so it isn't wiped.
	msg := hooksSubmitRule(hooksFormEdit, "r1", "ci", "org/repo", []string{"push"},
		"wf-1", "", "", map[string]string{"k": "v"})()
	if _, ok := msg.(hookRuleCreatedMsg); !ok {
		t.Fatalf("msg = %T, want hookRuleCreatedMsg", msg)
	}
	if rec.Method != "PUT" || rec.Path != "/hooks/rules/r1" {
		t.Errorf("request = %s %s, want PUT /hooks/rules/r1", rec.Method, rec.Path)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, present := got["secret"]; present {
		t.Errorf("a blank secret must be omitted on edit so the server keeps it; body=%s", rec.Body)
	}
	mapping, _ := got["input_mapping"].(map[string]any)
	if mapping["k"] != "v" {
		t.Errorf("input_mapping = %v, want preserved {k:v}", got["input_mapping"])
	}
}

func TestHooksView_RulesHelp_MentionsNew(t *testing.T) {
	m := applyHooksMsg(newHooksModel(), hookRulesMsg([]hookRule{}))
	if !strings.Contains(m.View(), "new") {
		t.Error("rules help should mention the new-rule shortcut")
	}
}

// ── Command registration ──────────────────────────────────────────────────────

func TestHooksTUICmd_RegisteredUnderHooks(t *testing.T) {
	if findSubcmd(t, hooksCmd, "tui") == nil {
		t.Error("tui subcommand not registered under hooks")
	}
}

func TestHooksCmd_HasRunE(t *testing.T) {
	if hooksCmd.RunE == nil {
		t.Error("hooksCmd.RunE should be set so 'armory hooks' launches the TUI")
	}
}

// ── Auto-refresh ──────────────────────────────────────────────────────────────

func TestHooksModel_AutoRefresh_RulesEmitsFetch(t *testing.T) {
	m := newHooksModel()
	m.loading = false
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh in the rules view should emit a fetch cmd")
	}
}

func TestHooksModel_AutoRefresh_EventsEmitsFetch(t *testing.T) {
	m := newHooksModel()
	m.view = hooksViewEvents
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh in the events view should emit a fetch cmd")
	}
}

func TestHooksModel_AutoRefresh_DetailAndFormNoop(t *testing.T) {
	for _, v := range []hooksViewID{hooksViewEventDetail, hooksViewCreate} {
		m := newHooksModel()
		m.view = v
		_, cmd := m.Update(tuiAutoRefreshMsg{})
		if cmd != nil {
			t.Errorf("view %d: auto-refresh should be a noop", v)
		}
	}
}
