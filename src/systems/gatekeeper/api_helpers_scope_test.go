package main

import "testing"

// scopeResource decides WHOSE resource a permission check is evaluated against. The
// first two cases are the pre-sharing world (owner == caller); the third is what makes
// a resource owned by someone else expressible at all.
func TestScopeResource_OwnerFirst(t *testing.T) {
	const svc = "codearmory_git_factory"
	tests := []struct {
		name              string
		resource          string
		username, orgName string
		service           string
		want              string
	}{
		// Unscoped: the service name leads, so the caller is assumed to be the owner.
		{"unscoped gets the caller", svc + "/repos/abc", "alice", "", svc, "alice/" + svc + "/repos/abc"},
		// Already the caller's own.
		{"caller-scoped is left alone", "alice/" + svc + "/repos/abc", "alice", "", svc, "alice/" + svc + "/repos/abc"},
		// Already the caller's org.
		{"org-scoped is left alone", "org/acme/" + svc + "/repos/abc", "alice", "acme", svc, "org/acme/" + svc + "/repos/abc"},

		// The sharing case: bob owns it, alice is asking. Must NOT become
		// "alice/bob/…", which no grant could ever match.
		{"another user's resource keeps its owner", "bob/" + svc + "/repos/abc", "alice", "", svc, "bob/" + svc + "/repos/abc"},
		{"another org's resource keeps its owner", "org/other/" + svc + "/repos/abc", "alice", "acme", svc, "org/other/" + svc + "/repos/abc"},

		// Backwards compatibility: a multi-segment resource that does NOT carry the
		// service name second is still just an unscoped resource (blueprints ships
		// "states/{username}/{workspace}") and must keep being caller-scoped.
		{"multi-segment unscoped still scoped", "states/alice/prod", "alice", "", "blueprints", "alice/states/alice/prod"},
		{"segment matching another service is not an owner", "hooks/rules", "alice", "", "blueprints", "alice/hooks/rules"},

		// Regression: a collection named after its own service. "tickets/tickets" is
		// the tickets service's own collection, NOT a resource owned by someone called
		// "tickets" — mistaking it for one denies every user their own tickets.
		{"collection sharing the service name is unscoped", "tickets/tickets", "alice", "", "tickets", "alice/tickets/tickets"},
		{"same, under an org", "org/acme/tickets/tickets", "alice", "acme", "tickets", "org/acme/tickets/tickets"},
		{"another owner's same-named collection still resolves", "bob/tickets/tickets", "alice", "", "tickets", "bob/tickets/tickets"},

		// No identity: nothing to scope with.
		{"empty username is unchanged", svc + "/repos/abc", "", "", svc, svc + "/repos/abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scopeResource(tt.resource, tt.username, tt.orgName, tt.service); got != tt.want {
				t.Errorf("scopeResource(%q, %q, %q, %q) = %q, want %q",
					tt.resource, tt.username, tt.orgName, tt.service, got, tt.want)
			}
		})
	}
}
