package main

// Executable spec for the owner-first per-record authorization contract.
//
// The property under test: authorization for a per-record operation depends on WHO OWNS
// THE RECORD, not merely on the caller having rights in their own namespace. Before
// per-record resources led with the owner, every caller's check on every id produced the
// identical string ("<service>/repos/{id}" scoped to the CALLER), so gatekeeper answered
// "authorized" for any id and the only thing standing between a user and someone else's
// repository was each handler remembering to filter its own query by owner. One missed
// filter was a silent cross-tenant read.
//
// These tests pin both halves of the replacement:
//   - a non-owner is DENIED (as 404, so existence does not leak) on every per-record
//     route, with no reliance on a query-level owner filter;
//   - an explicitly-shared collaborator is still ALLOWED, and only for the actions the
//     share carries — sharing must not be regressed by the boundary.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	gk "github.com/code-armory-app/codearmory_sdk/gatekeeper"
	"github.com/google/uuid"
)

// gatekeeperGrants models the two gatekeeper behaviours this boundary rests on, since a
// stub that merely says "yes" cannot tell a correct resource from the broken one:
//
//   - scopeResource: a resource that does not already name its owner gets the CALLER's
//     username prepended. This is exactly why a caller-scoped per-record resource
//     ("<service>/repos/{id}") authorizes EVERY id — it becomes the caller's own.
//   - matchPermission: the caller holds the manifest's default grants over their own
//     namespace ("{username}/<service>/repos" and "…/repos/*"), plus whatever explicit
//     shares name someone else's owner-first resource — which is what collab.go creates.
//
// Anything else is 403, which the service turns into a 404.
type gatekeeperGrants struct {
	userID   string
	username string
	shares   map[string][]string // resource → actions granted by an explicit share
}

// ownerQualifiedResource mirrors gatekeeper's ownerQualified: a resource names its owner
// when the SERVICE name follows the owner prefix. One that LEADS with the service name
// is unscoped and gets the caller's name instead.
func ownerQualifiedResource(resource string) bool {
	if strings.HasPrefix(resource, serviceName+"/") {
		return false
	}
	rest := strings.TrimPrefix(resource, "org/")
	_, after, found := strings.Cut(rest, "/")
	return found && (after == serviceName || strings.HasPrefix(after, serviceName+"/"))
}

func (g gatekeeperGrants) allows(action, resource string) bool {
	// An explicit share (a gatekeeper role) naming someone else's owner-first resource.
	if slices.Contains(g.shares[resource], action) {
		return true
	}
	scoped := resource
	if !ownerQualifiedResource(resource) {
		scoped = g.username + "/" + resource
	}
	own := g.username + "/" + resRepos
	return scoped == own || strings.HasPrefix(scoped, own+"/")
}

