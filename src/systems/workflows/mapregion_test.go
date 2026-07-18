package main

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// mapped returns a step assigned to a map region.
func mapped(name, mapID string) WorkflowStep {
	return WorkflowStep{Step: Step{Name: name, Action: "forge/run"}, MapID: mapID}
}

func plain(name string) WorkflowStep {
	return WorkflowStep{Step: Step{Name: name, Action: "forge/run"}}
}

// The user-facing shape: discover -> [build -> test -> push] -> deploy -> integration,
// where the bracketed body repeats per directory.
func mapPipeline() ([]WorkflowStep, []MapDef, []WorkflowRoute) {
	steps := []WorkflowStep{
		plain("discover"),
		mapped("build", "m1"),
		mapped("test", "m1"),
		mapped("push", "m1"),
		plain("deploy"),
		plain("integration"),
	}
	defs := []MapDef{{
		ID: "m1", Var: "dir", ValuesFrom: "${steps.discover.output.DIRS}",
		Volume: "workspace", MaxConcurrent: 4,
	}}
	routes := []WorkflowRoute{
		{From: "discover", To: "build"},
		{From: "build", To: "test"},
		{From: "test", To: "push", When: `inputs.env == "prod"`},
		{From: "push", To: "deploy"},
		{From: "deploy", To: "integration"},
	}
	return steps, defs, routes
}

func TestMapRegion_GroupsMembersAndValidates(t *testing.T) {
	steps, defs, routes := mapPipeline()
	if msg := validateGraph(steps, routes, defs); msg != "" {
		t.Fatalf("valid map pipeline rejected: %s", msg)
	}
	regions := regionsOf(steps, defs)
	r, ok := regions["m1"]
	if !ok {
		t.Fatal("region m1 missing")
	}
	if len(r.nodes) != 3 {
		t.Fatalf("region nodes = %v, want build/test/push", r.nodes)
	}
	// Region membership is by MapID, not by position.
	if regionOfNode(regions)["deploy"] != "" {
		t.Error("deploy must not be in the region")
	}
}

// The region's subgraph is its own nodes and only the routes wholly inside it — the
// boundary routes belong to the region-as-super-node, not to an iteration.
func TestMapRegion_SubGraphExcludesBoundaryRoutes(t *testing.T) {
	steps, defs, routes := mapPipeline()
	g := newGraph(steps, routes).withMaps(defs)
	sub := g.subGraph(g.regions["m1"])

	if len(sub.steps) != 3 {
		t.Fatalf("subgraph steps = %d, want 3", len(sub.steps))
	}
	got := routeSet(sub.routes)
	if len(got) != 2 || got[0] != "build->test" || got[1] != "test->push" {
		t.Fatalf("subgraph routes = %v, want [build->test test->push]", got)
	}
	// build is the iteration's entry: discover->build is the REGION's inbound edge.
	if len(sub.entries) != 1 || sub.entries[0] != "build" {
		t.Fatalf("subgraph entries = %v, want [build]", sub.entries)
	}
}

// An iteration's step runs must carry the step's index in the WORKFLOW's array, not
// its position within the region — StepIndex is the join key back to the definition
// (wf.Steps[sr.StepIndex]), so re-indexing would make every mapped step run name the
// wrong step.
func TestMapRegion_SubGraphKeepsWorkflowStepIndices(t *testing.T) {
	steps, defs, routes := mapPipeline()
	g := newGraph(steps, routes).withMaps(defs)
	sub := g.subGraph(g.regions["m1"])

	for name, want := range map[string]int{"build": 1, "test": 2, "push": 3} {
		if got := sub.stepIndex(name); got != want {
			t.Errorf("subgraph stepIndex(%s) = %d, want %d (its position in the workflow)", name, got, want)
		}
	}
	// The local index still addresses this graph's own slice, so fetching a node works.
	if sub.steps[sub.index["build"]].Name != "build" {
		t.Error("local index must still address the subgraph's own step slice")
	}
}

