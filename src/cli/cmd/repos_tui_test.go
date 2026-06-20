package cmd

import (
	"encoding/json"
	"net/http"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyReposMsg(m reposModel, msg tea.Msg) reposModel {
	updated, _ := m.Update(msg)
	return updated.(reposModel)
}

func TestRpFetchRepos_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"name":"web","full_name":"alice/web","private":true,"default_branch":"main"}]`)
	setupCLI(t, srv)
	msg := rpFetchRepos()
	if rec.Path != "/gitea_integration/repos" {
		t.Errorf("path = %q, want /gitea_integration/repos", rec.Path)
	}
	repos, ok := msg.(rpReposMsg)
	if !ok || len(repos) != 1 || repos[0].FullName != "alice/web" {
		t.Fatalf("msg = %#v, want one repo alice/web", msg)
	}
}

func TestRpFetchSection_PullsPathAndDecode(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"number":7,"title":"Fix","state":"open","head":{"ref":"feat"},"base":{"ref":"main"}}]`)
	setupCLI(t, srv)
	msg := rpFetchSection("/gitea_integration/repos/alice/web", rpPulls)()
	if rec.Path != "/gitea_integration/repos/alice/web/pulls" {
		t.Errorf("path = %q, want …/alice/web/pulls", rec.Path)
	}
	pulls, ok := msg.(rpPullsMsg)
	if !ok || len(pulls) != 1 || pulls[0].Number != 7 {
		t.Fatalf("msg = %#v, want one PR #7", msg)
	}
}

func TestRpCreateRepo_PostsPayload(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"full_name":"alice/web"}`)
	setupCLI(t, srv)
	msg := rpCreateRepo("web", "my site", true)()
	if _, ok := msg.(rpDoneMsg); !ok {
		t.Fatalf("msg = %T, want rpDoneMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/gitea_integration/repos" {
		t.Errorf("request = %s %s, want POST /gitea_integration/repos", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["name"] != "web" || got["private"] != true || got["description"] != "my site" {
		t.Errorf("body = %v, want name/private/description", got)
	}
}

func TestRpCreatePR_PostsToRepoPulls(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusCreated, `{"number":1}`)
	setupCLI(t, srv)
	rpCreatePR("/gitea_integration/repos/alice/web", "Fix bug", "feature", "main", "details")()
	if rec.Method != "POST" || rec.Path != "/gitea_integration/repos/alice/web/pulls" {
		t.Errorf("request = %s %s, want POST …/alice/web/pulls", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["title"] != "Fix bug" || got["head"] != "feature" || got["base"] != "main" {
		t.Errorf("body = %v, want title/head/base", got)
	}
}

func TestRpMergePR_PostsMerge(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	msg := rpMergePR("/gitea_integration/repos/alice/web", 7)()
	if _, ok := msg.(rpDoneMsg); !ok {
		t.Fatalf("msg = %T, want rpDoneMsg", msg)
	}
	if rec.Method != "POST" || rec.Path != "/gitea_integration/repos/alice/web/pulls/7/merge" {
		t.Errorf("request = %s %s, want POST …/pulls/7/merge", rec.Method, rec.Path)
	}
}

func TestRpAccount_LinkUnlink(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{}`)
	setupCLI(t, srv)
	rpLinkAccount("tok-123")()
	if rec.Method != "PUT" || rec.Path != "/gitea_integration/account" {
		t.Errorf("link request = %s %s, want PUT /gitea_integration/account", rec.Method, rec.Path)
	}
	var got map[string]any
	json.Unmarshal(rec.Body, &got) //nolint:errcheck
	if got["token"] != "tok-123" {
		t.Errorf("link body = %v, want token", got)
	}
	rpUnlinkAccount()()
	if rec.Method != "DELETE" {
		t.Errorf("unlink method = %q, want DELETE", rec.Method)
	}
}

func TestRpFetchAccount_LinkedAndNot(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"gitea_username":"alice"}`)
	setupCLI(t, srv)
	if a, ok := rpFetchAccount().(rpAccountMsg); !ok || !a.linked || a.username != "alice" {
		t.Errorf("account = %#v, want linked alice", rpFetchAccount())
	}

	srv2, _ := recordingServer(t, http.StatusNotFound, `not linked`)
	setupCLI(t, srv2)
	if a, ok := rpFetchAccount().(rpAccountMsg); !ok || a.linked {
		t.Errorf("account = %#v, want not linked on 404", rpFetchAccount())
	}
}

func TestRpModel_EnterOpensRepoAndFetchesBranches(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	m := applyReposMsg(newReposModel(), rpReposMsg([]rpRepo{{Name: "web", FullName: "alice/web"}}))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(reposModel)
	if m2.view != rpViewRepo || m2.section != rpBranches || m2.selRepo == nil {
		t.Fatalf("enter should open the repo at the branches section (view=%v section=%v)", m2.view, m2.section)
	}
	if cmd == nil {
		t.Error("opening a repo should fetch the active section")
	}
}

func TestRpModel_TabCyclesSection(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `[]`)
	setupCLI(t, srv)
	m := applyReposMsg(newReposModel(), rpReposMsg([]rpRepo{{FullName: "alice/web"}}))
	m = applyReposMsg(m, tea.KeyMsg{Type: tea.KeyEnter}) // open repo (branches)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if updated.(reposModel).section != rpTags {
		t.Errorf("tab should advance to the Tags section, got %v", updated.(reposModel).section)
	}
	if cmd == nil {
		t.Error("switching section should fetch its data")
	}
}

func TestRpHelpers(t *testing.T) {
	o, n := rpSplit("alice/web")
	if o != "alice" || n != "web" {
		t.Errorf("rpSplit = %q/%q, want alice/web", o, n)
	}
	if rpShortSHA("0123456789abcdef") != "012345678" {
		t.Errorf("rpShortSHA = %q, want 9 chars", rpShortSHA("0123456789abcdef"))
	}
	if rpFirstLine("line1\nline2") != "line1" {
		t.Errorf("rpFirstLine = %q, want line1", rpFirstLine("line1\nline2"))
	}
}

func TestReposScreen_InUserHub(t *testing.T) {
	isolateHome(t)
	for _, s := range hubScreens() {
		if s.Title == "Repos" {
			return
		}
	}
	t.Error("Repos screen should be registered in the user hub")
}