// newGatekeeperStubGrants points the three gatekeeper globals at a stub enforcing g.
func newGatekeeperStubGrants(t *testing.T, g gatekeeperGrants) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/check_permissions":
			var body struct{ Action, Resource string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if g.allows(body.Action, body.Resource) {
				writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "user_id": g.userID})
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		case r.URL.Path == "/projects/accessible":
			writeJSON(w, http.StatusOK, []any{})
		case strings.HasPrefix(r.URL.Path, "/users/"):
			writeJSON(w, http.StatusOK, map[string]any{
				"user_id": g.userID, "username": g.username, "active": true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	prevClient, prevURL, prevHTTP := gatekeeperClient, gatekeeperURL, httpClient
	gatekeeperClient = &gk.Client{URL: srv.URL, Service: serviceName, HTTPClient: srv.Client()}
	gatekeeperURL = srv.URL
	httpClient = srv.Client()
	t.Cleanup(func() {
		srv.Close()
		gatekeeperClient = prevClient
		gatekeeperURL = prevURL
		httpClient = prevHTTP
	})
}

// recordRoute is one per-record management route, invoked straight at its handler with
// the path values the mux would have set.
type recordRoute struct {
	name    string
	handler func(http.ResponseWriter, *http.Request)
	method  string
	suffix  string            // appended to /repos/{id} for the URL only
	body    string            // JSON body for the write methods
	extra   map[string]string // additional path values (sha, number, pattern, user)
}

// everyRecordRoute is the complete per-record surface from main.go's route table. It is
// exhaustive on purpose: the point of the owner-first resource is that no single handler
// has to remember anything, so a new handler that forgets is caught here.
func everyRecordRoute() []recordRoute {
	return []recordRoute{
		{name: "getRepo", handler: handleGetRepo, method: http.MethodGet},
		{name: "updateRepo", handler: handleUpdateRepo, method: http.MethodPatch, body: `{"description":"x"}`},
		{name: "deleteRepo", handler: handleDeleteRepo, method: http.MethodDelete},
		{name: "listCommits", handler: handleListCommits, method: http.MethodGet, suffix: "/commits"},
		{name: "commit", handler: handleCommit, method: http.MethodGet, suffix: "/commits/HEAD", extra: map[string]string{"sha": "HEAD"}},
		{name: "readme", handler: handleGetReadme, method: http.MethodGet, suffix: "/readme"},
		{name: "branches", handler: handleListBranches, method: http.MethodGet, suffix: "/branches"},
		{name: "tags", handler: handleListTags, method: http.MethodGet, suffix: "/tags"},
		{name: "setDefaultBranch", handler: handleSetDefaultBranch, method: http.MethodPut, suffix: "/default-branch", body: `{"default_branch":"main"}`},
		{name: "tree", handler: handleTree, method: http.MethodGet, suffix: "/tree"},
		{name: "blob", handler: handleBlob, method: http.MethodGet, suffix: "/blob?path=README.md"},
		{name: "writeBlob", handler: handleWriteBlob, method: http.MethodPut, suffix: "/blob", body: `{"path":"a.txt","content":"x"}`},
		{name: "archive", handler: handleArchive, method: http.MethodGet, suffix: "/archive"},
		{name: "listProtections", handler: handleListProtections, method: http.MethodGet, suffix: "/protections"},
		{name: "setProtection", handler: handleSetProtection, method: http.MethodPut, suffix: "/protections", body: `{"pattern":"main"}`},
		{name: "deleteProtection", handler: handleDeleteProtection, method: http.MethodDelete, suffix: "/protections/main", extra: map[string]string{"pattern": "main"}},
		{name: "listPulls", handler: handleListPulls, method: http.MethodGet, suffix: "/pulls"},
		{name: "createPull", handler: handleCreatePull, method: http.MethodPost, suffix: "/pulls", body: `{"title":"t","source_ref":"feature"}`},
		{name: "getPull", handler: handleGetPull, method: http.MethodGet, suffix: "/pulls/1", extra: map[string]string{"number": "1"}},
		{name: "mergePull", handler: handleMergePull, method: http.MethodPost, suffix: "/pulls/1/merge", body: `{}`, extra: map[string]string{"number": "1"}},
		{name: "closePull", handler: handleClosePull, method: http.MethodPost, suffix: "/pulls/1/close", body: `{}`, extra: map[string]string{"number": "1"}},
		{name: "listCollaborators", handler: handleListCollaborators, method: http.MethodGet, suffix: "/collaborators"},
		{name: "addCollaborator", handler: handleAddCollaborator, method: http.MethodPut, suffix: "/collaborators", body: `{"user":"user-mallory","level":"read"}`},
		{name: "removeCollaborator", handler: handleRemoveCollaborator, method: http.MethodDelete, suffix: "/collaborators/user-x", extra: map[string]string{"user": "user-x"}},
	}
}

// call invokes one record route against id as the caller holding bearer.
func (rt recordRoute) call(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if rt.body != "" {
		body = strings.NewReader(rt.body)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(rt.method, "/repos/"+id+rt.suffix, body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.SetPathValue("id", id)
	for k, v := range rt.extra {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	rt.handler(rec, req)
	return rec
}

// A caller who holds every right over their OWN namespace reaches nothing of someone
// else's — on any per-record route. This is the regression the ticket names: with a
// caller-scoped per-record resource these all returned "authorized" from gatekeeper and
// survived only on each handler's own owner filter.
func TestPerRecord_NonOwnerIsDeniedEveryRoute(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStubGrants(t, gatekeeperGrants{userID: "user-bob", username: "bob"})

	re := seedRepo(t, uuid.New().String(), "user-alice", "alice", "private-repo", "secret")

	for _, rt := range everyRecordRoute() {
		t.Run(rt.name, func(t *testing.T) {
			rec := rt.call(t, re.ID)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404 for a non-owner (body %q)",
					rt.method, "/repos/{id}"+rt.suffix, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "private-repo") || strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("denial body leaks the record: %q", rec.Body.String())
			}
		})
	}

	// The row itself must be untouched — a denial that still performed the write would
	// be worse than one that leaked a read.
	after, err := getRepoByID(t.Context(), re.ID)
	if err != nil {
		t.Fatalf("victim repo must survive: %v", err)
	}
	if after.Name != "private-repo" || after.Description != "secret" {
		t.Errorf("victim repo was modified by a denied caller: %+v", after)
	}
}

// The other half of owner-first: a collaborator the owner explicitly shared with IS
// allowed, and only for the actions the share carries. Nothing in the service knows this
// user — the grant lives in gatekeeper against the OWNER's resource, which is precisely
// what a caller-scoped resource could not express.
func TestPerRecord_SharedCollaboratorIsAllowed(t *testing.T) {
	setupTestDB(t)
	initMetrics()

	re := seedRepo(t, uuid.New().String(), "user-alice", "alice", "shared-repo", "")
	newGatekeeperStubGrants(t, gatekeeperGrants{
		userID:   "user-bob",
		username: "bob",
		// Exactly what collab.go's read-level share creates: the owner-first resource,
		// carrying the read action set.
		shares: map[string][]string{resRepoOf(re): shareActions["read"]},
	})

	// Granted by the share.
	for _, rt := range everyRecordRoute() {
		if !slices.Contains([]string{"getRepo", "listCommits", "branches", "tags"}, rt.name) {
			continue
		}
		t.Run("allowed/"+rt.name, func(t *testing.T) {
			if rec := rt.call(t, re.ID); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for a shared collaborator (body %q)", rec.Code, rec.Body.String())
			}
		})
	}

	// NOT granted by a read share: the share is attenuated, so it must not become
	// ownership. Re-sharing (shareRepo) stays with the owner at every level.
	for _, rt := range everyRecordRoute() {
		if !slices.Contains([]string{"deleteRepo", "listCollaborators", "addCollaborator", "setProtection", "writeBlob"}, rt.name) {
			continue
		}
		t.Run("denied/"+rt.name, func(t *testing.T) {
			if rec := rt.call(t, re.ID); rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 — a read share must not carry %s", rec.Code, rt.name)
			}
		})
	}
}

