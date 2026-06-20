package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyOpMsg(m outpostModel, msg tea.Msg) outpostModel {
	updated, _ := m.Update(msg)
	return updated.(outpostModel)
}

func TestOpFetch_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"outpost_id":"o1","name":"prod","modules":"chaos,argo","status":"online"}]`)
	setupCLI(t, srv)
	msg := opFetch()
	if rec.Path != "/outpost-gateway/outposts" {
		t.Errorf("path = %q, want /outpost-gateway/outposts", rec.Path)
	}
	if list, ok := msg.(opListMsg); !ok || len(list) != 1 || list[0].Name != "prod" {
		t.Fatalf("msg = %#v, want one outpost", msg)
	}
}

func TestOpCreate_PostsAndReturnsToken(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"outpost_id":"o1","enrollment_token":"tok-abc"}`)
	setupCLI(t, srv)
	msg := opCreate("prod", []string{"chaos", "argo"})()
	created, ok := msg.(opCreatedMsg)
	if !ok || created.token != "tok-abc" {
		t.Fatalf("msg = %#v, want opCreatedMsg with token", msg)
	}
	if rec.Method != "POST" || rec.Path != "/outpost-gateway/outposts" {
		t.Errorf("request = %s %s, want POST /outpost-gateway/outposts", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	mods, _ := got["modules"].([]any)
	if got["name"] != "prod" || len(mods) != 2 {
		t.Errorf("body = %v, want name + 2 modules", got)
	}
}

func TestOpModel_CreatedShowsTokenOnce(t *testing.T) {
	m := applyOpMsg(newOutpostModel(), opCreatedMsg{token: "tok-xyz"})
	if m.view != opViewToken {
		t.Fatalf("view = %v, want opViewToken", m.view)
	}
	if !strings.Contains(m.vp.View(), "tok-xyz") {
		t.Errorf("token view should reveal the enrollment token, got: %q", m.vp.View())
	}
}

func TestOpModel_Delete_ConfirmRuns(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusNoContent, ``)
	setupCLI(t, srv)
	m := applyOpMsg(newOutpostModel(), opListMsg([]outpostRec{{OutpostID: "o1", Name: "prod"}}))
	m = applyOpMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	if m.pending == nil {
		t.Fatal("D should set a delete confirmation")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("y should run the delete")
	}
	cmd()
	if rec.Method != "DELETE" || rec.Path != "/outpost-gateway/outposts/o1" {
		t.Errorf("request = %s %s, want DELETE /outpost-gateway/outposts/o1", rec.Method, rec.Path)
	}
}

func TestOutpostScreen_InAdminHub(t *testing.T) {
	isolateHome(t)
	for _, s := range adminScreens() {
		if s.Title == "Outposts" {
			return
		}
	}
	t.Error("Outposts screen should be registered in the admin hub")
}

func TestOutpostCLI_Subcommands(t *testing.T) {
	for _, n := range []string{"list", "create", "get", "delete"} {
		if findSubcmd(t, outpostCmd, n) == nil {
			t.Errorf("outpost subcommand %q not registered", n)
		}
	}
	srv, rec := recordingServer(t, http.StatusCreated, `{"enrollment_token":"tok"}`)
	setupCLI(t, srv)
	c := findSubcmd(t, outpostCmd, "create")
	_ = c.Flags().Set("name", "prod")
	if err := c.RunE(c, nil); err != nil {
		t.Fatalf("create RunE: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/outpost-gateway/outposts" {
		t.Errorf("request = %s %s, want POST /outpost-gateway/outposts", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["name"] != "prod" {
		t.Errorf("name = %v, want prod", got["name"])
	}
}
