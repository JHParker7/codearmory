package main

import "testing"

func hasPerm(ps []PermissionSpec, svc, action, resource string) bool {
	for _, p := range ps {
		if p.Service == svc && p.Action == action && p.Resource == resource {
			return true
		}
	}
	return false
}

// An http step has no catalog entry, so nothing can be derived from its action —
// without a declared grant the run role gets nothing and the call 403s. This was the
// exact shape the old git-factory-cd retarget step used (that pipeline is gone, but
// any hand-written http step has the same problem).
func TestCollectWorkflowPermissions_HTTPStepGrantsDeclared(t *testing.T) {
	steps := []WorkflowStep{{
		Step: Step{
			Name:   "retarget",
			Action: ActionHTTP,
			Permissions: []PermissionSpec{
				{Service: "builder", Action: "setOrgServiceImage", Resource: "codearmory/builder/orgs/default"},
			},
		},
	}}
	got := collectWorkflowPermissions(steps, nil, nil)
	if !hasPerm(got, "builder", "setOrgServiceImage", "codearmory/builder/orgs/default") {
		t.Errorf("declared grant missing from %v", got)
	}
}

// An http step with nothing declared must still contribute nothing — the skip is
// intact, so this change cannot silently widen an existing pipeline's role.
func TestCollectWorkflowPermissions_HTTPStepWithoutDeclarationGrantsNothing(t *testing.T) {
	steps := []WorkflowStep{{Step: Step{Name: "call", Action: ActionHTTP}}}
	if got := collectWorkflowPermissions(steps, nil, nil); len(got) != 0 {
		t.Errorf("got %v, want no permissions", got)
	}
}

// Path params in a declared resource are wildcarded like catalog-derived ones: a role
// carries the string verbatim and gatekeeper matches exact strings, so granting a
// literal "{id}" would 403 every real call.
func TestCollectWorkflowPermissions_DeclaredResourceWildcardsPathParams(t *testing.T) {
	steps := []WorkflowStep{{
		Step: Step{
			Name: "x", Action: ActionHTTP,
			Permissions: []PermissionSpec{{Service: "tickets", Action: "updateTicket", Resource: "tickets/tickets/{id}"}},
		},
	}}
	got := collectWorkflowPermissions(steps, nil, nil)
	if !hasPerm(got, "tickets", "updateTicket", "tickets/tickets/*") {
		t.Errorf("got %v, want the path param wildcarded", got)
	}
}

// An incomplete spec is ignored rather than emitted as a malformed grant.
func TestCollectWorkflowPermissions_IncompleteDeclarationIgnored(t *testing.T) {
	steps := []WorkflowStep{{
		Step: Step{
			Name: "x", Action: ActionHTTP,
			Permissions: []PermissionSpec{
				{Service: "builder", Action: "", Resource: "codearmory/builder/orgs/default"},
				{Service: "", Action: "a", Resource: "r"},
				{Service: "s", Action: "a", Resource: ""},
			},
		},
	}}
	if got := collectWorkflowPermissions(steps, nil, nil); len(got) != 0 {
		t.Errorf("got %v, want incomplete specs dropped", got)
	}
}

// Declaring the same grant on two steps must not emit it twice.
func TestCollectWorkflowPermissions_DeclaredGrantsDeduped(t *testing.T) {
	p := []PermissionSpec{{Service: "builder", Action: "setOrgServiceImage", Resource: "codearmory/builder/orgs/default"}}
	steps := []WorkflowStep{
		{Step: Step{Name: "a", Action: ActionHTTP, Permissions: p}},
		{Step: Step{Name: "b", Action: ActionHTTP, Permissions: p}},
	}
	if got := collectWorkflowPermissions(steps, nil, nil); len(got) != 1 {
		t.Errorf("got %v, want exactly one", got)
	}
}
