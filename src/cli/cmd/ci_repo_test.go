package cmd

import (
	"reflect"
	"strings"
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
	if nodes[0].stages[0].names[0] != "build" || nodes[0].stages[0].repos[0] != "" {
		t.Errorf("build node = %+v, want name=build repo=''", nodes[0])
	}
	if nodes[1].stages[0].names[0] != "test" || nodes[1].stages[0].repos[0] != "https://github.com/acme/app.git" {
		t.Errorf("test node = %+v, want name=test repo=<url>", nodes[1])
	}
	if nodes[2].stages[0].repos[0] != "" {
		t.Errorf("deploy node repo = %q, want ''", nodes[2].stages[0].repos[0])
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
	if !reflect.DeepEqual(nodes[0].stages[0].names, []string{"unit", "lint"}) {
		t.Errorf("parallel names = %v, want [unit lint]", nodes[0].stages[0].names)
	}
	if !reflect.DeepEqual(nodes[0].stages[0].repos, []string{"${inputs.REPO}", "https://x.git"}) {
		t.Errorf("parallel repos = %v, want [${inputs.REPO} https://x.git]", nodes[0].stages[0].repos)
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
		{Name: "unit", With: gitRepoStepWith("${inputs.REPO}"), stage: &g0},
		{Name: "lint", With: gitRepoStepWith("https://x.git"), stage: &g0},
		{Name: "deploy", With: gitRepoStepWith("https://github.com/acme/app.git")},
	}
	want := "build->[unit@${inputs.REPO},lint@https://x.git]->deploy@https://github.com/acme/app.git"
	if got := stepsToDSL(steps); got != want {
		t.Errorf("stepsToDSL = %q, want %q", got, want)
	}
}

// ── the map region grammar ────────────────────────────────────────────────────

// "[body]*<map-id>" marks a bracket as a map region: the body runs once per value.
// The body may itself be a sequence, which is why the DSL cannot simply be split on
// "->" — the separator appears inside the brackets.
func TestParseDSL_MapRegion(t *testing.T) {
	nodes, err := parseDSL("checkout->[build->test]*per-module->save")
	if err != nil {
		t.Fatalf("parseDSL error: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3 (checkout, the region, save)", len(nodes))
	}
	region := nodes[1]
	if region.mapID != "per-module" {
		t.Errorf("mapID = %q, want per-module", region.mapID)
	}
	if len(region.stages) != 2 {
		t.Fatalf("region stages = %d, want 2 (build then test)", len(region.stages))
	}
	if region.stages[0].names[0] != "build" || region.stages[1].names[0] != "test" {
		t.Errorf("region body = %+v, want build then test", region.stages)
	}
	// The region's edges face outward from its first/last stage.
	if got := region.entries(); len(got) != 1 || got[0] != "build" {
		t.Errorf("entries = %v, want [build]", got)
	}
	if got := region.exits(); len(got) != 1 || got[0] != "test" {
		t.Errorf("exits = %v, want [test]", got)
	}
	// The neighbours are plain steps.
	if nodes[0].stages[0].names[0] != "checkout" || nodes[2].stages[0].names[0] != "save" {
		t.Errorf("neighbours = %+v / %+v", nodes[0], nodes[2])
	}
}

// A map body may fan out too: its steps run in parallel, once per value.
func TestParseDSL_MapRegionWithParallelBody(t *testing.T) {
	nodes, err := parseDSL("[lint,test]*per-module")
	if err != nil {
		t.Fatalf("parseDSL error: %v", err)
	}
	if nodes[0].mapID != "per-module" || len(nodes[0].stages) != 1 {
		t.Fatalf("node = %+v", nodes[0])
	}
	if !reflect.DeepEqual(nodes[0].stages[0].names, []string{"lint", "test"}) {
		t.Errorf("body = %v, want [lint test]", nodes[0].stages[0].names)
	}
}

// A single-step map body is meaningful (run it once per value) even though a
// single-step PLAIN bracket is not (it is just that step).
func TestParseDSL_SingleStepMapIsAllowed(t *testing.T) {
	if _, err := parseDSL("[build]*per-module"); err != nil {
		t.Errorf("single-step map body rejected: %v", err)
	}
	if _, err := parseDSL("[build]"); err == nil {
		t.Error("a one-step plain bracket should be rejected — it is just that step")
	}
}

func TestParseDSL_Rejects(t *testing.T) {
	cases := []struct{ name, dsl, want string }{
		{"unclosed bracket", "a->[b,c", "unclosed"},
		{"stray close", "a->b]", "unexpected ']'"},
		{"nested bracket", "[a,[b,c]]*m", "nested"},
		{"bad map id", "[a->b]*not a map", "invalid map id"},
		{"junk after bracket", "[a,b]x", "did you mean"},
		{"empty segment", "a->->b", "empty segment"},
		// Commas only mean anything inside a bracket; silently treating "a,b" as a
		// fork would be a pipeline that does not match what was written.
		{"bare comma", "a,b->c", "must be bracketed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDSL(tc.dsl)
			if err == nil {
				t.Fatalf("parseDSL(%q) = nil error, want one mentioning %q", tc.dsl, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
