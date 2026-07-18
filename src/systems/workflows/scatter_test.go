package main

import (
	"encoding/json"
	"testing"
)

// getWith is a small helper to read a nested map[string]any from a step's With.
func getMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("With[%q] is not a map: %v", key, m[key])
	}
	return v
}

func getSlice(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("With[%q] is not a slice: %v", key, m[key])
	}
	return v
}

// TestValidateScatter covers the shape checks.
func TestValidateScatter(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ScatterConfig
		wantErr bool
	}{
		{name: "valid", cfg: ScatterConfig{Regex: "svc/.*", Mode: "dir"}},
		{name: "valid paths_from", cfg: ScatterConfig{PathsFrom: "${inputs.services}"}},
		{name: "one source required", cfg: ScatterConfig{}, wantErr: true},
		{name: "regex and paths_from mutually exclusive", cfg: ScatterConfig{Regex: ".*", PathsFrom: "${inputs.x}"}, wantErr: true},
		{name: "bad mode", cfg: ScatterConfig{Regex: ".*", Mode: "socket"}, wantErr: true},
		{name: "negative depth", cfg: ScatterConfig{Regex: ".*", MaxDepth: -1}, wantErr: true},
		{name: "negative concurrency", cfg: ScatterConfig{Regex: ".*", MaxConcurrent: -1}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateScatter(&tc.cfg)
			if tc.wantErr != (msg != "") {
				t.Fatalf("validateScatter = %q, wantErr=%v", msg, tc.wantErr)
			}
		})
	}
}

// TestScatterResolveStep checks the resolve step attaches the base volume and carries
// the regex/mode into the forge/resolve-paths action.
func TestScatterResolveStep(t *testing.T) {
	cfg := ScatterConfig{Regex: "^svc/[^/]+$", Mode: "dir"}
	s := scatterResolveStep(&cfg, "run-1")
	if s.Action != actionForgeResolvePaths {
		t.Fatalf("action = %q", s.Action)
	}
	r := getMap(t, s.With, "resolve")
	if r["regex"] != "^svc/[^/]+$" || r["mode"] != "dir" || r["output"] != scatterPathsOutput {
		t.Errorf("resolve = %v", r)
	}
	vols := getSlice(t, s.With, "volumes")
	if len(vols) != 1 {
		t.Fatalf("want 1 volume, got %v", vols)
	}
	v := vols[0].(map[string]any)
	if v["name"] != defaultScatterVolume || v["read_only"] != true {
		t.Errorf("base mount = %v", v)
	}
}

// TestScatterCloneStep checks a leg clone mounts the shard workdir + base source and
// copies the whole tree from the base.
func TestScatterCloneStep(t *testing.T) {
	cfg := ScatterConfig{Volume: "ws", MountPath: "/w"}
	s := scatterCloneStep(&cfg, "run-1", "ws-s2")
	if s.Action != actionForgeVolumeCopy {
		t.Fatalf("action = %q", s.Action)
	}
	vols := getSlice(t, s.With, "volumes")
	if len(vols) != 2 {
		t.Fatalf("want 2 volumes, got %v", vols)
	}
	dst := vols[0].(map[string]any)
	if dst["name"] != "ws-s2" || dst["workdir"] != true || dst["mount_path"] != "/w" {
		t.Errorf("dest = %v", dst)
	}
	src := vols[1].(map[string]any)
	if src["name"] != "ws" || src["read_only"] != true {
		t.Errorf("src = %v", src)
	}
	cp := getMap(t, s.With, "copy")
	sources := cp["sources"].([]any)
	if len(sources) != 1 || sources[0].(map[string]any)["volume"] != "ws" {
		t.Errorf("copy.sources = %v", sources)
	}
	if _, hasDisjoint := cp["disjoint"]; hasDisjoint {
		t.Error("a clone must not set disjoint")
	}
}

// TestScatterLegStep checks the user's step gains the shard volume as workdir while
// keeping its own With keys.
func TestScatterLegStep(t *testing.T) {
	ws := WorkflowStep{Step: Step{Name: "build", Action: "forge/run", With: map[string]any{"run": "make -C ${scatter.path}", "runner_class": "large"}}}
	cfg := ScatterConfig{Volume: "ws", MountPath: "/w"}
	s := scatterLegStep(ws, &cfg, "run-1", "ws-s0")
	if s.Action != "forge/run" || s.With["run"] != "make -C ${scatter.path}" || s.With["runner_class"] != "large" {
		t.Errorf("leg step lost its config: %v", s.With)
	}
	vols := getSlice(t, s.With, "volumes")
	v := vols[0].(map[string]any)
	if v["name"] != "ws-s0" || v["workdir"] != true {
		t.Errorf("leg volume = %v", v)
	}
}

// TestScatterGatherStep checks the gather is a disjoint copy pulling each leg's owned
// (path-substituted) outputs from its shard into the base workspace.
func TestScatterGatherStep(t *testing.T) {
	cfg := ScatterConfig{Volume: "ws", MountPath: "/w", Outputs: []string{"${scatter.path}/dist"}}
	shards := []string{"ws-s0", "ws-s1"}
	paths := []string{"svc/a", "svc/b"}
	s, ok := scatterGatherStep(&cfg, "run-1", shards, paths)
	if !ok {
		t.Fatal("gather should be produced when Outputs is set")
	}
	cp := getMap(t, s.With, "copy")
	if cp["disjoint"] != true {
		t.Error("gather must be disjoint")
	}
	sources := cp["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("want 2 sources, got %v", sources)
	}
	s0 := sources[0].(map[string]any)
	owned := s0["paths"].([]any)
	if s0["volume"] != "ws-s0" || len(owned) != 1 || owned[0] != "svc/a/dist" {
		t.Errorf("source0 owned paths = %v (want svc/a/dist)", s0)
	}
	// The destination (workdir) must be the base workspace.
	dst := getSlice(t, s.With, "volumes")[0].(map[string]any)
	if dst["name"] != "ws" || dst["workdir"] != true {
		t.Errorf("gather dest = %v", dst)
	}

	// No declared outputs → no gather.
	if _, ok := scatterGatherStep(&ScatterConfig{Volume: "ws"}, "run-1", shards, paths); ok {
		t.Error("gather should be skipped when Outputs is empty")
	}
}

// TestParseScatterPaths checks the matched-path list is pulled from the resolve step's
// captured output map (and tolerates a bare value), split like a matrix list.
func TestParseScatterPaths(t *testing.T) {
	outputMap, _ := json.Marshal(map[string]string{"paths": "svc/a\nsvc/b\nsvc/c"})
	got := parseScatterPaths(string(outputMap))
	if len(got) != 3 || got[0] != "svc/a" || got[2] != "svc/c" {
		t.Errorf("from output map = %v", got)
	}
	if got := parseScatterPaths(`["x","y"]`); len(got) != 2 || got[1] != "y" {
		t.Errorf("from bare JSON array = %v", got)
	}
	if got := parseScatterPaths(""); got != nil {
		t.Errorf("empty = %v, want nil", got)
	}
}

// TestScatterPathSubstitution checks ${scatter.path} resolves in a With template.
func TestScatterPathSubstitution(t *testing.T) {
	sc := substContext{scatterPath: "services/api"}
	if got := substitute("cd ${scatter.path} && make", sc); got != "cd services/api && make" {
		t.Errorf("substitute = %q", got)
	}
	// Unset scatter.path leaves the reference untouched.
	if got := substitute("${scatter.path}", substContext{runID: "r"}); got != "${scatter.path}" {
		t.Errorf("unset scatter.path = %q, want literal", got)
	}
}
