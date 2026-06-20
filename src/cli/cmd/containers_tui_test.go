package cmd

import (
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func applyCtMsg(m containersModel, msg tea.Msg) containersModel {
	updated, _ := m.Update(msg)
	return updated.(containersModel)
}

func TestCtFetchRepos_Success(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `[{"name":"alice/web"},{"name":"alice/api"}]`)
	setupCLI(t, srv)
	msg := ctFetchRepos()
	if rec.Method != "GET" || rec.Path != "/containers/repositories" {
		t.Errorf("request = %s %s, want GET /containers/repositories", rec.Method, rec.Path)
	}
	repos, ok := msg.(ctReposMsg)
	if !ok || len(repos) != 2 || repos[0] != "alice/web" {
		t.Fatalf("msg = %#v, want [alice/web alice/api]", msg)
	}
}

func TestCtFetchTags_SplitsRepoPath(t *testing.T) {
	srv, rec := recordingServer(t, http.StatusOK, `{"name":"alice/web","tags":["v1","latest"]}`)
	setupCLI(t, srv)
	msg := ctFetchTags("alice/web")()
	if rec.Path != "/containers/repositories/alice/web/tags" {
		t.Errorf("path = %q, want /containers/repositories/alice/web/tags", rec.Path)
	}
	tags, ok := msg.(ctTagsMsg)
	if !ok || len(tags.tags) != 2 || tags.tags[1] != "latest" {
		t.Fatalf("msg = %#v, want tags [v1 latest]", msg)
	}
}

func TestCtDeleteTag_ResolvesDigestThenDeletes(t *testing.T) {
	var deletePath string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/repositories/{ns}/{img}/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"digest":"sha256:abc123"}`)) //nolint:errcheck
	})
	mux.HandleFunc("DELETE /containers/repositories/{ns}/{img}/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		deletePath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	})
	setupCLI(t, routeServer(t, mux))

	msg := ctDeleteTag("alice/web", "v1")()
	if _, ok := msg.(ctDoneMsg); !ok {
		t.Fatalf("msg = %T, want ctDoneMsg", msg)
	}
	if !strings.HasSuffix(deletePath, "/manifests/sha256:abc123") {
		t.Errorf("delete path = %q, want it to target the resolved digest", deletePath)
	}
}

func TestCtModel_EnterReposFetchesTags(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, `{"tags":["v1"]}`)
	setupCLI(t, srv)
	m := applyCtMsg(newContainersModel(), ctReposMsg([]string{"alice/web"}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a repo should emit a tags-fetch cmd")
	}
	if _, ok := cmd().(ctTagsMsg); !ok {
		t.Errorf("enter cmd returned %T, want ctTagsMsg", cmd())
	}
}

func TestContainersScreen_InUserHub(t *testing.T) {
	isolateHome(t)
	for _, s := range hubScreens() {
		if s.Title == "Containers" {
			return
		}
	}
	t.Error("Containers screen should be registered in the user hub")
}
