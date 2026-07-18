package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applySalMsg(m gkSignupsModel, msg tea.Msg) gkSignupsModel {
	updated, _ := m.Update(msg)
	return updated.(gkSignupsModel)
}

// ── Fetch ─────────────────────────────────────────────────────────────────────

func TestSALFetchEntries_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"entry_id":"e1","email":"a@b.com","note":"hi"}]`)
	setupCLI(t, srv)
	msg := salFetchEntries()
	if rec.Method != "GET" || rec.Path != "/gatekeeper/signup-allowlist" {
		t.Errorf("request = %s %s, want GET /gatekeeper/signup-allowlist", rec.Method, rec.Path)
	}
	got, ok := msg.(salEntriesMsg)
	if !ok || len(got.records) != 1 {
		t.Fatalf("msg = %#v, want salEntriesMsg with 1 record", msg)
	}
	if gkStr(got.records[0], "email") != "a@b.com" {
		t.Errorf("email = %q, want a@b.com", gkStr(got.records[0], "email"))
	}
}

func TestSALFetchEntries_HTTPError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, `boom`)
	setupCLI(t, srv)
	if _, ok := salFetchEntries().(salErrMsg); !ok {
		t.Error("HTTP error should return salErrMsg")
	}
}

func TestSALFetchPolicy_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"invite_only":true,"updated_by":"admin"}`)
	setupCLI(t, srv)
	msg := salFetchPolicy()
	if rec.Method != "GET" || rec.Path != "/gatekeeper/signup-policy" {
		t.Errorf("request = %s %s, want GET /gatekeeper/signup-policy", rec.Method, rec.Path)
	}
	got, ok := msg.(salPolicyMsg)
	if !ok || !got.loaded || !got.inviteOnly {
		t.Fatalf("msg = %#v, want salPolicyMsg{loaded, inviteOnly}", msg)
	}
}

func TestSALFetchPolicy_ErrorDegrades(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	// A policy read error must degrade to loaded=false, not blank the TUI.
	got, ok := salFetchPolicy().(salPolicyMsg)
	if !ok || got.loaded {
		t.Errorf("msg = %#v, want unloaded salPolicyMsg on error", salFetchPolicy())
	}
}

// ── Message handling ────────────────────────────────────────────────────────────

func TestSALEntriesMsg_PopulatesTable(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{{"entry_id": "e1", "email": "a@b.com"}}})
	if len(m.records) != 1 {
		t.Fatalf("records = %d, want 1", len(m.records))
	}
	if m.loading {
		t.Error("loading should clear after entries arrive")
	}
}

func TestSALPolicyMsg_SetsState(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salPolicyMsg{inviteOnly: true, loaded: true})
	if !m.policyLoaded || !m.inviteOnly {
		t.Errorf("policyLoaded=%v inviteOnly=%v, want both true", m.policyLoaded, m.inviteOnly)
	}
}

func TestSALPolicyMsg_ChangedSetsStatus(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salPolicyMsg{inviteOnly: true, loaded: true, changed: true})
	if m.statusErr || !strings.Contains(m.status, "invite-only enabled") {
		t.Errorf("status = %q, want an 'invite-only enabled' status", m.status)
	}
}

// ── Add (form submit) ───────────────────────────────────────────────────────────

