package cmd

import (
	"encoding/json"
	"net/http"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyChaosMsg(m chaosModel, msg tea.Msg) chaosModel {
	updated, _ := m.Update(msg)
	return updated.(chaosModel)
}

func TestChaosFetch_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"experiment_id":"e1","experiment_type":"pod-delete","target_app_ns":"prod","target_app_label":"app=web","status":"running"}]`)
	setupCLI(t, srv)
	msg := chaosFetch()
	if rec.Path != "/chaos/experiments" {
		t.Errorf("path = %q, want /chaos/experiments", rec.Path)
	}
	if list, ok := msg.(chaosListMsg); !ok || len(list) != 1 || list[0].ExperimentType != "pod-delete" {
		t.Fatalf("msg = %#v, want one experiment", msg)
	}
}

func TestChaosFetchTypes_ExtractsNames(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[{"name":"pod-delete"},{"name":"cpu-hog"}]`)
	setupCLI(t, srv)
	if msg, ok := chaosFetchTypes().(chaosTypesMsg); !ok || len(msg) != 2 || msg[0] != "pod-delete" {
		t.Fatalf("types = %#v, want [pod-delete cpu-hog]", chaosFetchTypes())
	}
}

func TestChaosFetchOutposts_NameIDPairs(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[{"outpost_id":"o1","name":"prod"}]`)
	setupCLI(t, srv)
	msg, ok := chaosFetchOutposts().(chaosOutpostsMsg)
	if !ok || len(msg.ids) != 1 || msg.ids[0] != "o1" || msg.names[0] != "prod" {
		t.Fatalf("outposts = %#v, want one name/id pair", chaosFetchOutposts())
	}
}

func TestChaosCreate_PostsPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"experiment_id":"e1"}`)
	setupCLI(t, srv)
	payload := map[string]any{
		"outpost_id": "o1", "experiment_type": "pod-delete",
		"target_app_ns": "prod", "target_app_label": "app=web", "target_app_kind": "deployment",
		"params": map[string]string{"duration": "30"},
	}
	msg := chaosCreate(payload)()
	if _, ok := msg.(chaosDoneMsg); !ok {
		t.Fatalf("msg = %T, want chaosDoneMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/chaos/experiments" {
		t.Errorf("request = %s %s, want POST /chaos/experiments", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["experiment_type"] != "pod-delete" || got["target_app_ns"] != "prod" {
		t.Errorf("body = %v, want experiment_type/target_app_ns", got)
	}
}

func TestChaosModel_Create_RequiredFields(t *testing.T) {
	m := newChaosModel()
	m.form, _ = m.newChaosForm()
	m.view = chaosViewForm
	// type selector defaults to free-text empty (no catalog) → submit should fail
	m2, cmd := m.keyForm(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd != nil {
		t.Error("submitting an empty experiment form should not emit a request")
	}
	if m2.form.errMsg == "" {
		t.Error("missing fields should set an inline error")
	}
}

func TestChaosModel_Delete_Confirm(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusNoContent, ``)
	setupCLI(t, srv)
	m := applyChaosMsg(newChaosModel(), chaosListMsg([]chaosExp{{ExperimentID: "e1", ExperimentType: "pod-delete"}}))
	m = applyChaosMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m.pending == nil {
		t.Fatal("D should set a delete confirmation")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("y should run the delete")
	}
	cmd()
	if rec.Method != "DELETE" || rec.Path != "/chaos/experiments/e1" {
		t.Errorf("request = %s %s, want DELETE /chaos/experiments/e1", rec.Method, rec.Path)
	}
}

func TestChaosScreen_InUserHub(t *testing.T) {
	isolateHome(t)
	for _, s := range hubScreens() {
		if s.Title == "Chaos" {
			return
		}
	}
	t.Error("Chaos screen should be registered in the user hub")
}

func TestChaosCLI_Subcommands(t *testing.T) {
	for _, n := range []string{"list", "types", "create", "get", "delete"} {
		if findSubcmd(t, chaosCmd, n) == nil {
			t.Errorf("chaos subcommand %q not registered", n)
		}
	}
	srv, rec := recordingServer(t, http.StatusCreated, `{}`)
	setupCLI(t, srv)
	c := findSubcmd(t, chaosCmd, "create")
	_ = c.Flags().Set("outpost", "o1")
	_ = c.Flags().Set("type", "pod-delete")
	_ = c.Flags().Set("namespace", "prod")
	_ = c.Flags().Set("label", "app=web")
	if err := c.RunE(c, nil); err != nil {
		t.Fatalf("create RunE: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/chaos/experiments" {
		t.Errorf("request = %s %s, want POST /chaos/experiments", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["experiment_type"] != "pod-delete" || got["target_app_kind"] != "deployment" {
		t.Errorf("body = %v, want pod-delete/deployment", got)
	}
}
