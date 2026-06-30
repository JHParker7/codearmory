package cmd

import (
	"reflect"
	"testing"
)

// parseDSL splits a step token "name@repo" into the bare step name and a per-step
// git repo, leaving plain steps untouched.
func TestParseDSL_PerStepRepo(t *testing.T) {
	nodes, err := parseDSL("build->test@https://github.com/acme/app.git->deploy")
	if err != nil {
		t.Fatalf("parseDSL error: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3", len(nodes))
	}
	if nodes[0].names[0] != "build" || nodes[0].repos[0] != "" {
		t.Errorf("build node = %+v, want name=build repo=''", nodes[0])
	}
	if nodes[1].names[0] != "test" || nodes[1].repos[0] != "https://github.com/acme/app.git" {
		t.Errorf("test node = %+v, want name=test repo=<url>", nodes[1])
	}
	if nodes[2].repos[0] != "" {
		t.Errorf("deploy node repo = %q, want ''", nodes[2].repos[0])
	}
}

// A per-step repo can reference a run input (${inputs.X}) and works inside a parallel
// group, with each member carrying its own repo.
func TestParseDSL_PerStepRepo_InputRefAndParallel(t *testing.T) {
	nodes, err := parseDSL("[unit@${inputs.REPO},lint@https://x.git]->deploy")
	if err != nil {
		t.Fatalf("parseDSL error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	if !reflect.DeepEqual(nodes[0].names, []string{"unit", "lint"}) {
		t.Errorf("parallel names = %v, want [unit lint]", nodes[0].names)
	}
	if !reflect.DeepEqual(nodes[0].repos, []string{"${inputs.REPO}", "https://x.git"}) {
		t.Errorf("parallel repos = %v, want [${inputs.REPO} https://x.git]", nodes[0].repos)
	}
}

// gitRepoStepWith wraps a repo as the forge secret_ref override, and gitRepoFromStepWith
// reverses it — round-tripping a clone URL and an input template, and ignoring refs the
// DSL can't express (a non-git ref, or secret_refs with sibling keys).
func TestGitRepoStepWith_RoundTrip(t *testing.T) {
	for _, repo := range []string{"https://github.com/acme/app.git", "${inputs.REPO}"} {
		w := gitRepoStepWith(repo)
		if got := gitRepoFromStepWith(w); got != repo {
			t.Errorf("round-trip %q -> %q", repo, got)
		}
	}
	if gitRepoStepWith("") != nil {
		t.Error("empty repo should produce no override")
	}
	// Sibling secret_refs keys can't be expressed in the DSL, so the repo is not
	// extracted (avoids clobbering them on a name@repo round-trip).
	withSibling := map[string]any{"secret_refs": map[string]any{
		"GIT_CLONE_URL": "git:https://x.git", "TOKEN": "secret:t",
	}}
	if got := gitRepoFromStepWith(withSibling); got != "" {
		t.Errorf("repo with sibling secret_refs = %q, want '' (left for -f JSON)", got)
	}
	// A non-git ref is ignored.
	if got := gitRepoFromStepWith(map[string]any{"secret_refs": map[string]any{"GIT_CLONE_URL": "secret:x"}}); got != "" {
		t.Errorf("non-git ref = %q, want ''", got)
	}
}

// stepsToDSL renders a per-step repo back as name@repo, including inside a parallel
// group — the inverse of parseDSL.
func TestStepsToDSL_PerStepRepo(t *testing.T) {
	g0 := 0
	steps := []tuiWorkflowStep{
		{Name: "build"},
		{Name: "unit", With: gitRepoStepWith("${inputs.REPO}"), ParallelGroup: &g0},
		{Name: "lint", With: gitRepoStepWith("https://x.git"), ParallelGroup: &g0},
		{Name: "deploy", With: gitRepoStepWith("https://github.com/acme/app.git")},
	}
	want := "build->[unit@${inputs.REPO},lint@https://x.git]->deploy@https://github.com/acme/app.git"
	if got := stepsToDSL(steps); got != want {
		t.Errorf("stepsToDSL = %q, want %q", got, want)
	}
}