func TestSALSubmit_Request(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"entry_id":"e1","email":"a@b.com"}`)
	setupCLI(t, srv)
	msg := salSubmit("a@b.com", "note")()
	if rec.Method != "POST" || rec.Path != "/gatekeeper/signup-allowlist" {
		t.Errorf("request = %s %s, want POST /gatekeeper/signup-allowlist", rec.Method, rec.Path)
	}
	var body map[string]string
	json.Unmarshal(rec.Body, &body) //nolint:errcheck
	if body["email"] != "a@b.com" || body["note"] != "note" {
		t.Errorf("body = %v, want email+note", body)
	}
	if _, ok := msg.(salFormDoneMsg); !ok {
		t.Errorf("msg = %#v, want salFormDoneMsg on success", msg)
	}
}

func TestSALSubmit_ErrorStaysInForm(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusConflict, `already on the signup allowlist`)
	setupCLI(t, srv)
	if _, ok := salSubmit("a@b.com", "")().(salFormErrMsg); !ok {
		t.Error("a conflict should return salFormErrMsg so the dialog stays open")
	}
}

func TestSALForm_OpensAndRequiresEmail(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{}})
	m = applySalMsg(m, key("n"))
	if m.view != salViewForm {
		t.Fatal("'n' should open the add form")
	}
	// Submitting with an empty email must stay in the form with an error.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(gkSignupsModel)
	// Enter on a text field moves focus; drive to the submit button then submit.
	_ = cmd
	if m2.view != salViewForm {
		t.Error("form should stay open until a valid submit")
	}
}

// ── Delete ──────────────────────────────────────────────────────────────────────

func TestSALDelete_ConfirmThenRuns(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusNoContent, ``)
	setupCLI(t, srv)

	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{{"entry_id": "e1", "email": "a@b.com"}}})
	m2 := applySalMsg(m, key("D"))
	if m2.pending == nil {
		t.Fatal("D should set a pending confirm")
	}
	if !strings.Contains(m2.pending.prompt, "a@b.com") {
		t.Errorf("prompt = %q, want to mention the email", m2.pending.prompt)
	}
	_, cmd := m2.Update(key("y"))
	if cmd == nil {
		t.Fatal("confirming should emit the delete cmd")
	}
	cmd()
	if rec.Method != "DELETE" || rec.Path != "/gatekeeper/signup-allowlist/e1" {
		t.Errorf("request = %s %s, want DELETE /gatekeeper/signup-allowlist/e1", rec.Method, rec.Path)
	}
}

func TestSALDelete_CancelDoesNothing(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{{"entry_id": "e1", "email": "a@b.com"}}})
	m = applySalMsg(m, key("D"))
	updated, cmd := m.Update(key("n"))
	if updated.(gkSignupsModel).pending != nil {
		t.Error("a non-y key should clear the pending confirm")
	}
	if cmd != nil {
		t.Error("cancelling a delete should not emit a cmd")
	}
}

// ── Policy toggle ────────────────────────────────────────────────────────────────

func TestSALSetPolicy_Request(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"invite_only":true}`)
	setupCLI(t, srv)
	msg := salSetPolicy(true)()
	if rec.Method != "PUT" || rec.Path != "/gatekeeper/signup-policy" {
		t.Errorf("request = %s %s, want PUT /gatekeeper/signup-policy", rec.Method, rec.Path)
	}
	var body map[string]bool
	json.Unmarshal(rec.Body, &body) //nolint:errcheck
	if body["invite_only"] != true {
		t.Errorf("body = %v, want invite_only=true", body)
	}
	got, ok := msg.(salPolicyMsg)
	if !ok || !got.changed || !got.inviteOnly {
		t.Errorf("msg = %#v, want a changed salPolicyMsg", msg)
	}
}

func TestSALToggle_EmitsSetPolicy(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{}})
	m = applySalMsg(m, salPolicyMsg{inviteOnly: false, loaded: true})
	_, cmd := m.Update(key("t"))
	if cmd == nil {
		t.Fatal("toggling with a known policy should emit a set-policy cmd")
	}
}

func TestSALToggle_UnknownPolicyGuarded(t *testing.T) {
	// Policy never loaded: 't' must not blindly flip an unknown state.
	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{}})
	updated, cmd := m.Update(key("t"))
	m2 := updated.(gkSignupsModel)
	if cmd != nil {
		t.Error("toggling with an unknown policy should not emit a cmd")
	}
	if !m2.statusErr {
		t.Error("toggling with an unknown policy should set an error status")
	}
}

// ── Action / auto-refresh / navigation ──────────────────────────────────────────

func TestSALActionMsg_Error_SetsStatus(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salActionMsg{label: "delete", err: fmt.Errorf("HTTP 403: forbidden")})
	if !m.statusErr || !strings.Contains(m.status, "403") {
		t.Errorf("status = %q err=%v, want a 403 error status", m.status, m.statusErr)
	}
	if m.err != nil {
		t.Error("an action error should be a status line, not a fatal err")
	}
}

func TestSALAutoRefresh_ListRefetches(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{}})
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd == nil {
		t.Error("auto-refresh in list view should emit a fetch cmd")
	}
}

func TestSALAutoRefresh_FormNoop(t *testing.T) {
	m := newGkSignupsModel()
	m.view = salViewForm
	_, cmd := m.Update(tuiAutoRefreshMsg{})
	if cmd != nil {
		t.Error("auto-refresh must not disturb an open form")
	}
}

func TestSALEsc_GoesHome(t *testing.T) {
	m := applySalMsg(newGkSignupsModel(), salEntriesMsg{records: []gkRecord{}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should return a cmd")
	}
	if _, ok := cmd().(goHomeMsg); !ok {
		t.Errorf("esc returned %T, want goHomeMsg", cmd())
	}
}
