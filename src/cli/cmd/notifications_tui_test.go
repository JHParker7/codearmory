package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyChanMsg(m notificationsModel, msg tea.Msg) notificationsModel {
	updated, _ := m.Update(msg)
	return updated.(notificationsModel)
}

func TestNotificationsModel_ListPopulates(t *testing.T) {
	m := applyChanMsg(newNotificationsModel(), chanListMsg([]channelRec{
		{ChannelID: "c1", Name: "Team Slack", Type: "slack", Enabled: true, Config: map[string]string{"webhook_url": "x"}},
	}))
	if m.loading {
		t.Error("loading should be false after list")
	}
	v := m.View()
	if !strings.Contains(v, "Team Slack") || !strings.Contains(v, "slack") {
		t.Errorf("list view should show channel name and type, got: %q", v)
	}
}

func TestChanFetchProviders_ExtractsTypes(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[{"type":"slack","label":"Slack"},{"type":"email","label":"Email"}]`)
	setupCLI(t, srv)
	msg, ok := chanFetchProviders().(chanProvidersMsg)
	if !ok || len(msg) != 2 || msg[0] != "slack" {
		t.Fatalf("providers = %#v, want [slack email]", chanFetchProviders())
	}
}

func TestChanFetchProviders_DegradesOnError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusForbidden, `nope`)
	setupCLI(t, srv)
	if msg, ok := chanFetchProviders().(chanProvidersMsg); !ok || len(msg) != 0 {
		t.Error("a providers error should degrade to an empty list")
	}
}

func TestChanSubmit_CreatePostsConfig(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"channel_id":"c1"}`)
	setupCLI(t, srv)
	payload := map[string]any{"name": "Team Slack", "type": "slack", "enabled": true, "config": map[string]string{"webhook_url": "https://x"}}
	msg := chanSubmit("create", "", payload)()
	if _, ok := msg.(chanDoneMsg); !ok {
		t.Fatalf("msg = %T, want chanDoneMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/notifications/channels" {
		t.Errorf("request = %s %s, want POST /notifications/channels", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	cfg, _ := got["config"].(map[string]any)
	if got["type"] != "slack" || cfg["webhook_url"] != "https://x" {
		t.Errorf("body = %v, want type slack + config", got)
	}
}

func TestChanSubmit_EditPuts(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	chanSubmit("edit", "c1", map[string]any{"name": "x", "type": "slack", "enabled": false, "config": map[string]string{}})()
	if rec.Method != "PUT" || rec.Path != "/notifications/channels/c1" {
		t.Errorf("request = %s %s, want PUT /notifications/channels/c1", rec.Method, rec.Path)
	}
}

func TestNotificationsModel_EditPrefillsConfig(t *testing.T) {
	m := applyChanMsg(newNotificationsModel(), chanListMsg([]channelRec{
		{ChannelID: "c1", Name: "Team Slack", Type: "slack", Enabled: true, Config: map[string]string{"webhook_url": "https://x"}},
	}))
	m = applyChanMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if m.view != chanViewForm || m.formMode != "edit" || m.editID != "c1" {
		t.Fatalf("e should open the edit form for c1 (view=%v mode=%q id=%q)", m.view, m.formMode, m.editID)
	}
	if !strings.Contains(m.form.value("config"), "webhook_url=https://x") {
		t.Errorf("config not pre-filled, got %q", m.form.value("config"))
	}
}

func TestNotificationsModel_TestSendsRequest(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	m := applyChanMsg(newNotificationsModel(), chanListMsg([]channelRec{{ChannelID: "c1", Name: "Team Slack"}}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd == nil {
		t.Fatal("t should emit a test cmd")
	}
	cmd()
	if rec.Method != "POST" || rec.Path != "/notifications/channels/c1/test" {
		t.Errorf("request = %s %s, want POST /notifications/channels/c1/test", rec.Method, rec.Path)
	}
}

func TestNotificationsModel_Create_TypeRequired(t *testing.T) {
	m := newNotificationsModel()
	m.formMode = "create"
	m.form, _ = m.newChannelForm(channelRec{Enabled: true})
	m.view = chanViewForm
	// Set a name but no type (no provider catalog → free-text type left blank).
	m.form.setValues(map[string]string{"name": "x"})
	m2, cmd := m.keyForm(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd != nil {
		t.Error("submitting without a type should not emit a request")
	}
	if m2.form.errMsg == "" {
		t.Error("missing type should set an inline error")
	}
}

func TestNotificationsScreen_InUserHub(t *testing.T) {
	isolateHome(t)
	for _, s := range hubScreens() {
		if s.Title == "Notifications" {
			return
		}
	}
	t.Error("Notifications screen should be registered in the user hub")
}