// The region waits on its inbound routes as a unit, and its members never launch
// individually — that is what makes deploy run once, after every iteration.
func TestMapRegion_ReadyOnlyAfterInboundTaken(t *testing.T) {
	steps, defs, routes := mapPipeline()
	g := newGraph(steps, routes).withMaps(defs)
	st := newRunState(g, map[string]string{}, nil)

	// Nothing has run: only discover is ready, and no region is.
	if ready := g.readyNodes(st); len(ready) != 1 || ready[0] != "discover" {
		t.Fatalf("ready = %v, want [discover]", ready)
	}
	if regions := g.readyRegions(st); len(regions) != 0 {
		t.Fatalf("region ready before its inbound route resolved")
	}

	// discover completes -> discover->build taken -> the region may expand.
	st.nodes["discover"] = nodeCompleted
	(&WorkerPool{}).resolveOutbound(context.Background(), g, st, "run", nil, "discover")
	regions := g.readyRegions(st)
	if len(regions) != 1 || regions[0].def.ID != "m1" {
		t.Fatalf("readyRegions = %v, want [m1]", regions)
	}
	// A region member must never be offered as a standalone node.
	for _, n := range g.readyNodes(st) {
		if g.regionOf[n] != "" {
			t.Fatalf("region member %q offered as a standalone node", n)
		}
	}
}

// Region members share the region's outcome, and downstream waits for the whole
// region — not for any single iteration or member.
func TestMapRegion_DownstreamWaitsForWholeRegion(t *testing.T) {
	steps, defs, routes := mapPipeline()
	g := newGraph(steps, routes).withMaps(defs)
	st := newRunState(g, map[string]string{}, nil)
	p := &WorkerPool{}

	st.nodes["discover"] = nodeCompleted
	p.resolveOutbound(context.Background(), g, st, "run", nil, "discover")

	// deploy is not ready while the region is unfinished.
	if g.nodeReady(st, "deploy") {
		t.Fatal("deploy ready before the map region finished")
	}
	// The region completes: every member takes its state, then edges resolve.
	for _, n := range g.regions["m1"].nodes {
		st.nodes[n] = nodeCompleted
	}
	for _, n := range g.regions["m1"].nodes {
		p.resolveOutbound(context.Background(), g, st, "run", nil, n)
	}
	if !g.nodeReady(st, "deploy") {
		t.Fatal("deploy must run once the whole region is done")
	}
}

func TestValidateMaps(t *testing.T) {
	steps, defs, routes := mapPipeline()
	cases := []struct {
		name  string
		steps []WorkflowStep
		defs  []MapDef
		rts   []WorkflowRoute
		want  string
	}{
		{
			"map_id naming no map",
			[]WorkflowStep{mapped("build", "ghost")},
			nil, nil, "names no declared map",
		},
		{
			"map with no members",
			[]WorkflowStep{plain("a")},
			[]MapDef{{ID: "m1", Var: "d", Values: []string{"x"}}},
			nil, "no step declares map_id",
		},
		{
			"both value sources",
			[]WorkflowStep{mapped("build", "m1")},
			[]MapDef{{ID: "m1", Var: "d", Values: []string{"x"}, ValuesFrom: "${inputs.d}"}},
			nil, "exactly one of values or values_from",
		},
		{
			"no value source",
			[]WorkflowStep{mapped("build", "m1")},
			[]MapDef{{ID: "m1", Var: "d"}},
			nil, "exactly one of values or values_from",
		},
		{
			"missing var",
			[]WorkflowStep{mapped("build", "m1")},
			[]MapDef{{ID: "m1", Values: []string{"x"}}},
			nil, "var is required",
		},
		{
			// A step's own fan-out inside a region would nest, multiplying the work
			// with no way to address the inner binding.
			"matrix inside a map",
			[]WorkflowStep{{Step: Step{Name: "build"}, MapID: "m1", Matrix: &MatrixConfig{Var: "os", Values: []string{"a"}}}},
			[]MapDef{{ID: "m1", Var: "d", Values: []string{"x"}}},
			nil, "matrix cannot be combined with map_id",
		},
		{
			// Resume keys completion by node name, which cannot distinguish one
			// iteration's gate from another's.
			"gate inside a map",
			[]WorkflowStep{{Step: Step{Name: "gate", Action: ActionApproval}, MapID: "m1"}},
			[]MapDef{{ID: "m1", Var: "d", Values: []string{"x"}}},
			nil, "approval gate cannot be inside a map region",
		},
		{
			"route crossing between two regions",
			[]WorkflowStep{mapped("a", "m1"), mapped("b", "m2")},
			[]MapDef{
				{ID: "m1", Var: "d", Values: []string{"x"}},
				{ID: "m2", Var: "e", Values: []string{"y"}},
			},
			[]WorkflowRoute{{From: "a", To: "b"}},
			"cannot cross directly between map regions",
		},
		{
			"duplicate map id",
			[]WorkflowStep{mapped("build", "m1")},
			[]MapDef{{ID: "m1", Var: "d", Values: []string{"x"}}, {ID: "m1", Var: "e", Values: []string{"y"}}},
			nil, "duplicate id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if msg := validateMaps(tc.steps, tc.defs, tc.rts); !strings.Contains(msg, tc.want) {
				t.Fatalf("validateMaps = %q, want it to contain %q", msg, tc.want)
			}
		})
	}
	// The real pipeline stays valid.
	if msg := validateMaps(steps, defs, routes); msg != "" {
		t.Fatalf("valid pipeline rejected: %s", msg)
	}
}

