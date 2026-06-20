package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applySecretsMsg(m secretsModel, msg tea.Msg) secretsModel {
	updated, _ := m.Update(msg)
	return updated.(secretsModel)
}

func TestSecretsModel_ListPopulates(t *testing.T) {
	m := applySecretsMsg(newSecretsModel(), secretsListMsg([]secretRec{{SecretID: "s1", Name: "DB_URL"}}))
	if m.loading {
		t.Error("loading should be false after list")
	}
	if !strings.Contains(m.View(), "DB_URL") {
		t.Errorf("list view should show the secret name, got: %q", m.View())
	}
}

func TestSecretsFetch_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"secret_id":"s1","name":"DB_URL"}]`)
	setupCLI(t, srv)
	msg := secretsFetch()
	if rec.Method != "GET" || rec.Path != "/gatekeeper/secrets" {
		t.Errorf("request = %s %s, want GET /gatekeeper/secrets", rec.Method, rec.Path)
	}
	if list, ok := msg.(secretsListMsg); !ok || len(list) != 1 {
		t.Fatalf("msg = %#v, want secretsListMsg with 1 item", msg)
	}
}

func TestSecretsSubmit_Create(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"secret_id":"s1"}`)
	setupCLI(t, srv)
	msg := secretsSubmit("create", "", "DB_URL", "postgres://x")()
	if _, ok := msg.(secretsDoneMsg); !ok {
		t.Fatalf("msg = %T, want secretsDoneMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/gatekeeper/secrets" {
		t.Errorf("request = %s %s, want POST /gatekeeper/secrets", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["name"] != "DB_URL" || got["value"] != "postgres://x" {
		t.Errorf("body = %v, want name/value", got)
	}
}

func TestSecretsSubmit_EditPutsValueOnly(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	msg := secretsSubmit("edit", "s1", "", "newval")()
	if _, ok := msg.(secretsDoneMsg); !ok {
		t.Fatalf("msg = %T, want secretsDoneMsg", msg)
	}
	if rec.Method != "PUT" || rec.Path != "/gatekeeper/secrets/s1" {
		t.Errorf("request = %s %s, want PUT /gatekeeper/secrets/s1", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["value"] != "newval" {
		t.Errorf("value = %v, want newval", got["value"])
	}
	if _, present := got["name"]; present {
		t.Error("edit must not send a name (only the value is updatable)")
	}
}

func TestSecretsModel_Delete_ConfirmRuns(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusNoContent, ``)
	setupCLI(t, srv)
	m := applySecretsMsg(newSecretsModel(), secretsListMsg([]secretRec{{SecretID: "s1", Name: "DB_URL"}}))
	m = applySecretsMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m.pending == nil {
		t.Fatal("D should set a delete confirmation")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("y should run the delete")
	}
	cmd()
	if rec.Method != "DELETE" || rec.Path != "/gatekeeper/secrets/s1" {
		t.Errorf("request = %s %s, want DELETE /gatekeeper/secrets/s1", rec.Method, rec.Path)
	}
}

func TestSecretsModel_Edit_ValueRequired(t *testing.T) {
	m := applySecretsMsg(newSecretsModel(), secretsListMsg([]secretRec{{SecretID: "s1", Name: "DB_URL"}}))
	m = applySecretsMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if m.view != secretsViewForm || m.formMode != "edit" {
		t.Fatalf("e should open the edit form (view=%v mode=%q)", m.view, m.formMode)
	}
	m2, cmd := m.keyForm(tea.KeyMsg{Type: tea.KeyCtrlS}) // empty value
	if cmd != nil {
		t.Error("submitting an empty value should not emit a request")
	}
	if m2.form.errMsg == "" {
		t.Error("empty value should set an inline error")
	}
}

func TestSecretsScreen_InUserHub(t *testing.T) {
	isolateHome(t)
	for _, s := range hubScreens() {
		if s.Title == "Secrets" {
			return
		}
	}
	t.Error("Secrets screen should be registered in the user hub")
}