// The git wire honours the same share as the management API — a collaborator can clone,
// but a read share does not become push access.
func TestWire_SharedCollaboratorFetchesButCannotPush(t *testing.T) {
	setupTestDB(t)
	initMetrics()

	re := seedRepo(t, uuid.New().String(), "user-alice", "alice", "shared-repo", "")
	newGatekeeperStubGrants(t, gatekeeperGrants{
		userID:   "user-bob",
		username: "bob",
		shares:   map[string][]string{resRepoOf(re): shareActions["read"]},
	})

	if rec := infoRefs(t, "/alice/shared-repo.git", string(svcUploadPack), "Bearer tok"); rec.Code != http.StatusOK {
		t.Errorf("upload-pack advertisement = %d, want 200 for a shared collaborator (body %q)", rec.Code, rec.Body.String())
	}
	if rec := infoRefs(t, "/alice/shared-repo.git", string(svcReceivePack), "Bearer tok"); rec.Code != http.StatusNotFound {
		t.Errorf("receive-pack advertisement = %d, want 404 — a read share must not carry push", rec.Code)
	}
}

// An ORG repo's namespace is the org's name, and gatekeeper can template {username} but
// not the org name — so no default grant can ever name "acme/<service>/repos/*" and the
// per-record check DENIES the repo's own creator. The owner fallback (callerOwnsRepo) is
// what closes that, and it closes it for the owner ONLY: the same request from another
// user, who holds the identical rights in their own namespace, is still 404.
func TestPerRecord_OrgNamespaceRepoReachableByItsOwnerOnly(t *testing.T) {
	setupTestDB(t)
	initMetrics()

	re := seedRepo(t, uuid.New().String(), "user-alice", "acme", "org-repo", "")

	// The owner: username "alice", repo namespace "acme" — no grant can match.
	newGatekeeperStubGrants(t, gatekeeperGrants{userID: "user-alice", username: "alice"})
	if rec := (recordRoute{name: "getRepo", handler: handleGetRepo, method: http.MethodGet}).call(t, re.ID); rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want 200 — an org repo must be reachable by its owner (body %q)", rec.Code, rec.Body.String())
	}
	if rec := infoRefs(t, "/acme/org-repo.git", string(svcUploadPack), "Bearer tok"); rec.Code != http.StatusOK {
		t.Errorf("owner clone = %d, want 200 for an org repo", rec.Code)
	}

	// A stranger with the same rights in their own namespace: still nothing.
	newGatekeeperStubGrants(t, gatekeeperGrants{userID: "user-bob", username: "bob"})
	if rec := (recordRoute{name: "getRepo", handler: handleGetRepo, method: http.MethodGet}).call(t, re.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger status = %d, want 404 — the owner fallback must not authorize a non-owner", rec.Code)
	}
	if rec := infoRefs(t, "/acme/org-repo.git", string(svcUploadPack), "Bearer tok"); rec.Code != http.StatusNotFound {
		t.Errorf("stranger clone = %d, want 404", rec.Code)
	}
}