func TestResolveMapValues(t *testing.T) {
	// values_from reads an earlier step's output, which is what makes the fan-out
	// dynamic — the whole point of a map over a discovered list.
	d := MapDef{ID: "m1", Var: "dir", ValuesFrom: "${steps.discover.output.DIRS}"}
	sc := substContext{outputs: map[string]string{"discover": `{"DIRS":"src/cli src/systems/forge"}`}}
	vals, err := resolveMapValues(d, sc)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(vals) != 2 || vals[0] != "src/cli" || vals[1] != "src/systems/forge" {
		t.Fatalf("values = %v", vals)
	}

	// A JSON array works too, as with a matrix.
	sc = substContext{outputs: map[string]string{"discover": `{"DIRS":"[\"a\",\"b\"]"}`}}
	if vals, _ := resolveMapValues(d, sc); len(vals) != 2 || vals[0] != "a" {
		t.Fatalf("json values = %v", vals)
	}

	// Over the cap is an error rather than a silently truncated fan-out.
	many := make([]string, maxMapValues+1)
	for i := range many {
		many[i] = "v"
	}
	if _, err := resolveMapValues(MapDef{ID: "m1", Var: "d", Values: many}, substContext{}); err == nil {
		t.Fatal("want an error over maxMapValues")
	}
}

// A region records its first node's WORKFLOW index, so a failure in its own
// orchestration (the clone/gather, which are not user-authored steps) still lands on
// a real step index in the run view rather than defaulting to 0.
func TestRegionsOf_FirstIndexIsWorkflowScoped(t *testing.T) {
	steps, defs, _ := mapPipeline()
	r := regionsOf(steps, defs)["m1"]
	if r.firstIndex != 1 {
		t.Fatalf("firstIndex = %d, want 1 (build's position in the workflow)", r.firstIndex)
	}
}

// Clone names are per-iteration and deterministic, which is what lets an iteration
// release its own clone the moment it finishes — so the concurrent volume footprint
// tracks max_concurrent rather than the value count.
func TestIterVolume_IsPerIterationAndDeterministic(t *testing.T) {
	if a, b := iterVolume("workspace", 0), iterVolume("workspace", 1); a == b {
		t.Fatal("iterations must not share a clone name")
	}
	if iterVolume("workspace", 3) != iterVolume("workspace", 3) {
		t.Fatal("clone name must be stable for a given iteration")
	}
}

func TestMapConcurrency(t *testing.T) {
	if got := mapConcurrency(MapDef{}); got != defaultFanoutConcurrency {
		t.Errorf("default = %d, want %d", got, defaultFanoutConcurrency)
	}
	if got := mapConcurrency(MapDef{Sequential: true, MaxConcurrent: 9}); got != 1 {
		t.Errorf("sequential = %d, want 1 (it must win over max_concurrent)", got)
	}
	if got := mapConcurrency(MapDef{MaxConcurrent: 4}); got != 4 {
		t.Errorf("explicit = %d, want 4", got)
	}
	// Never above the run-wide ceiling.
	if got := mapConcurrency(MapDef{MaxConcurrent: 999}); got != maxParallelSteps {
		t.Errorf("clamped = %d, want %d", got, maxParallelSteps)
	}
}

