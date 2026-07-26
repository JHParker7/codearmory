package main

import (
	"encoding/json"
	"testing"
)

// sampleEvent is the running example from docs/events/design.md: a git_factory push to dev.
func sampleEvent() Event {
	return Event{
		ID:      "01J9Z",
		Type:    "repo.push",
		Source:  "git_factory",
		Subject: "jhparker7/codearmory_git_factory",
		Actor:   Actor{OrgID: "org1", UserID: "u1"},
		Data: map[string]any{
			"ref":    "dev",
			"commit": "9af3",
			"pusher": "jhparker7",
			"labels": []any{"ci", "backend"},
			"attempt": float64(3),
		},
	}
}

func match(t *testing.T, m Match, e Event) bool {
	t.Helper()
	ok, err := EvalEvent(m, e)
	if err != nil {
		t.Fatalf("EvalEvent error: %v", err)
	}
	return ok
}

// TestCanonicalTrigger is the exact filter from the design doc — it must match the dev push
// and reject a main push / a different repo. This is also the per-subject fix in action:
// two git_factory repos share Source but are told apart by Subject.
func TestCanonicalTrigger(t *testing.T) {
	m := Match{All: []Match{
		{Field: "type", Op: "eq", Value: "repo.push"},
		{Field: "subject", Op: "prefix", Value: "jhparker7/codearmory_git_factory"},
		{Field: "data.ref", Op: "glob", Value: "dev"},
	}}
	if !match(t, m, sampleEvent()) {
		t.Fatal("canonical trigger should match the dev push")
	}

	mainPush := sampleEvent()
	mainPush.Data["ref"] = "main"
	if match(t, m, mainPush) {
		t.Fatal("should not match a push to main")
	}

	otherRepo := sampleEvent()
	otherRepo.Subject = "jhparker7/codearmory"
	if match(t, m, otherRepo) {
		t.Fatal("should not match a different repo")
	}
}

func TestOperators(t *testing.T) {
	e := sampleEvent()
	cases := []struct {
		name string
		cond Match
		want bool
	}{
		{"eq true", Match{Field: "source", Op: "eq", Value: "git_factory"}, true},
		{"eq false", Match{Field: "source", Op: "eq", Value: "gitea"}, false},
		{"default op is eq", Match{Field: "data.ref", Value: "dev"}, true},
		{"ne", Match{Field: "data.ref", Op: "ne", Value: "main"}, true},
		{"in", Match{Field: "data.ref", Op: "in", Values: []any{"main", "dev", "release"}}, true},
		{"not_in", Match{Field: "data.ref", Op: "not_in", Values: []any{"main", "release"}}, true},
		{"exists true", Match{Field: "data.commit", Op: "exists"}, true},
		{"exists false-value on absent", Match{Field: "data.nope", Op: "exists", Value: false}, true},
		{"exists true on absent", Match{Field: "data.nope", Op: "exists", Value: true}, false},
		{"prefix", Match{Field: "subject", Op: "prefix", Value: "jhparker7/"}, true},
		{"suffix", Match{Field: "subject", Op: "suffix", Value: "_git_factory"}, true},
		{"glob star", Match{Field: "type", Op: "glob", Value: "repo.*"}, true},
		{"regex", Match{Field: "data.commit", Op: "regex", Value: "^[0-9a-f]+$"}, true},
		{"contains substring", Match{Field: "data.pusher", Op: "contains", Value: "park"}, true},
		{"contains array element", Match{Field: "data.labels", Op: "contains", Value: "backend"}, true},
		{"contains array miss", Match{Field: "data.labels", Op: "contains", Value: "frontend"}, false},
		{"numeric coercion eq", Match{Field: "data.attempt", Op: "eq", Value: float64(3)}, true},
		{"gte", Match{Field: "data.attempt", Op: "gte", Value: float64(3)}, true},
		{"gt false", Match{Field: "data.attempt", Op: "gt", Value: float64(3)}, false},
		{"lt", Match{Field: "data.attempt", Op: "lt", Value: float64(5)}, true},
		{"actor path", Match{Field: "actor.org_id", Op: "eq", Value: "org1"}, true},
		{"missing field fails closed", Match{Field: "data.absent", Op: "eq", Value: "x"}, false},
		{"missing field ne fails closed", Match{Field: "data.absent", Op: "ne", Value: "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := match(t, c.cond, e); got != c.want {
				t.Fatalf("%s: got %v want %v", c.name, got, c.want)
			}
		})
	}
}

func TestGroups(t *testing.T) {
	e := sampleEvent()

	// any: ref is dev OR main
	anyM := Match{Any: []Match{
		{Field: "data.ref", Op: "eq", Value: "main"},
		{Field: "data.ref", Op: "eq", Value: "dev"},
	}}
	if !match(t, anyM, e) {
		t.Fatal("any-group should match when one branch holds")
	}

	// all + any combined, with a nested not
	combined := Match{
		All: []Match{
			{Field: "type", Op: "eq", Value: "repo.push"},
			{Not: &Match{Field: "data.ref", Op: "eq", Value: "main"}}, // not main
		},
		Any: []Match{
			{Field: "source", Op: "eq", Value: "git_factory"},
			{Field: "source", Op: "eq", Value: "gitea"},
		},
	}
	if !match(t, combined, e) {
		t.Fatal("combined all+any+not should match the dev push")
	}

	// flip: a main push should fail the `not main`
	mainPush := e
	mainPush.Data = map[string]any{"ref": "main"}
	if match(t, combined, mainPush) {
		t.Fatal("combined should reject main push via the not clause")
	}
}

func TestEmptyFilterMatchesAll(t *testing.T) {
	if !match(t, Match{}, sampleEvent()) {
		t.Fatal("an empty filter should match every event")
	}
}

// TestFilterJSONRoundTrip ensures a filter authored as JSON (Terraform/portal/API) decodes
// into the same structure the engine evaluates.
func TestFilterJSONRoundTrip(t *testing.T) {
	raw := `{"all":[
		{"field":"type","op":"eq","value":"repo.push"},
		{"field":"data.ref","op":"glob","value":"dev"}
	]}`
	var m Match
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal filter: %v", err)
	}
	if !match(t, m, sampleEvent()) {
		t.Fatal("JSON-authored filter should match")
	}
	if len(m.All) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(m.All))
	}
}

func TestUnknownOperatorErrors(t *testing.T) {
	_, err := EvalEvent(Match{Field: "type", Op: "wat", Value: "x"}, sampleEvent())
	if err == nil {
		t.Fatal("unknown operator should error")
	}
}
