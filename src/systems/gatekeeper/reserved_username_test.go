package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestReservedUsername_RefusedOnCreate — each reserved name is a namespace PREFIX in
// the resource grammar, so a user holding one owns that namespace. "codearmory" is the
// sharp case: platform-owned resources are declared codearmory/<service>/…, so
// registering under that name would make every one of them evaluate inside that user's
// own space. That is privilege escalation, not a cosmetic clash.
func TestReservedUsername_RefusedOnCreate(t *testing.T) {
	for _, name := range []string{
		"codearmory", // the instance itself
		"org",        // org/<name>/<service>/…
		"project",    // project/<slug>/<service>/…
		// Case and surrounding space must not get around it. These would not collide in
		// the resource string (matching is exact) but they impersonate the owner, and
		// there is no legitimate reason to hold them.
		"CodeArmory", "CODEARMORY", "Org", "  codearmory  ",
	} {
		u := User{
			UserID:         uuid.New().String(),
			Email:          uuid.New().String() + "@test.com",
			Username:       name,
			HashedPassword: "x",
		}
		err := u.Add(context.Background())
		if err == nil {
			u.Remove(context.Background()) //nolint:errcheck
			t.Errorf("Add(username=%q) succeeded; a reserved namespace must not be claimable", name)
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("Add(username=%q) error = %v, want it to say the name is reserved", name, err)
		}
	}
}

// TestReservedUsername_RefusedOnRename covers the path that is easier to forget:
// signup is the one everyone guards, but a rename reaches the same column. Enforcing
// in Add/Update rather than per handler is what makes this hold for signup, invite
// acceptance, the OIDC identity path and the seeded bootstrap admin alike.
func TestReservedUsername_RefusedOnRename(t *testing.T) {
	u := createTestUser(t)

	u.Username = "codearmory"
	if err := u.Update(context.Background()); err == nil {
		t.Fatal("rename to a reserved name succeeded; Update must refuse it too")
	}

	// The row is untouched — a refused rename must not have written anything.
	got, err := (User{UserID: u.UserID}).Get(context.Background())
	if err != nil {
		t.Fatalf("re-read user: %v", err)
	}
	if got.(User).Username == "codearmory" {
		t.Error("username was persisted despite Update returning an error")
	}
}

// TestReservedUsername_OrdinaryNamesUnaffected — the guard must not be so broad that it
// catches names that merely contain a reserved word. Only the exact name is a namespace.
func TestReservedUsername_OrdinaryNamesUnaffected(t *testing.T) {
	for _, name := range []string{
		"codearmory-ci", "my-org", "org-admin", "projects", "codearmoryy", "alice",
	} {
		u := User{
			UserID:         uuid.New().String(),
			Email:          uuid.New().String() + "@test.com",
			Username:       name,
			HashedPassword: "x",
		}
		if err := u.Add(context.Background()); err != nil {
			t.Errorf("Add(username=%q) was refused, but only the exact reserved names are namespaces: %v", name, err)
			continue
		}
		u.Remove(context.Background()) //nolint:errcheck
	}
}

// TestPlatformNamespaceIsOwnerQualified pins the other half of the codearmory/ change.
//
// Platform-owned resources (runner classes, allowed images, OIDC config, the audit log)
// belong to the INSTANCE, not to any user. Declaring them as codearmory/<service>/…
// works only because scopeResource treats a resource whose SECOND segment is the
// service name as already owner-qualified, and so leaves it alone. If that ever
// changed, every platform resource would silently start evaluating inside the caller's
// own namespace again — which is the exact fiction this change removes, where a grant
// and a check matched only because BOTH got the caller prefixed.
func TestPlatformNamespaceIsOwnerQualified(t *testing.T) {
	for _, c := range []struct{ resource, service, want string }{
		// Platform-owned: left untouched, so the grant (a literal, no {username}) and
		// the check evaluate to the same string for every caller.
		{"codearmory/forge/runner-classes", "forge", "codearmory/forge/runner-classes"},
		{"codearmory/forge/images", "forge", "codearmory/forge/images"},
		{"codearmory/gatekeeper/oidc", "gatekeeper", "codearmory/gatekeeper/oidc"},
		{"codearmory/containers/registries", "containers", "codearmory/containers/registries"},

		// Controls — per-user collections MUST still be scoped to the caller, or every
		// user would share one namespace and tenant isolation would be gone.
		{"forge/executions", "forge", "alice/forge/executions"},
		{"tickets/boards", "tickets", "alice/tickets/boards"},

		// And another user's resource stays theirs, which is what makes sharing work.
		{"bob/forge/executions", "forge", "bob/forge/executions"},
	} {
		if got := scopeResource(c.resource, "alice", "", c.service); got != c.want {
			t.Errorf("scopeResource(%q, alice, service=%q) = %q, want %q", c.resource, c.service, got, c.want)
		}
	}
}

// TestPlatformNamespaceUserCannotClaimIt ties the two halves together: the namespace is
// only safe because no user can be called "codearmory". Without the reserved-name guard
// this whole scheme is a privilege-escalation route rather than an isolation boundary.
func TestPlatformNamespaceUserCannotClaimIt(t *testing.T) {
	if err := checkReservedUsername("codearmory"); err == nil {
		t.Fatal("a user may be named codearmory — they would own every platform resource")
	}
}
