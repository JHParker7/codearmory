package cmd

import (
	"net/http"
	"reflect"
	"testing"
)

// projectFilter resolves --all > --project > the sticky current project.
func TestProjectFilter_Precedence(t *testing.T) {
	isolateHome(t)
	if err := saveConfig(cliConfig{CurrentProject: "sticky"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	defer func() { flagProject = ""; flagAllProjects = false }()

	// Sticky config value when no flags are set.
	flagProject, flagAllProjects = "", false
	if got := projectFilter(); got != "sticky" {
		t.Errorf("config only: got %q, want sticky", got)
	}

	// --project overrides the sticky value.
	flagProject = "flagproj"
	if got := projectFilter(); got != "flagproj" {
		t.Errorf("--project override: got %q, want flagproj", got)
	}

	// --all wins over both --project and the sticky value.
	flagAllProjects = true
	if got := projectFilter(); got != "" {
		t.Errorf("--all: got %q, want empty", got)
	}
}

func TestAppendProjectParam(t *testing.T) {
	isolateHome(t)
	defer func() { flagProject = ""; flagAllProjects = false }()

	flagProject, flagAllProjects = "alpha", false
	if got := appendProjectParam("/x"); got != "/x?project=alpha" {
		t.Errorf("no query: got %q", got)
	}
	if got := appendProjectParam("/x?status=open"); got != "/x?status=open&project=alpha" {
		t.Errorf("existing query: got %q", got)
	}

	// Labels with spaces/special chars are URL-escaped.
	flagProject = "a b"
	if got := appendProjectParam("/x"); got != "/x?project=a+b" {
		t.Errorf("escaping: got %q", got)
	}

	// --all suppresses the param entirely.
	flagProject, flagAllProjects = "alpha", true
	if got := appendProjectParam("/x?status=open"); got != "/x?status=open" {
		t.Errorf("--all suppresses: got %q", got)
	}
}

// fetchKnownProjects aggregates distinct, non-empty labels across all four list
// endpoints and returns them sorted; endpoints that error are skipped.
func TestFetchKnownProjects_AggregatesAndDedups(t *testing.T) {
	isolateHome(t)
	list := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body)) //nolint:errcheck
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/pipelines", list(`[{"project":"alpha"},{"project":""}]`))
	mux.HandleFunc("/tickets/tickets", list(`[{"project":"beta"},{"project":"alpha"}]`))
	mux.HandleFunc("/forge/executions", list(`[{"project":"gamma"}]`))
	// /gitea_integration/repos intentionally unregistered → 404 → skipped, not fatal.
	srv := routeServer(t, mux)
	setupCLI(t, srv)

	got := fetchKnownProjects()
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fetchKnownProjects() = %v, want %v", got, want)
	}
}

// setCurrentProject round-trips through the config file, and clearing removes it.
func TestSetCurrentProject_RoundTrip(t *testing.T) {
	isolateHome(t)
	if err := setCurrentProject("payments-api"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := loadConfig().CurrentProject; got != "payments-api" {
		t.Errorf("after set: got %q, want payments-api", got)
	}
	if err := setCurrentProject(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := loadConfig().CurrentProject; got != "" {
		t.Errorf("after clear: got %q, want empty", got)
	}
}
