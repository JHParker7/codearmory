package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// paramTypeFor classifies a path param by name: {id} is a UUID, and the slug-typed
// names (username/workspace/org/team — the blueprints state route; ns — owner-first
// per-record routes) get the slug regex. Any other name is unvalidated (e.g. forge's
// runner-class {name}), so conductor forwards it and the backend decides.
func TestParamTypeFor(t *testing.T) {
	for _, name := range []string{"ns", "username", "workspace", "org", "team"} {
		if got := paramTypeFor(name); got != "slug" {
			t.Errorf("paramTypeFor(%q) = %q, want slug", name, got)
		}
	}
	if got := paramTypeFor("id"); got != "uuid" {
		t.Errorf("paramTypeFor(id) = %q, want uuid", got)
	}
	// "namespace" must stay unvalidated: the container registry routes use it for a
	// docker namespace, which legitimately contains characters the slug pattern
	// rejects. Constraining it here would start 400ing pulls.
	for _, name := range []string{"name", "owner", "index", "namespace", ""} {
		if got := paramTypeFor(name); got != "" {
			t.Errorf("paramTypeFor(%q) = %q, want unvalidated", name, got)
		}
	}
}

// Owner-first per-record resources put the OWNER's namespace at the front of the
// resource string ("{ns}/tickets/tickets/{id}"), which is what makes gatekeeper
// evaluate the check against the record's owner instead of silently re-scoping it to
// whoever asked. That only works if conductor substitutes {ns} as readily as {id} —
// otherwise the literal "{ns}" reaches gatekeeper and matches no grant.
func TestResolveResourceOwnerFirst(t *testing.T) {
	got := resolveResource("{ns}/tickets/tickets/{id}",
		[]string{"ns", "id"}, []string{"alice", "abc-123"})
	if want := "alice/tickets/tickets/abc-123"; got != want {
		t.Errorf("resolveResource = %q, want %q", got, want)
	}

	// The legacy caller-scoped form has to keep resolving unchanged — both shapes are
	// served during the migration.
	got = resolveResource("tickets/tickets/{id}", []string{"id"}, []string{"abc-123"})
	if want := "tickets/tickets/abc-123"; got != want {
		t.Errorf("legacy resolveResource = %q, want %q", got, want)
	}
}

// validatePathParams rejects malformed slug/UUID segments with 400 before any forward,
// and normalises UUIDs. This is conductor's input-validation gate (previously covered
// at the integration level via the blueprints /state/{username}/{workspace} route).
func TestValidatePathParams(t *testing.T) {
	cases := []struct {
		desc   string
		names  []string
		values []string
		ok     bool
	}{
		{"valid slugs", []string{"username", "workspace"}, []string{"alice", "dev"}, true},
		{"slug with hyphen and underscore", []string{"username"}, []string{"my-user_123"}, true},
		{"slug with special char rejected", []string{"username"}, []string{"bad!user"}, false},
		{"second slug special char rejected", []string{"username", "workspace"}, []string{"alice", "bad!ws"}, false},
		{"slug too long rejected", []string{"workspace"}, []string{strings.Repeat("a", 65)}, false},
		{"non-slug param name is not validated", []string{"name"}, []string{"bad!name"}, true},
		{"valid hyphenated uuid", []string{"id"}, []string{"d28c1a9a-6c7a-4e35-9e95-cb63cc680b48"}, true},
		{"valid hex uuid normalised", []string{"id"}, []string{"d28c1a9a6c7a4e359e95cb63cc680b48"}, true},
		{"non-uuid id rejected", []string{"id"}, []string{"not-a-uuid"}, false},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		out, ok := validatePathParams(rec, c.names, c.values)
		if ok != c.ok {
			t.Errorf("%s: ok=%v, want %v (status %d)", c.desc, ok, c.ok, rec.Code)
			continue
		}
		if !c.ok && rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", c.desc, rec.Code)
		}
		if c.ok && c.desc == "valid hex uuid normalised" && out[0] != "d28c1a9a-6c7a-4e35-9e95-cb63cc680b48" {
			t.Errorf("hex uuid not normalised to hyphenated form: %q", out[0])
		}
	}
}