// ${map.<var>} is its own namespace: a mapped step must be able to read its
// iteration's binding without colliding with a matrix's.
func TestSubstitute_MapBinding(t *testing.T) {
	sc := substContext{mapVars: map[string]string{"dir": "src/cli"}, matrix: map[string]string{"os": "linux"}}
	if got := substitute("cd ${map.dir} && build ${matrix.os}", sc); got != "cd src/cli && build linux" {
		t.Fatalf("got %q", got)
	}
	// ${map.value} is the single-binding alias, as ${matrix.value} is.
	if got := substitute("${map.value}", substContext{mapVars: map[string]string{"dir": "x"}}); got != "x" {
		t.Fatalf("map.value = %q, want x", got)
	}
	// An unknown binding stays literal, matching substitution's tolerant contract.
	if got := substitute("${map.nope}", sc); got != "${map.nope}" {
		t.Fatalf("unknown binding = %q, want it left literal", got)
	}
}

// The clone volume replaces a body mount at the same path, so a body written against
// ${run_id}/workspace runs on ITS iteration's clone rather than the shared base.
func TestWithIterVolume_RedirectsCollidingMount(t *testing.T) {
	base := map[string]any{
		"run": "make build",
		"volumes": []any{
			map[string]any{"workflow_id": "run1", "name": "workspace", "mount_path": "/workspace"},
			map[string]any{"workflow_id": "run1", "name": "cache", "mount_path": "/cache"},
		},
	}
	clone := volMount("run1", "workspace-m0", "/workspace", true, false)
	out := withIterVolume(base, clone)

	vols, _ := out["volumes"].([]any)
	if len(vols) != 2 {
		t.Fatalf("volumes = %v, want the clone plus /cache", vols)
	}
	first, _ := vols[0].(map[string]any)
	if first["name"] != "workspace-m0" {
		t.Errorf("first volume = %v, want the iteration clone", first["name"])
	}
	// The colliding base mount is gone; the unrelated one survives.
	names := []string{}
	for _, v := range vols {
		m, _ := v.(map[string]any)
		names = append(names, m["name"].(string))
	}
	sort.Strings(names)
	if names[0] != "cache" || names[1] != "workspace-m0" {
		t.Fatalf("volumes = %v, want [cache workspace-m0]", names)
	}
	if out["run"] != "make build" {
		t.Error("the rest of the step config must be untouched")
	}
}

// A region with a volume drives forge's clone/copy actions, so its run role needs
// them — the reason workflowRolePermsVersion had to move.
func TestCollectWorkflowPermissions_MapVolumeGrants(t *testing.T) {
	withCatalog(t, map[string]ActionDef{
		"forge/run":           {Name: "forge/run", RequiredPermission: &PermissionSpec{Service: "forge", Action: "createExecution", Resource: "forge/executions"}},
		"forge/create-volume": {Name: "forge/create-volume", RequiredPermission: &PermissionSpec{Service: "forge", Action: "createVolume", Resource: "forge/volumes"}},
		"forge/volume-copy":   {Name: "forge/volume-copy", RequiredPermission: &PermissionSpec{Service: "forge", Action: "copyVolume", Resource: "forge/volumes"}},
	})
	steps := []WorkflowStep{mapped("build", "m1")}
	defs := []MapDef{{ID: "m1", Var: "d", Values: []string{"x"}, Volume: "workspace"}}
	perms := collectWorkflowPermissions(steps, defs, nil)
	var actions []string
	for _, p := range perms {
		actions = append(actions, p.Action)
	}
	sort.Strings(actions)
	// Without these the first clone 403s and the region hangs.
	joined := strings.Join(actions, ",")
	for _, want := range []string{"createVolume", "copyVolume"} {
		if !strings.Contains(joined, want) {
			t.Errorf("perms %v missing %s", actions, want)
		}
	}
}

// Teardown must reap a region's per-iteration clones, which are created under the
// run id without any create-volume step in the pipeline.
func TestWorkflowUsesVolumes_MapRegion(t *testing.T) {
	steps := []WorkflowStep{mapped("build", "m1")}
	if workflowUsesVolumes(steps, []MapDef{{ID: "m1", Volume: "workspace"}}) != true {
		t.Error("a map region with a volume must trigger teardown")
	}
	if workflowUsesVolumes(steps, []MapDef{{ID: "m1"}}) != false {
		t.Error("a map region with no volume creates none")
	}
}

func intPtr(i int) *int { return &i }