// callerOwnsRepo is the fallback's core, so its refusals are pinned directly: no
// credential, no recorded owner, and an authenticated caller who is simply not the owner
// must all fail closed.
func TestCallerOwnsRepo_FailsClosed(t *testing.T) {
	setupTestDB(t)
	initMetrics()
	newGatekeeperStubGrants(t, gatekeeperGrants{userID: "user-bob", username: "bob"})

	re := Repo{ID: "r1", Owner: "user-alice", Namespace: "alice", Name: "x"}
	for _, tc := range []struct {
		name   string
		bearer string
		repo   Repo
	}{
		{"no credential", "", re},
		{"unowned record", "tok", Repo{ID: "r1", Namespace: "alice"}},
		{"caller is not the owner", "tok", re},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if uid, ok := callerOwnsRepo(t.Context(), tc.bearer, "getRepo", tc.repo); ok {
				t.Fatalf("callerOwnsRepo authorized (%q), want refusal", uid)
			}
		})
	}
	if uid, ok := callerOwnsRepo(t.Context(), "tok", "getRepo", Repo{ID: "r1", Owner: "user-bob", Namespace: "acme"}); !ok || uid != "user-bob" {
		t.Fatalf("callerOwnsRepo(owner) = (%q, %v), want (user-bob, true)", uid, ok)
	}
}

// The manifest half of the convention. git_factory's per-record routes carry an opaque
// repo id and no namespace, so conductor — which can only template a resource from PATH
// parameters — cannot name the owner. Declaring "<service>/repos/{id}" there would be a
// per-record gate in appearance only: gatekeeper prefixes the CALLER, so the string is
// identical for every id and authorizes all of them. The manifest therefore declares the
// honest COLLECTION resource ("may this caller use the repo API at all") and the
// per-record decision belongs to the service, against the owner-first resource.
func TestManifests_DeclareNoCallerScopedPerRecordResource(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	for _, rel := range []string{
		filepath.Join("infra", "local", "registry-manifest.json"),
		filepath.Join("infra", "helm", "codearmory", "files", "registry-manifest.json"),
	} {
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var services []struct {
			Name      string `json:"name"`
			Endpoints []struct {
				Method   string `json:"method"`
				Path     string `json:"path"`
				Resource string `json:"resource"`
			} `json:"endpoints"`
			DefaultGrants []struct {
				GrantOn   string   `json:"grant_on"`
				Actions   []string `json:"actions"`
				Resources []string `json:"resources"`
			} `json:"default_grants"`
		}
		if err := json.Unmarshal(raw, &services); err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		var found bool
		for _, svc := range services {
			if svc.Name != serviceName {
				continue
			}
			found = true
			for _, ep := range svc.Endpoints {
				if strings.Contains(ep.Resource, "{") {
					t.Errorf("%s: %s %s declares the templated resource %q — conductor would evaluate it against the CALLER, authorizing every id",
						rel, ep.Method, ep.Path, ep.Resource)
				}
			}
			// The owner's own per-record grant is what actually authorizes them inside
			// the service, so it must survive; the collection grant is the gateway gate.
			var haveCollection, haveRecord bool
			for _, g := range svc.DefaultGrants {
				for _, r := range g.Resources {
					haveCollection = haveCollection || r == "{username}/"+serviceName+"/repos"
					haveRecord = haveRecord || r == "{username}/"+serviceName+"/repos/*"
				}
				if slices.Contains(g.Actions, "getRepo") && !slices.Contains(g.Actions, "listRepo") {
					t.Errorf("%s: the per-record actions must be granted alongside the collection ones, or the gateway gate denies them", rel)
				}
			}
			if !haveCollection || !haveRecord {
				t.Errorf("%s: default grants = collection:%v record:%v, want both", rel, haveCollection, haveRecord)
			}
		}
		if !found {
			t.Errorf("%s: no %s service entry", rel, serviceName)
		}
	}
}
