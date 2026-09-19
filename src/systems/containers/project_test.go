package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
)

// ── selectLink (precedence) ───────────────────────────────────────────────────

func TestSelectLink_ExactBeatsNamespaceWide(t *testing.T) {
	links := []ProjectLink{
		{Namespace: "acme", Image: "", Project: "wide"},
		{Namespace: "acme", Image: "api", Project: "exact"},
	}
	got, ok := selectLink(links, "api")
	if !ok || got.Project != "exact" {
		t.Fatalf("got (%+v, %v), want the exact api link", got, ok)
	}
}

func TestSelectLink_FallsBackToNamespaceWide(t *testing.T) {
	links := []ProjectLink{{Namespace: "acme", Image: "", Project: "wide"}}
	got, ok := selectLink(links, "web")
	if !ok || got.Project != "wide" {
		t.Fatalf("got (%+v, %v), want the namespace-wide link", got, ok)
	}
}

func TestSelectLink_NoMatch(t *testing.T) {
	links := []ProjectLink{{Namespace: "acme", Image: "api", Project: "p"}}
	if _, ok := selectLink(links, "web"); ok {
		t.Fatal("expected no match for an unlinked image with no namespace-wide link")
	}
}

// ── filterCatalogByLinks ──────────────────────────────────────────────────────

func TestFilterCatalogByLinks(t *testing.T) {
	catalog := []string{"acme/api", "acme/web", "other/db", "acme/sub/img"}
	links := []ProjectLink{
		{Namespace: "acme", Image: "api"}, // exact
		{Namespace: "other", Image: ""},   // namespace-wide
	}
	got := filterCatalogByLinks(catalog, links)
	sort.Strings(got)
	want := []string{"acme/api", "other/db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFilterCatalogByLinks_NoLinksEmpty(t *testing.T) {
	got := filterCatalogByLinks([]string{"acme/api"}, nil)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty (no links means nothing in the project)", got)
	}
}

func TestFilterCatalogByLinks_NamespaceWideMatchesBare(t *testing.T) {
	// A repository name with no slash still carries a namespace (itself).
	got := filterCatalogByLinks([]string{"acme"}, []ProjectLink{{Namespace: "acme", Image: ""}})
	if len(got) != 1 || got[0] != "acme" {
		t.Fatalf("got %v, want [acme]", got)
	}
}

// ── authorizeRepo owner short-circuit ─────────────────────────────────────────

// A caller who owns the namespace is authorized without any project link — proving
// the fallback never weakens (or even consults the DB on) the existing check.
func TestAuthorizeRepo_OwnerShortCircuits(t *testing.T) {
	orgID := "org-" + t.Name()
	t.Cleanup(func() { orgNameCache.Delete(orgID) })
	// Gatekeeper reports the caller's org owns "bob"; no /check_permissions needed.
	fakeGatekeeperMulti(t, `{"authorized":true}`, "", "bob")

	r := httptest.NewRequest(http.MethodGet, "/repositories/bob/app/tags", nil)
	r.Header.Set("Authorization", "Bearer tok")
	if !authorizeRepo(t.Context(), r, "u1", orgID, "listTag", "bob", "app") {
		t.Fatal("owner of the namespace must be authorized without a project link")
	}
}

// ── checkProjectPermission resource shape ─────────────────────────────────────

// The project grant a member holds is owner-namespace-qualified as
// project/<slug>/containers/repositories/<ns>/<img>; verify the exact resource sent.
func TestCheckProjectPermission_ResourceShape(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authorized":true}`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = orig })

	if !checkProjectPermission(t.Context(), "Bearer tok", "listTag", "repositories", "my-proj", "acme/api") {
		t.Fatal("expected authorized")
	}
	if gotBody["service"] != "containers" || gotBody["action"] != "listTag" {
		t.Fatalf("service/action = %q/%q", gotBody["service"], gotBody["action"])
	}
	if want := "project/my-proj/containers/repositories/acme/api"; gotBody["resource"] != want {
		t.Fatalf("resource = %q, want %q", gotBody["resource"], want)
	}
}

func TestCheckProjectPermission_CollectionWideWildcard(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Write([]byte(`{"authorized":true}`))   //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = orig })

	checkProjectPermission(t.Context(), "Bearer tok", "createRepository", "repositories", "my-proj", "")
	if want := "project/my-proj/containers/repositories/*"; gotBody["resource"] != want {
		t.Fatalf("resource = %q, want %q", gotBody["resource"], want)
	}
}

func TestCheckProjectPermission_NoBearerFailsClosed(t *testing.T) {
	if checkProjectPermission(t.Context(), "", "listTag", "repositories", "p", "acme/api") {
		t.Fatal("no bearer must fail closed")
	}
}

// ── resolveProjectSlug ────────────────────────────────────────────────────────

func TestResolveProjectSlug_PrefersOwner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]accessibleProject{ //nolint:errcheck
			{ProjectID: "p1", Slug: "dup", Namespace: "alice", Tier: "viewer"},
			{ProjectID: "p2", Slug: "dup", Namespace: "bob", Tier: "owner"},
		})
	}))
	t.Cleanup(srv.Close)
	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = orig })

	p := resolveProjectSlug(t.Context(), "Bearer tok", "dup")
	if p == nil || p.ProjectID != "p2" {
		t.Fatalf("got %+v, want the owner-tier project p2", p)
	}
}

func TestResolveProjectSlug_Unresolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	orig := gatekeeperURL
	gatekeeperURL = srv.URL
	t.Cleanup(func() { gatekeeperURL = orig })

	if p := resolveProjectSlug(t.Context(), "Bearer tok", "nope"); p != nil {
		t.Fatalf("got %+v, want nil for an inaccessible slug", p)
	}
}
