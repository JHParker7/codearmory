package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// smRouteSet renders routes as a comparable, order-independent set of "from->to@when".
func smRouteSet(routes []WorkflowRoute) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.From+"->"+r.To+"@"+r.When)
	}
	sort.Strings(out)
	return out
}

func stepNames(steps []WorkflowStepRef) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func mustSM(t *testing.T, doc string) *smDoc {
	t.Helper()
	var d smDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}
	return &d
}

func TestSMToModel_Sequence(t *testing.T) {
	doc := mustSM(t, `{"name":"seq","states":{
		"a":{"run":"one","next":"b"},
		"b":{"run":"two","next":"c"},
		"c":{"run":"three","end":true}}}`)
	steps, routes, maps, err := smToModel(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) != 0 {
		t.Fatalf("expected no maps, got %d", len(maps))
	}
	if got, want := stepNames(steps), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if got, want := smRouteSet(routes), []string{"a->b@", "b->c@"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	// The first state is inline: action set, no step_id.
	if steps[0].Action != "one" || steps[0].StepID != "" {
		t.Fatalf("state a not inline: %+v", steps[0])
	}
}

func TestSMToModel_ChoiceIsASeparateState(t *testing.T) {
	// The choice is its OWN state, not a field on the runner step; it lowers to the
	// conditional routes leaving the step that flows into it.
	doc := mustSM(t, `{"name":"c","states":{
		"tests":{"run":"test","next":"decide"},
		"decide":{"choice":[
			{"when":"steps.tests.status == \"failed\"","next":"mark_failed"},
			{"default":"done"}]},
		"mark_failed":{"run":"mark","end":true},
		"done":{"end":true,"run":"noop"}}}`)
	steps, routes, _, err := smToModel(doc)
	if err != nil {
		t.Fatal(err)
	}
	// "decide" is a choice — it is NOT a backend step.
	for _, s := range steps {
		if s.Name == "decide" {
			t.Fatalf("choice state leaked in as a step: %+v", s)
		}
	}
	want := []string{
		`tests->done@`,
		`tests->mark_failed@steps.tests.status == "failed"`,
	}
	if got := smRouteSet(routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}

func TestSMToModel_ChoiceOnRunStateRejected(t *testing.T) {
	doc := mustSM(t, `{"name":"c","states":{
		"tests":{"run":"test","choice":[{"default":"done"}]},
		"done":{"run":"n","end":true}}}`)
	if _, _, _, err := smToModel(doc); err == nil {
		t.Fatal("expected a choice-on-a-runner-step to be rejected")
	}
}

func TestSMToModel_ParallelForkJoin(t *testing.T) {
	doc := mustSM(t, `{"name":"p","states":{
		"build":{"run":"b","next":["lint","test"]},
		"lint":{"run":"l","next":"gate"},
		"test":{"run":"t","next":"gate"},
		"gate":{"run":"g","end":true}}}`)
	_, routes, _, err := smToModel(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build->lint@", "build->test@", "lint->gate@", "test->gate@"}
	if got := smRouteSet(routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}

func TestSMToModel_MapRegionBoundary(t *testing.T) {
	doc := mustSM(t, `{"name":"m","states":{
		"seed":{"run":"seed","next":"per_dir"},
		"per_dir":{"map":{"var":"dir","over":"${inputs.dirs}","volume":"workspace","states":{
			"unit":{"run":"test","next":"pkg"},
			"pkg":{"run":"build","end":true}}},"next":"report"},
		"report":{"run":"report","end":true}}}`)
	steps, routes, maps, err := smToModel(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) != 1 || maps[0].ID != "per_dir" || maps[0].Var != "dir" || maps[0].ValuesFrom != "${inputs.dirs}" {
		t.Fatalf("map def wrong: %+v", maps)
	}
	// unit/pkg carry the map id; the container name is not a node.
	byName := map[string]WorkflowStepRef{}
	for _, s := range steps {
		byName[s.Name] = s
	}
	if _, ok := byName["per_dir"]; ok {
		t.Fatal("map container should not be a node")
	}
	if byName["unit"].MapID != "per_dir" || byName["pkg"].MapID != "per_dir" {
		t.Fatalf("sub-states missing map id: %+v", byName)
	}
	// Boundary: seed -> region entry (unit); region exit (pkg) -> report.
	want := []string{"pkg->report@", "seed->unit@", "unit->pkg@"}
	if got := smRouteSet(routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}

// Round-trip: a model built by smToModel, rendered back to a document by modelToSM,
// and re-expanded must yield the same routes and step set.
func TestRoundTrip(t *testing.T) {
	doc := mustSM(t, `{"name":"rt","description":"d","states":{
		"build":{"run":"b","next":["lint","test"]},
		"lint":{"run":"l","next":"check"},
		"test":{"run":"t","next":"check"},
		"check":{"run":"c","next":"decide"},
		"decide":{"choice":[{"when":"x == 1","next":"ship"},{"default":"skip"}]},
		"ship":{"run":"s","end":true},
		"skip":{"run":"k","end":true}}}`)
	steps, routes, maps, err := smToModel(doc)
	if err != nil {
		t.Fatal(err)
	}
	// Enrich just enough for modelToSM (it reads Name/Action/StepID/With/MapID etc).
	wsteps := make([]WorkflowStep, len(steps))
	for i, s := range steps {
		wsteps[i] = WorkflowStep{Step: Step{Name: s.Name, Action: s.Action, StepID: s.StepID, With: s.With}, Matrix: s.Matrix, Scatter: s.Scatter, Approval: s.Approval, MapID: s.MapID}
	}
	back := modelToSM("rt", "d", wsteps, routes, maps, nil, nil)
	steps2, routes2, _, err := smToModel(back)
	if err != nil {
		t.Fatalf("re-expand: %v", err)
	}
	if got, want := smRouteSet(routes2), smRouteSet(routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip routes differ:\n got=%v\nwant=%v", got, want)
	}
	if got, want := stepNames(steps2), stepNames(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip steps differ: %v vs %v", got, want)
	}
}

func TestSMToModel_Errors(t *testing.T) {
	cases := map[string]string{
		"no states":      `{"name":"x","states":{}}`,
		"no kind":        `{"name":"x","states":{"a":{"next":"b"},"b":{"end":true}}}`,
		"unknown target": `{"name":"x","states":{"a":{"run":"r","next":"ghost"}}}`,
		"end and next":   `{"name":"x","states":{"a":{"run":"r","end":true,"next":"b"},"b":{"run":"r","end":true}}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := smToModel(mustSM(t, doc)); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}
