package cmd

import (
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyArgoMsg(m argoModel, msg tea.Msg) argoModel {
	updated, _ := m.Update(msg)
	return updated.(argoModel)
}

func TestArgoFetch_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"name":"web","sync_status":"Synced","health_status":"Healthy","revision":"abc123"}]`)
	setupCLI(t, srv)
	msg := argoFetch()
	if rec.Path != "/argo/apps" {
		t.Errorf("path = %q, want /argo/apps", rec.Path)
	}
	if list, ok := msg.(argoListMsg); !ok || len(list) != 1 || list[0].Name != "web" {
		t.Fatalf("msg = %#v, want one app", msg)
	}
}

func TestArgoSync_PostsSync(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"sync_id":"s1","status":"pending"}`)
	setupCLI(t, srv)
	msg := argoSync("web")()
	st, ok := msg.(argoStatusMsg)
	if !ok || st.isErr {
		t.Fatalf("msg = %#v, want a successful argoStatusMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/argo/apps/web/sync" {
		t.Errorf("request = %s %s, want POST /argo/apps/web/sync", rec.Method, rec.Path)
	}
}

func TestArgoSync_ErrorStatus(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusBadRequest, `unknown outpost`)
	setupCLI(t, srv)
	if st, ok := argoSync("web")().(argoStatusMsg); !ok || !st.isErr {
		t.Error("a failed sync should return an error status")
	}
}

func TestArgoModel_S_TriggersSync(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	m := applyArgoMsg(newArgoModel(), argoListMsg([]argoApp{{Name: "web"}}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd == nil {
		t.Fatal("s should emit a sync cmd")
	}
	cmd()
	if rec.Method != "POST" || rec.Path != "/argo/apps/web/sync" {
		t.Errorf("request = %s %s, want POST /argo/apps/web/sync", rec.Method, rec.Path)
	}
}

func TestArgoModel_EnterOpensDetail(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"name":"web","sync_status":"Synced"}`)
	setupCLI(t, srv)
	m := applyArgoMsg(newArgoModel(), argoListMsg([]argoApp{{Name: "web"}}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should emit a detail-fetch cmd")
	}
	if _, ok := cmd().(argoDetailMsg); !ok {
		t.Errorf("enter cmd returned %T, want argoDetailMsg", cmd())
	}
}

func TestArgoModel_DetailRenders(t *testing.T) {
	m := applyArgoMsg(newArgoModel(), argoDetailMsg{content: `{"name":"web"}`})
	if m.view != argoViewDetail || !strings.Contains(m.vp.View(), "web") {
		t.Errorf("detail view should render the app JSON")
	}
}

func TestArgoScreen_InUserHub(t *testing.T) {
	isolateHome(t)
	for _, s := range hubScreens() {
		if s.Title == "Argo" {
			return
		}
	}
	t.Error("Argo screen should be registered in the user hub")
}

func TestArgoCLI_Subcommands(t *testing.T) {
	for _, n := range []string{"list", "get", "sync", "sync-status"} {
		if findSubcmd(t, argoCmd, n) == nil {
			t.Errorf("argo subcommand %q not registered", n)
		}
	}
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	c := findSubcmd(t, argoCmd, "sync")
	_ = c.Flags().Set("revision", "HEAD")
	if err := c.RunE(c, []string{"web"}); err != nil {
		t.Fatalf("sync RunE: %v", err)
	}
	if rec.Method != "POST" || rec.Path != "/argo/apps/web/sync" {
		t.Errorf("request = %s %s, want POST /argo/apps/web/sync", rec.Method, rec.Path)
	}
	if !strings.Contains(string(rec.Body), "HEAD") {
		t.Errorf("body = %s, want revision HEAD", rec.Body)
	}
}
